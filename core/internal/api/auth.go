package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/pool"
	"wild-work3/main/protocol/authflow"
	"wild-work3/main/protocol/jsonl"
)

// 认证（登录）转发：core 只做三件事——挑渠道、把 authflow 调用转给插件、把成功后的账号事实落库。
//
// 契约见 docs/AUTH_FLOW.md 与 PLUGIN_PROTOCOL §5.3：
//   - 登录流程与凭据完全归插件（凭据绝不经过 IPC，W3-07）
//   - core 不关心具体是哪种登录方式，只按 authflow.Type 的三个判据决定 UI 行为
//     （NeedsPoll / NeedsBrowser / CanPasteCallback），因此新增渠道不需要改 core
//   - flow_id 与账号无绑定：一个插件可同时跑多条流程、可管多个账号

// authFlow 记录 flow_id 归属（status 只有 flow_id，必须知道该问哪个插件）。
type authFlow struct {
	ChannelID string
	PluginID  string
	Region    string
	CreatedAt time.Time
	// 成功结果缓存：插件在成功后会消费掉 flow（再查得 expired），
	// 而前端可能有多个在途 status 请求，故 core 保留终态结果直接回放。
	DoneRaw json.RawMessage
}

var (
	authMu    sync.Mutex
	authFlows = map[string]authFlow{}
)

// rememberFlow 记下 flow 归属，并清理 30 分钟前的残留。
func rememberFlow(id, channelID, pluginID, region string) {
	if id == "" {
		return
	}
	authMu.Lock()
	defer authMu.Unlock()
	for k, f := range authFlows {
		if time.Since(f.CreatedAt) > 30*time.Minute {
			delete(authFlows, k)
		}
	}
	authFlows[id] = authFlow{ChannelID: channelID, PluginID: pluginID, Region: region, CreatedAt: time.Now()}
}

func lookupFlow(id string) (authFlow, bool) {
	authMu.Lock()
	defer authMu.Unlock()
	f, ok := authFlows[id]
	return f, ok
}

func forgetFlow(id string) {
	authMu.Lock()
	defer authMu.Unlock()
	delete(authFlows, id)
}

// authMethods GET /api/auth/methods?channel=<id>
// 返回该渠道声明的登录方式（前端据此决定显示「打开浏览器/输入 API Key/CDP」等）。
func (s *Server) authMethods(w http.ResponseWriter, r *http.Request) error {
	ch := s.Store.Channel(r.URL.Query().Get("channel"))
	if ch == nil {
		return errf("渠道不存在: %s", r.URL.Query().Get("channel"))
	}
	if !s.hasCap(ch.PluginID, "auth.methods") {
		// 没声明认证能力的渠道（如纯 API Key 渠道）返回空列表而非报错
		writeJSON(w, authflow.MethodsResult{Methods: []authflow.Method{}})
		return nil
	}
	res, err := s.RT.Call(r.Context(), ch.PluginID, "auth.methods", map[string]any{
		"config": ch.Config, "region": ch.Config["region"],
	})
	if err != nil {
		return errf("%v", err)
	}
	writeJSON(w, json.RawMessage(res.Result))
	return nil
}

// authBegin POST /api/auth/begin {channel_id, method?, region?, values?}
func (s *Server) authBegin(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		ChannelID string            `json:"channel_id"`
		Method    string            `json:"method"`
		Region    string            `json:"region"`
		Values    map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return err
	}
	ch := s.Store.Channel(body.ChannelID)
	if ch == nil {
		return errf("渠道不存在: %s", body.ChannelID)
	}
	if !s.hasCap(ch.PluginID, "auth.begin") {
		return errf("渠道 %s 不支持登录流程（可直接在渠道配置里填写密钥）", body.ChannelID)
	}
	if body.Region == "" {
		if v, ok := ch.Config["region"].(string); ok {
			body.Region = v
		}
	}
	res, err := s.RT.Call(r.Context(), ch.PluginID, "auth.begin", map[string]any{
		"method": orDefault(body.Method, "default"),
		"region": body.Region,
		"values": body.Values,
		"config": ch.Config,
	})
	if err != nil {
		Logf("渠道 %s 发起登录失败: %v", ch.ID, err)
		return errf("%v", err)
	}
	var br authflow.BeginResult
	_ = json.Unmarshal(res.Result, &br)
	rememberFlow(br.FlowID, ch.ID, ch.PluginID, orDefault(body.Region, br.Method))
	Logf("渠道 %s 发起登录（方式 %s，类型 %s）", ch.ID, br.Method, br.Type)
	writeJSON(w, json.RawMessage(res.Result))
	return nil
}

// authStatus POST /api/auth/status {flow_id}
//
// 登录成功后把插件上报的账号事实落进 core 配置与账号池（凭据仍在插件私有目录）。
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		FlowID string `json:"flow_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return err
	}
	if body.FlowID == "" {
		return errf("缺少 flow_id")
	}
	f, ok := lookupFlow(body.FlowID)
	if !ok {
		// 可能是 core 重启后遗留流程：在所有声明 auth.status 的渠道上探一遍
		for _, ch := range s.Store.Get().Channels {
			if !ch.Enabled || !s.hasCap(ch.PluginID, "auth.status") {
				continue
			}
			if res, err := s.RT.Call(r.Context(), ch.PluginID, "auth.status",
				map[string]any{"flow_id": body.FlowID, "config": ch.Config}); err == nil {
				var st authflow.StatusResult
				if json.Unmarshal(res.Result, &st) == nil && st.State != authflow.StateExpired {
					rememberFlow(body.FlowID, ch.ID, ch.PluginID, "")
					return s.finishStatus(w, ch, body.FlowID, res.Result)
				}
			}
		}
		return errf("登录流程不存在或已过期，请重新发起")
	}
	if len(f.DoneRaw) > 0 { // 终态已缓存：直接回放，不再打扰插件
		writeJSON(w, f.DoneRaw)
		return nil
	}
	ch := s.Store.Channel(f.ChannelID)
	if ch == nil {
		forgetFlow(body.FlowID)
		return errf("渠道已删除: %s", f.ChannelID)
	}
	res, err := s.RT.Call(r.Context(), f.PluginID, "auth.status",
		map[string]any{"flow_id": body.FlowID, "config": ch.Config})
	if err != nil {
		return errf("%v", err)
	}
	return s.finishStatus(w, ch, body.FlowID, res.Result)
}

// finishStatus 把插件返回的 StatusResult 透传给前端，并在成功时落账号。
func (s *Server) finishStatus(w http.ResponseWriter, ch *config.ChannelConfig, flowID string, raw json.RawMessage) error {
	var st authflow.StatusResult
	_ = json.Unmarshal(raw, &st)
	if st.State == authflow.StateSuccess && st.Account != nil {
		if err := s.upsertAccount(ch, st.Account); err != nil {
			return errf("账号保存失败: %v", err)
		}
		Logf("渠道 %s 登录成功：%s", ch.ID, orDefault(st.Account.Nickname, st.Account.UID))
	}
	// 终止态处理：成功时保留记录一小段时间，避免前端多个在途 poll 里后到的那个把
	// 已经成功的流程报成 expired（前端会立刻停轮询，但请求可能已发出）。
	// 失败/过期/取消则立即释放，避免占着渠道去反复询问插件。
	if st.State == authflow.StateSuccess {
		authMu.Lock()
		if f, ok := authFlows[flowID]; ok {
			f.DoneRaw = raw
			authFlows[flowID] = f
		}
		authMu.Unlock()
	} else if st.State.Terminal() {
		forgetFlow(flowID)
	}
	writeJSON(w, raw)
	return nil
}

// upsertAccount 把插件上报的账号事实写进 core 的账号表与账号池。
//
// 账号主键 = **凭据标识（auth_file）**，不是上游 uid（PLUGIN_CLI §5.2）：
// 同一自然人可能同时有国内版与国际版账号，上游 uid 相同；凭据标识带区前缀，
// 天然唯一，且它就是回传给插件的 `account_uid`（`--auth` 参数）。
// 插件未上报 auth_file 时退回 uid（兼容尚未升级的插件）。
func (s *Server) upsertAccount(ch *config.ChannelConfig, a *authflow.Account) error {
	remote := orDefault(a.AuthFile, a.UID)
	if remote == "" {
		return errf("插件未上报账号标识")
	}
	nick := orDefault(a.Nickname, remote)
	// 内部主键再掺渠道 ID：同一凭据标识理论上也可能被同插件的两个渠道实例引用。
	key := remote + "@" + ch.ID
	if err := s.Store.Update(func(c *config.Config) error {
		for _, x := range c.Accounts {
			if x.UID == key {
				x.Nickname, x.ChannelID, x.RemoteUID = nick, ch.ID, remote
				x.LoginKind, x.ExpiresAt = orDefault(a.LoginKind, "oauth"), a.ExpiresAt
				if a.Credits != nil {
					x.Credits, x.Unit = a.Credits, a.Unit
				}
				return nil
			}
		}
		c.Accounts = append(c.Accounts, &config.Account{
			UID: key, RemoteUID: remote, ChannelID: ch.ID, Nickname: nick,
			LoginKind: orDefault(a.LoginKind, "oauth"), ExpiresAt: a.ExpiresAt,
			Credits: a.Credits, Unit: a.Unit,
		})
		return nil
	}); err != nil {
		return err
	}
	// pool.Entry.ExpireAt 是「最早到期日」（YYYY-MM-DD，路由排序键），与账号 token 过期不同：
	// token 过期属插件私有凭据事实，core 只把额度到期日作为路由键。
	s.Pool.Upsert(pool.Entry{
		UID: key, ChannelID: ch.ID, Nickname: nick,
		Credits: a.Credits, Unit: a.Unit,
	})
	return nil
}

// accountsSync POST /api/accounts/sync：从各启用渠道插件拉取账号事实并落库。
//
// 动机：凭据的**事实来源是插件磁盘**（<core data>/plugins/<id>/auths/*.json，W3-39）。用户可能通过
// 旧版迁移、手工放置或客户端导入（旧版 import_local 语义）的方式已有凭据，而 core
// 的账号表只在登录流程（auth.status）时写入。该端点与启动时的自动同步补齐这一缺口。
func (s *Server) accountsSync(w http.ResponseWriter, r *http.Request) error {
	n := s.SyncAccounts(r.Context())
	writeJSON(w, map[string]any{"ok": true, "imported": n})
	return nil
}

// SyncAccounts 遍历启用渠道，调用插件 `accounts.list` 并把账号事实 upsert 进 core。
// 单个渠道失败不影响其他渠道（插件未安装/未声明能力/调用超时都跳过）。
//
// 按渠道实例并发（不同渠道是不同上游；同插件的 q-cn/q-com 也各自独立），
// upsert 串行做——Store.Update 是读改写，不能并发。
func (s *Server) SyncAccounts(ctx context.Context) int {
	type task struct {
		ch  *config.ChannelConfig
		res jsonl.Msg
		err error
	}
	tasks := make([]task, 0, len(s.Store.Get().Channels))
	for _, ch := range s.Store.Get().Channels {
		if !ch.Enabled {
			continue
		}
		p := s.RT.Plugin(ch.PluginID)
		if p == nil || !p.Met.Capabilities.HasControl("accounts.list") {
			continue
		}
		tasks = append(tasks, task{ch: ch})
	}
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t := &tasks[i]
			t.res, t.err = s.RT.CallGroup(ctx, t.ch.ID, t.ch.PluginID, "accounts.list",
				map[string]any{"config": t.ch.Config})
		}(i)
	}
	wg.Wait()

	n := 0
	for _, t := range tasks {
		if t.err != nil {
			continue
		}
		var out struct {
			Accounts []authflow.Account `json:"accounts"`
		}
		if json.Unmarshal(t.res.Result, &out) != nil {
			continue
		}
		for i := range out.Accounts {
			if s.upsertAccount(t.ch, &out.Accounts[i]) == nil {
				n++
			}
		}
	}
	return n
}

// authFinish POST /api/auth/finish {flow_id, callback_url, values}
// 人工粘贴兜底（AUTH_FLOW §2.2 R101）：只有 loopback_callback 范式需要，
// 但入口对所有插件统一开放，由插件自行决定是否接受。
func (s *Server) authFinish(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		FlowID      string            `json:"flow_id"`
		CallbackURL string            `json:"callback_url"`
		Values      map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return err
	}
	f, ok := lookupFlow(body.FlowID)
	if !ok {
		return errf("登录流程不存在或已过期，请重新发起")
	}
	ch := s.Store.Channel(f.ChannelID)
	if ch == nil {
		return errf("渠道已删除: %s", f.ChannelID)
	}
	if !s.hasCap(f.PluginID, "auth.finish") {
		return errf("渠道 %s 不支持手工提交回调", ch.ID)
	}
	res, err := s.RT.Call(r.Context(), f.PluginID, "auth.finish", map[string]any{
		"flow_id": body.FlowID, "callback_url": body.CallbackURL,
		"values": body.Values, "config": ch.Config,
	})
	if err != nil {
		return errf("%v", err)
	}
	return s.finishStatus(w, ch, body.FlowID, res.Result)
}

// authCancel POST /api/auth/cancel {flow_id}
func (s *Server) authCancel(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		FlowID string `json:"flow_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return err
	}
	f, ok := lookupFlow(body.FlowID)
	if !ok {
		writeJSON(w, map[string]any{"ok": true})
		return nil
	}
	if ch := s.Store.Channel(f.ChannelID); ch != nil {
		_, _ = s.RT.Call(r.Context(), f.PluginID, "auth.cancel",
			map[string]any{"flow_id": body.FlowID, "config": ch.Config})
	}
	forgetFlow(body.FlowID)
	Logf("登录流程已取消: %s", body.FlowID)
	writeJSON(w, map[string]any{"ok": true})
	return nil
}

// 编译期确保 context 被使用（Call 需要 ctx，这里保留导入以防后续重构丢参数）。
var _ = context.Background

// Package api 实现 core 管理面 HTTP API（前缀已由 gateway 剥离，路径形如 /api/state），
// 并在同一监听上提供 Web UI 静态资产（见 ui.go，W3-37 定案 1）。
//
// 对外视图结构见 views.go：形状以 web/app.js（既有前端资产）为权威契约（W3-17）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/pool"
	"wild-work3/main/core/internal/route"
	"wild-work3/main/core/internal/runtime"
	"wild-work3/main/core/internal/stat"
	"wild-work3/main/protocol/meta"
)

// Server core 管理面。
type Server struct {
	Store  *config.Store
	Pool   *pool.Pool
	RT     *runtime.Manager
	Stat   *stat.Book
	Router *route.Router
	Ver    map[string]string

	// fbMu/fbRing 降级记录环形缓冲（旧版 stats.Fallbacks，上限 200 条，最新在前）。
	// 纯内存：降级是当日运维事件，跨重启保留意义不大，且落盘会与 stat 记账口径重叠。
	fbMu   sync.Mutex
	fbRing []FallbackEvent
}

// FallbackEvent 一条降级记录（旧版 stats.FallbackEvent 同形状）。
type FallbackEvent struct {
	TS      string `json:"ts"`      // 发生时刻 HH:MM:SS
	From    string `json:"from"`    // 原模型
	To      string `json:"to"`      // 实际执行模型
	Channel string `json:"channel"` // 渠道实例 ID
}

// fbRingCap 降级记录保留条数（旧版同值：200）。
const fbRingCap = 200

// AddFallback 追加一条降级记录（chat.generate 结束时调用）。
func (s *Server) AddFallback(from, to, channel string) {
	s.fbMu.Lock()
	defer s.fbMu.Unlock()
	s.fbRing = append(s.fbRing, FallbackEvent{TS: time.Now().Format("15:04:05"), From: from, To: to, Channel: channel})
	if len(s.fbRing) > fbRingCap {
		s.fbRing = s.fbRing[len(s.fbRing)-fbRingCap:]
	}
}

// fallbacksView 降级记录（最新在前）。空时返回 []（前端隐藏区块，不能收 null）。
func (s *Server) fallbacksView() []FallbackEvent {
	s.fbMu.Lock()
	defer s.fbMu.Unlock()
	out := make([]FallbackEvent, 0, len(s.fbRing))
	for i := len(s.fbRing) - 1; i >= 0; i-- {
		out = append(out, s.fbRing[i])
	}
	return out
}

// Handler 返回 http.Handler（前缀已剥离，路径形如 /api/state）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.get(s.health))
	mux.HandleFunc("/api/state", s.get(s.state))
	mux.HandleFunc("/api/channels", s.route(s.channels))
	mux.HandleFunc("/api/channels/", s.route(s.channelOne))
	mux.HandleFunc("/api/accounts", s.get(s.accounts))
	mux.HandleFunc("/api/accounts/sync", s.post(s.accountsSync))
	mux.HandleFunc("/api/accounts/", s.route(s.accountOne))
	mux.HandleFunc("/api/usage", s.get(s.usage))
	mux.HandleFunc("/api/credits", s.get(s.credits))
	mux.HandleFunc("/api/stats", s.get(s.stats))
	mux.HandleFunc("/api/stats/channels", s.get(s.channelStats))
	mux.HandleFunc("/api/fees", s.get(s.fees))
	mux.HandleFunc("/api/logs", s.get(s.logs))
	mux.HandleFunc("/api/logs/requests", s.get(s.requestLogs))
	mux.HandleFunc("/api/config", s.route(s.configAll))
	mux.HandleFunc("/api/config/rules", s.route(s.rulesView))
	mux.HandleFunc("/api/plugins", s.get(s.plugins))
	// 登录授权（authflow 契约；前端按范式决定 UI，core 只转发不认渠道）
	mux.HandleFunc("/api/auth/methods", s.get(s.authMethods))
	mux.HandleFunc("/api/auth/begin", s.post(s.authBegin))
	mux.HandleFunc("/api/auth/status", s.post(s.authStatus))
	mux.HandleFunc("/api/auth/finish", s.post(s.authFinish))
	mux.HandleFunc("/api/auth/cancel", s.post(s.authCancel))
	mux.HandleFunc("/api/quit", s.post(s.quit))
	// 标准可选业务能力（W3-30，非流式一次性调用；网关转/*v1/systemone、/*v1/images/*）
	mux.HandleFunc("/api/systemone", s.post(s.systemoneDecide))
	mux.HandleFunc("/api/images/generate", s.post(s.imagesGenerate))
	// Web UI（W3-37 定案 1）：静态资产内嵌在 core，gateway 把未命中更具体前缀的路径透传到这里。
	mux.Handle("/", uiHandler())
	return mux
}

type hFn func(w http.ResponseWriter, r *http.Request) error

func (s *Server) get(f hFn) http.HandlerFunc  { return wrap(f, http.MethodGet) }
func (s *Server) post(f hFn) http.HandlerFunc { return wrap(f, http.MethodPost) }

// route 按方法分发到同一函数（函数内自查 r.Method）。
func (s *Server) route(f hFn) http.HandlerFunc { return wrap(f, "") }

func wrap(f hFn, method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if method != "" && r.Method != method {
			writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
			return
		}
		if err := f(w, r); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, map[string]any{"ok": true, "state": "ok", "version": s.Ver})
	return nil
}

// state 面板初始数据（对齐 app.js loadTopbar/loadAccounts 读取的字段）。
func (s *Server) state(w http.ResponseWriter, r *http.Request) error {
	cfg := s.Store.Get()
	host, port := splitListen(cfg.Listen)
	writeJSON(w, map[string]any{
		"ok":              true,
		"listen":          map[string]any{"host": host, "port": port},
		"api_key":         mask(cfg.APIKey),
		"autostart":       false,
		"running":         true,
		"version":         s.Ver,
		"default_channel": cfg.DefaultChannel,
		"channels":        s.channelsView(),
		"accounts":        s.accountsView(""),
		"plugins":         s.pluginViews(),
		// 调度配置（面板设置页回显；实际执行由 core 内置调度器负责）
		"checkin_times":   cfg.CheckinTimes,
		"keepalive_hours": cfg.KeepaliveHours,
	})
	return nil
}

// ---- 渠道 ----

func (s *Server) channels(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.channelsView()) // 裸数组
		return nil
	case http.MethodPost:
		var body struct {
			ChannelID   string         `json:"channel_id"`
			PluginID    string         `json:"plugin_id"`
			DisplayName string         `json:"display_name"`
			Config      map[string]any `json:"config"`
			Models      []string       `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if body.PluginID == "" {
			return errf("plugin_id 必填")
		}
		if s.RT.Plugin(body.PluginID) == nil {
			return errf("插件未安装: %s，请先到插件商店安装", body.PluginID)
		}
		id := body.ChannelID
		if id == "" {
			id = body.PluginID + "-" + shortID()
		}
		if s.Store.Channel(id) != nil {
			return errf("渠道 ID 已存在: %s", id)
		}
		if body.Models == nil && body.Config != nil {
			if ms, ok := body.Config["models"].([]any); ok {
				for _, m := range ms {
					if str, ok := m.(string); ok {
						body.Models = append(body.Models, str)
					}
				}
			}
		}
		name := orDefault(body.DisplayName, id)
		if err := s.Store.Update(func(c *config.Config) error {
			c.Channels = append(c.Channels, &config.ChannelConfig{
				ID: id, PluginID: body.PluginID, DisplayName: name,
				Enabled: true, Config: body.Config, Models: body.Models,
			})
			c.Accounts = append(c.Accounts, &config.Account{
				UID: id, ChannelID: id, Nickname: name, LoginKind: "api_key",
			})
			return nil
		}); err != nil {
			return err
		}
		s.Pool.Upsert(pool.Entry{UID: id, ChannelID: id, Nickname: name})
		Logf("新增渠道 %s（插件 %s）", id, body.PluginID)
		// 回显渠道视图（app.js 用 saved.display_name 提示登录向导）
		for _, v := range s.channelsView() {
			if v.ChannelID == id {
				writeJSON(w, v)
				return nil
			}
		}
		writeJSON(w, map[string]any{"ok": true, "channel_id": id})
		return nil
	}
	return errf("方法不支持")
}

// channelOne：GET 详情 / PUT 更新 / DELETE 删除 / POST 子操作。
func (s *Server) channelOne(w http.ResponseWriter, r *http.Request) error {
	rest := strings.TrimPrefix(r.URL.Path, "/api/channels/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	if id == "" {
		return errf("缺少渠道 ID")
	}
	ch := s.Store.Channel(id)
	if ch == nil {
		// 全局渠道操作（不绑定单个渠道）
		if id == "models_refresh_all" && len(parts) == 1 {
			return s.channelModelsRefreshAll(w, r)
		}
		return errf("渠道不存在: %s", id)
	}
	// 子操作
	if len(parts) > 1 {
		switch parts[1] {
		case "test":
			return s.channelTest(w, ch)
		case "toggle":
			en := !ch.Enabled
			if err := s.Store.Update(func(c *config.Config) error {
				for _, x := range c.Channels {
					if x.ID == id {
						x.Enabled = en
					}
				}
				return nil
			}); err != nil {
				return err
			}
			if !en {
				s.Pool.Cooldown(id, "", pool.CoolDisable)
			} else {
				s.Pool.ClearSessionDead(id)
			}
			writeJSON(w, map[string]any{"ok": true, "enabled": en})
			return nil
		case "models":
			// 有第三段 refresh → 重新拉取；否则返回当前模型 ID 列表（app.js 期望裸字符串数组）
			if len(parts) > 2 && parts[2] == "refresh" {
				return s.channelModelsRefresh(w, r, ch)
			}
			// 第四段 detail → 带思考档位的对象数组（W3-32；不动裸数组契约，新增只读视图）
			if len(parts) > 2 && parts[2] == "detail" {
				writeJSON(w, s.modelInfosOfChannel(r.Context(), ch))
				return nil
			}
			writeJSON(w, ch.Models)
			return nil
		}
		return errf("未知子操作: %s", parts[1])
	}
	switch r.Method {
	case http.MethodGet:
		v := map[string]any{
			"channel_id": ch.ID, "plugin_id": ch.PluginID, "display_name": ch.DisplayName,
			"enabled": ch.Enabled, "models": ch.Models, "config": maskConfig(ch.Config),
		}
		// 附插件 config.schema（渠道编辑表单由它动态生成，W3-06）
		if res, err := s.RT.CachedCall(r.Context(), ch.PluginID, "config.schema", nil); err == nil {
			v["config_schema"] = json.RawMessage(res.Result)
		}
		writeJSON(w, v)
		return nil
	case http.MethodPut:
		var body struct {
			DisplayName *string        `json:"display_name"`
			Enabled     *bool          `json:"enabled"`
			Config      map[string]any `json:"config"`
			Models      []string       `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if err := s.Store.Update(func(c *config.Config) error {
			for _, x := range c.Channels {
				if x.ID != id {
					continue
				}
				if body.DisplayName != nil {
					x.DisplayName = *body.DisplayName
				}
				if body.Enabled != nil {
					x.Enabled = *body.Enabled
				}
				if body.Config != nil {
					// 未修改的脱敏密钥回填旧值（前端拿不到明文）
					for k, v := range body.Config {
						if str, ok := v.(string); ok && strings.HasPrefix(str, "***") {
							if old, ok := x.Config[k].(string); ok {
								body.Config[k] = old
							}
						}
					}
					x.Config = body.Config
				}
				if body.Models != nil {
					x.Models = body.Models
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if body.Enabled != nil {
			if !*body.Enabled {
				s.Pool.Cooldown(id, "", pool.CoolDisable)
			} else {
				s.Pool.ClearSessionDead(id)
			}
		}
		Logf("更新渠道 %s", id)
		writeJSON(w, map[string]any{"ok": true})
		return nil
	case http.MethodDelete:
		if err := s.Store.Update(func(c *config.Config) error {
			for i, x := range c.Channels {
				if x.ID == id {
					c.Channels = append(c.Channels[:i], c.Channels[i+1:]...)
					break
				}
			}
			for i, a := range c.Accounts {
				if a.ChannelID == id {
					c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
					break
				}
			}
			return nil
		}); err != nil {
			return err
		}
		s.Pool.Remove(id)
		Logf("删除渠道 %s", id)
		writeJSON(w, map[string]any{"ok": true})
		return nil
	}
	return errf("方法不支持")
}

// channelTest 用真实密钥测试连接（调用插件 models.list；W3-15「添加渠道」机制）。
func (s *Server) channelTest(w http.ResponseWriter, ch *config.ChannelConfig) error {
	res, err := s.RT.Call(context.Background(), ch.PluginID, "models.list", map[string]any{"config": ch.Config})
	if err != nil {
		Logf("渠道 %s 测试连接失败: %v", ch.ID, err)
		return errf("测试失败: %v", err)
	}
	var payload struct {
		Models []meta.ModelInfo `json:"models"`
	}
	_ = json.Unmarshal(res.Result, &payload)
	// 顺手回填档位缓存（面板随后读 /core/fees 就不必再 spawn 一轮插件进程）
	cacheModelInfos(ch.PluginID, payload.Models)
	Logf("渠道 %s 测试连接成功，%d 个模型", ch.ID, len(payload.Models))
	writeJSON(w, map[string]any{"ok": true, "message": fmt.Sprintf("连接成功，发现 %d 个模型", len(payload.Models)), "models": payload.Models})
	return nil
}

// channelModelsRefreshAll 刷新全部启用渠道的模型列表。
//
// 后台按插件分组执行（CallByPlugin）：组内串行 + BatchGap，组间并发。
// 不同渠道连的是不同上游，跨渠道串行只会让总耗时线性叠加（W3-20 的约束是
// “别给同一个上游瞬间打一堆请求”，由 per-plugin 信号量与 gapBefore 保证）。
func (s *Server) channelModelsRefreshAll(w http.ResponseWriter, r *http.Request) error {
	s.RefreshAllModelsAsync()
	writeJSON(w, map[string]any{"ok": true, "message": "已开始刷新全部渠道模型（按渠道并行）"})
	return nil
}

// RefreshAllModelsAsync 后台刷新所有启用渠道的模型列表（跨渠道并行、同渠道串行），
// 返回作业完成时关闭的通道。
func (s *Server) RefreshAllModelsAsync() <-chan struct{} {
	done := make(chan struct{})
	chans := make([]config.ChannelConfig, 0)
	for _, ch := range s.Store.Get().Channels {
		if ch.Enabled && ch.PluginID != "" {
			chans = append(chans, *ch)
		}
	}
	jobs := make([]runtime.Job, 0, len(chans))
	for _, ch := range chans {
		jobs = append(jobs, runtime.Job{PluginID: ch.PluginID, Group: ch.ID, Method: "models.list",
			Params: map[string]any{"config": ch.Config}})
	}
	go func() {
		defer close(done)
		if len(jobs) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(jobs))*2*time.Minute)
		defer cancel()
		res, errs := s.RT.CallByPlugin(ctx, jobs)
		var changed []string
		for i, ch := range chans {
			if errs[i] != nil {
				Logf("刷新渠道 %s 模型失败: %v", ch.ID, errs[i])
				continue
			}
			var payload struct {
				Models []meta.ModelInfo `json:"models"`
			}
			_ = json.Unmarshal(res[i].Result, &payload)
			cacheModelInfos(ch.PluginID, payload.Models)
			ids := make([]string, 0, len(payload.Models))
			for _, m := range payload.Models {
				ids = append(ids, m.ID)
			}
			id := ch.ID
			_ = s.Store.Update(func(c *config.Config) error {
				for _, x := range c.Channels {
					if x.ID == id {
						x.Models = ids
					}
				}
				return nil
			})
			changed = append(changed, fmt.Sprintf("%s(%d)", id, len(ids)))
		}
		Logf("全部渠道模型刷新完成：%s", strings.Join(changed, ", "))
	}()
	return done
}

// accountRefreshAll 刷新全部账号（token 续期 + 额度回读），串行 + 间隔。
func (s *Server) accountRefreshAll(w http.ResponseWriter, r *http.Request) error {
	s.RefreshAllAccountsAsync()
	writeJSON(w, map[string]any{"ok": true, "message": "已开始刷新全部账号（后台串行执行）"})
	return nil
}

// RefreshAllAccountsAsync 后台串行刷新所有账号，返回作业完成时关闭的通道。
func (s *Server) RefreshAllAccountsAsync() <-chan struct{} {
	done := make(chan struct{})
	type target struct {
		ch     config.ChannelConfig
		remote string
	}
	var targets []target
	for _, ac := range s.Store.Get().Accounts {
		ch := s.Store.Channel(ac.ChannelID)
		if ch == nil || !ch.Enabled || !s.hasCap(ch.PluginID, "accounts.refresh") {
			continue
		}
		targets = append(targets, target{ch: *ch, remote: orDefault(ac.RemoteUID, ac.UID)})
	}
	jobs := make([]runtime.Job, 0, len(targets))
	for _, t := range targets {
		jobs = append(jobs, runtime.Job{PluginID: t.ch.PluginID, Group: t.ch.ID, Method: "accounts.refresh",
			Params: map[string]any{"uid": t.remote, "config": t.ch.Config}})
	}
	go func() {
		defer close(done)
		if len(jobs) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(jobs))*time.Minute)
		defer cancel()
		_, errs := s.RT.CallByPlugin(ctx, jobs)
		ok := 0
		for i := range errs {
			if errs[i] == nil {
				ok++
			} else {
				Logf("刷新账号 %s 失败: %v", targets[i].remote, errs[i])
			}
		}
		Logf("全部账号刷新完成：成功 %d，失败 %d", ok, len(errs)-ok)
	}()
	return done
}

func (s *Server) channelModelsRefresh(w http.ResponseWriter, r *http.Request, ch *config.ChannelConfig) error {
	res, err := s.RT.Call(r.Context(), ch.PluginID, "models.list", map[string]any{"config": ch.Config})
	if err != nil {
		return errf("拉取模型失败: %v", err)
	}
	var payload struct {
		Models []meta.ModelInfo `json:"models"`
	}
	_ = json.Unmarshal(res.Result, &payload)
	cacheModelInfos(ch.PluginID, payload.Models)
	ids := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		ids = append(ids, m.ID)
	}
	if err := s.Store.Update(func(c *config.Config) error {
		for _, x := range c.Channels {
			if x.ID == ch.ID {
				x.Models = ids
			}
		}
		return nil
	}); err != nil {
		return err
	}
	Logf("渠道 %s 模型已刷新：%d 个", ch.ID, len(ids))
	writeJSON(w, ids) // 裸字符串数组（app.js 直接 join）
	return nil
}

// ---- 账号 ----

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, s.accountsView(r.URL.Query().Get("channel"))) // 裸数组
	return nil
}

// accountOne：/{uid}/{op}。
func (s *Server) accountOne(w http.ResponseWriter, r *http.Request) error {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	uid := parts[0]
	if uid == "" {
		return errf("缺少账号 uid")
	}
	op := ""
	if len(parts) > 1 {
		op = parts[1]
	}
	// 全局操作（不绑定单个账号）
	if uid == "checkin_all" {
		return s.checkinAll(w, r)
	}
	if uid == "refresh_all" {
		return s.accountRefreshAll(w, r)
	}
	cfg := s.Store.Get()
	var acct *config.Account
	for _, a := range cfg.Accounts {
		if a.UID == uid {
			acct = a
			break
		}
	}
	if acct == nil {
		return errf("账号不存在: %s", uid)
	}
	// 插件 ID 需从账号所属渠道反查（Call 收的是插件 ID，不是渠道 ID）
	pluginID := acct.ChannelID
	var chConfig map[string]any
	if ch := s.Store.Channel(acct.ChannelID); ch != nil {
		pluginID, chConfig = ch.PluginID, ch.Config
	}
	// 回传插件控制面时必须用插件侧原始 uid（core 内部主键掺了渠道 ID）
	remoteUID := orDefault(acct.RemoteUID, uid)
	switch op {
	case "":
		writeJSON(w, map[string]any{"ok": true, "account": acct})
		return nil
	case "disable":
		// UI 的「停用/启用」按钮不传 body → 取反
		var body struct {
			Disabled *bool `json:"disabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		dis := !acct.Disabled
		if body.Disabled != nil {
			dis = *body.Disabled
		}
		if err := s.Store.Update(func(c *config.Config) error {
			for _, a := range c.Accounts {
				if a.UID == uid {
					a.Disabled = dis
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if dis {
			s.Pool.Cooldown(uid, "", pool.CoolDisable)
		} else {
			s.Pool.ClearSessionDead(uid)
		}
		Logf("账号 %s 已%s", uid, map[bool]string{true: "停用", false: "启用"}[dis])
		if v := s.accountView(uid); v != nil {
			writeJSON(w, v)
		} else {
			writeJSON(w, map[string]any{"ok": true})
		}
		return nil
	case "remove":
		if err := s.Store.Update(func(c *config.Config) error {
			for i, a := range c.Accounts {
				if a.UID == uid {
					c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
					break
				}
			}
			return nil
		}); err != nil {
			return err
		}
		s.Pool.Remove(uid)
		Logf("账号 %s 已移除", uid)
		writeJSON(w, map[string]any{"ok": true})
		return nil
	case "resource_detail":
		// 额度明细：插件上报的事实（ResourceItem 形态，W3-14）
		if !s.hasCap(pluginID, "accounts.resource_detail") {
			return errf("渠道 %s 不支持额度明细", pluginID)
		}
		res, err := s.RT.Call(r.Context(), pluginID, "accounts.resource_detail",
			map[string]any{"uid": remoteUID, "config": chConfig})
		if err != nil {
			return errf("%v", err)
		}
		// 顺带把额度事实回写账号池 + 差分记账（旧版 DiffCredits 语义）
		s.applyResourceFacts(uid, acct.ChannelID, res.Result)
		writeJSON(w, json.RawMessage(res.Result)) // 裸数组（app.js showDetail 直接 map）
		return nil
	case "nickname":
		// 显示名是 core 自己的字段（不是插件事实）：本地改本地存，不走 IPC。
		// 旧版 POST /api/account/nickname {uid,nickname}（dev/cmd/wild-work/web/app.js:3295）。
		var body struct {
			Nickname string `json:"nickname"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := strings.TrimSpace(body.Nickname)
		if name == "" {
			return errf("显示名不能为空")
		}
		if len([]rune(name)) > 60 {
			return errf("显示名过长（最多 60 字）")
		}
		if err := s.Store.Update(func(c *config.Config) error {
			for _, a := range c.Accounts {
				if a.UID == uid {
					a.Nickname = name
				}
			}
			return nil
		}); err != nil {
			return err
		}
		writeJSON(w, map[string]any{"ok": true, "account": s.accountView(uid)})
		return nil
	case "refresh", "checkin":
		// 转发到插件控制面（能力未声明则返回可读提示）
		if !s.hasCap(pluginID, "accounts."+op) {
			return errf("渠道 %s 不支持 %s", pluginID, op)
		}
		res, err := s.RT.Call(r.Context(), pluginID, "accounts."+op,
			map[string]any{"uid": remoteUID, "config": chConfig})
		if err != nil {
			Logf("账号 %s %s 失败: %v", uid, op, err)
			return errf("%v", err)
		}
		// 签到可能改变余额：顺手拉一次额度明细做差分记账（失败不影响签到结果）
		if op == "checkin" {
			s.refreshCreditsOne(r.Context(), acct.ChannelID, uid, remoteUID, chConfig)
		}
		Logf("账号 %s %s 成功", uid, op)
		writeJSON(w, map[string]any{"ok": true, "message": orDefault(pluginMessage(res.Result), "成功")})
		return nil
	}
	return errf("未知操作: %s", op)
}

// accountView 取单个账号视图（找不到返回 nil）。
func (s *Server) accountView(uid string) map[string]any {
	for _, a := range s.accountsView("") {
		if a.UID == uid {
			return map[string]any{"uid": a.UID, "nickname": a.Nickname, "channel": a.Channel,
				"credits": a.Credits, "unit": a.Unit, "disabled": a.Disabled,
				"cooling": a.Cooling, "until": a.Until, "stale": a.Stale}
		}
	}
	return nil
}

// checkinAll 对所有声明了 accounts.checkin 能力的渠道批量签到。
//
// 按插件分组（CallByPlugin）：同一渠道内多个账号串行 + 间隔（签到会写上游，不能并发压同一上游），
// 不同渠道是不同供应商，可以并行。
func (s *Server) checkinAll(w http.ResponseWriter, r *http.Request) error {
	var jobs []runtime.Job
	skipped := 0
	for _, ch := range s.Store.Get().Channels {
		pl := s.RT.Plugin(ch.PluginID)
		if pl == nil || !pl.Met.Capabilities.HasControl("accounts.checkin") {
			skipped++
			continue
		}
		jobs = append(jobs, runtime.Job{PluginID: ch.PluginID, Group: ch.ID, Method: "accounts.checkin",
			Params: map[string]any{"config": ch.Config}})
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(len(jobs)+1)*time.Minute)
	defer cancel()
	_, errs := s.RT.CallByPlugin(ctx, jobs)
	done, failed := 0, 0
	for i := range errs {
		if errs[i] == nil {
			done++
		} else {
			failed++
			Logf("渠道 %s 签到失败: %v", jobs[i].PluginID, errs[i])
		}
	}
	msg := fmt.Sprintf("签到完成：成功 %d，跳过 %d，失败 %d", done, skipped, failed)
	Logf("%s", msg)
	writeJSON(w, map[string]any{"ok": true, "message": msg})
	return nil
}

// hasCap 判定插件是否声明了某控制面能力（未安装插件返回 false）。
func (s *Server) hasCap(pluginID, cap string) bool {
	pl := s.RT.Plugin(pluginID)
	return pl != nil && pl.Met.Capabilities.HasControl(cap)
}

// pluginMessage 从插件返回里取 message 字段（没有则空串）。
func pluginMessage(raw json.RawMessage) string {
	var v struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.Message
}

// ---- 用量 / 积分 / 统计 / 费率 / 日志 ----

func (s *Server) usage(w http.ResponseWriter, r *http.Request) error {
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	agg := s.usageView(days)
	writeJSON(w, map[string]any{
		"tokens": agg.Tokens, "requests": agg.Requests,
		"spend": agg.Spend, "earn": agg.Earn, "cache_ratio": agg.CacheRate,
		"by_channel": agg.ByChannel, "by_model": agg.ByModel,
	})
	return nil
}

// creditEntry 一条积分流水的对外形状（前端积分流水子 tab 消费）。
type creditEntry struct {
	TS      string   `json:"ts"`
	UID     string   `json:"uid"`
	Channel string   `json:"channel"`
	Kind    string   `json:"kind"` // earn | spend | expire
	Amount  float64  `json:"amount"`
	After   *float64 `json:"after"`
	Note    string   `json:"note,omitempty"`
}

func (s *Server) credits(w http.ResponseWriter, r *http.Request) error {
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	recs := s.Stat.QueryCredits(days)
	out := make([]creditEntry, 0, len(recs)) // 空时必须是 []
	for _, c := range recs {
		amt := 0.0
		if c.Delta != nil {
			amt = *c.Delta
		}
		out = append(out, creditEntry{TS: c.TS, UID: c.AccountUID, Channel: c.ChannelID,
			Kind: c.Kind, Amount: amt, After: c.After, Note: c.Note})
	}
	// 新的在前（面板从上往下看最近发生的事）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, out)
	return nil
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, s.statsView())
	return nil
}

// channelStats 渠道插件自记统计的聚合视图（W3-22，/core/stats/channels?days=N）。
func (s *Server) channelStats(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, s.channelStatsView(atoiDefault(r.URL.Query().Get("days"), 7)))
	return nil
}

func (s *Server) fees(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, s.feesView(r.Context()))
	return nil
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, Logs(atoiDefault(r.URL.Query().Get("limit"), 200)))
	return nil
}

// ---- 配置 / 插件 / 退出 ----

func (s *Server) configAll(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.configView())
		return nil
	case http.MethodPut:
		// 局部更新：未出现的字段保持不变（*T == nil 表示「未提供」）；
		// default_channel 用 *string 以区分「未提供」与「显式清空」。
		var body struct {
			Listen          *string             `json:"listen"`
			Proxy           *string             `json:"proxy"`
			BodyLog         *bool               `json:"body_log"`
			CoolSecondsSoft *int                `json:"cool_seconds_soft"`
			CoolSecondsHard *int                `json:"cool_seconds_hard"`
			ModelFallback   map[string][]string `json:"model_fallback"`
			ModelRoutes     map[string]string   `json:"model_routes"`
			DefaultChannel  *string             `json:"default_channel"`
			CheckinTimes    []string            `json:"checkin_times"`
			KeepaliveHours  []int               `json:"keepalive_hours"`
			ReasonEfforts   map[string]string   `json:"reasoning_efforts"`
			Compat          *struct {
				MaxTokensCap *int              `json:"max_tokens_cap"`
				ModelMap     map[string]string `json:"model_map"`
			} `json:"compat"`
			LoopGuard *bool `json:"loop_guard"` // 便利开关：映射到 Hooks 里 loopguard 插件的 Enabled
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if err := s.Store.Update(func(c *config.Config) error {
			if body.Listen != nil && *body.Listen != "" {
				c.Listen = *body.Listen
			}
			if body.Proxy != nil {
				c.Proxy = *body.Proxy
			}
			if body.BodyLog != nil {
				c.BodyLog = *body.BodyLog
				// 进程级开关同步（旧版 app.SetBodyLog 同口径：面板一改立即生效，不需重启）
				defer stat.SetBodyTrace(*body.BodyLog)
			}
			if body.CoolSecondsSoft != nil && *body.CoolSecondsSoft > 0 {
				c.CoolSecondsSoft = *body.CoolSecondsSoft
			}
			if body.CoolSecondsHard != nil && *body.CoolSecondsHard > 0 {
				c.CoolSecondsHard = *body.CoolSecondsHard
			}
			if body.ModelFallback != nil {
				c.ModelFallback = body.ModelFallback
			}
			if body.ModelRoutes != nil {
				c.ModelRoutes = body.ModelRoutes
			}
			if body.DefaultChannel != nil {
				c.DefaultChannel = *body.DefaultChannel
			}
			// nil 切片/映射 = 未提供；显式 [] / {} 会解码成非 nil → 允许清空。
			if body.CheckinTimes != nil {
				c.CheckinTimes = body.CheckinTimes
			}
			if body.KeepaliveHours != nil {
				c.KeepaliveHours = body.KeepaliveHours
			}
			if body.ReasonEfforts != nil {
				c.ReasoningEfforts = body.ReasonEfforts
			}
			if body.Compat != nil {
				if body.Compat.MaxTokensCap != nil && *body.Compat.MaxTokensCap >= 0 {
					c.Compat.MaxTokensCap = *body.Compat.MaxTokensCap
				}
				if body.Compat.ModelMap != nil {
					c.Compat.ModelMap = body.Compat.ModelMap
				}
			}
			if body.LoopGuard != nil {
				setLoopGuard(c, *body.LoopGuard)
			}
			return nil
		}); err != nil {
			return err
		}
		// 冷却参数改完立即生效：池子只在启动时读一次，不然面板调完要重启 core。
		if s.Pool != nil {
			s.Pool.Configure(s.Store.Get().Cooling())
		}
		Logf("全局配置已更新")
		writeJSON(w, map[string]any{"ok": true})
		return nil
	}
	return errf("方法不支持")
}

// loopGuardPlugin 是官方拦截器插件的 PluginID（W3-25）。
const loopGuardPlugin = "loopguard"

// configView 在配置快照上补一个派生的 loop_guard 开关（前端只需要一个 bool，
// 不必理解 hooks 链）；其余字段与 Config 完全一致。
//
// 安全：/core/config 是只读展示面，前端并不从这里取凭据（listen/api_key 走 /gw/state，
// 渠道配置走 /core/channels 的 maskConfig），所以这里主动剥掉凭据类字段与整块
// channels/accounts，避免面板一次拉取就把模型 API Key、管理口令、所有渠道密钥的明文
// 写进浏览器内存与可能的代理日志里（零 token 落盘红线的延伸）。
func (s *Server) configView() map[string]any {
	c := s.Store.Get()
	b, _ := json.Marshal(c)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"api_key", "admin_password", "channels", "accounts"} {
		delete(m, k)
	}
	for _, h := range c.Hooks {
		if h != nil && h.PluginID == loopGuardPlugin {
			m["loop_guard"] = h.Enabled
			return m
		}
	}
	m["loop_guard"] = false
	return m
}

// setLoopGuard 切换 loopguard 拦截器实例的启用位；不存在且要开启时补一条默认实例。
func setLoopGuard(c *config.Config, on bool) {
	for _, h := range c.Hooks {
		if h != nil && h.PluginID == loopGuardPlugin {
			h.Enabled = on
			return
		}
	}
	if on {
		c.Hooks = append(c.Hooks, &config.HookConfig{ID: loopGuardPlugin, PluginID: loopGuardPlugin, Enabled: true, Order: 10})
	}
}

func (s *Server) plugins(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, s.pluginViews())
	return nil
}

func (s *Server) quit(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, map[string]any{"ok": true})
	go func() { time.Sleep(100 * time.Millisecond); quitFn() }()
	return nil
}

// quitFn 由 main 注入（优雅退出）。
var quitFn = func() {}

// SetQuit 注入退出回调。
func SetQuit(f func()) { quitFn = f }

// ---- 结构化请求日志环形（供 /api/logs/requests；旧版 stats 的「请求日志」区块）----
//
// 与 Logf 的文本日志分开：这里每条是一次完整对话的**结构化事实**（模型/渠道/账号/
// ok/errkind/token/耗时/首字），供面板表格渲染与降级排查；纯内存环形，重启即清。
type RequestLogEntry struct {
	TS         string `json:"ts"`
	Model      string `json:"model"`
	Channel    string `json:"channel"`
	Account    string `json:"account"`
	OK         bool   `json:"ok"`
	ErrKind    string `json:"err_kind,omitempty"`
	Prompt     int    `json:"prompt_tokens"`
	Completion int    `json:"completion_tokens"`
	Total      int    `json:"total_tokens"`
	TimeMs     int64  `json:"time_ms"`
	FirstMs    int64  `json:"first_token_ms"`
}

const reqLogCap = 200

var (
	reqLogMu  sync.Mutex
	reqLogBuf []RequestLogEntry
)

// AddRequestLog 记一条请求日志（chat.generate 结束时调用）。
func AddRequestLog(e RequestLogEntry) {
	if e.TS == "" {
		e.TS = time.Now().Format("15:04:05")
	}
	reqLogMu.Lock()
	reqLogBuf = append(reqLogBuf, e)
	if len(reqLogBuf) > reqLogCap {
		reqLogBuf = reqLogBuf[len(reqLogBuf)-reqLogCap:]
	}
	reqLogMu.Unlock()
}

// requestLogs 返回最近 n 条结构化请求日志（最新在前）。空时是 []，不能是 null。
func (s *Server) requestLogs(w http.ResponseWriter, r *http.Request) error {
	n := atoiDefault(r.URL.Query().Get("limit"), 50)
	reqLogMu.Lock()
	if n <= 0 || n > len(reqLogBuf) {
		n = len(reqLogBuf)
	}
	out := make([]RequestLogEntry, 0, n)
	for i := len(reqLogBuf) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, reqLogBuf[i])
	}
	reqLogMu.Unlock()
	writeJSON(w, out)
	return nil
}

// ---- 日志环形缓冲（供 /api/logs；主流程日志仍写 stderr）----

var (
	logMu  sync.Mutex
	logBuf []string
)

const logCap = 500

// Logf 记录一条管理面可见的日志（不写 stderr，避免与 main 的 stderr 重复）。
func Logf(format string, a ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, a...)
	logMu.Lock()
	logBuf = append(logBuf, line)
	if len(logBuf) > logCap {
		logBuf = logBuf[len(logBuf)-logCap:]
	}
	logMu.Unlock()
}

// Logs 返回最近 n 条日志（n<=0 取全部）。
func Logs(n int) []string {
	logMu.Lock()
	defer logMu.Unlock()
	if n <= 0 || n > len(logBuf) {
		n = len(logBuf)
	}
	out := make([]string, n)
	copy(out, logBuf[len(logBuf)-n:])
	return out
}

// ---- 小工具 ----

// maskConfig 脱敏：api_key 只留尾 4 位。
func maskConfig(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok && isSecret(k) {
			out[k] = mask(s)
			continue
		}
		out[k] = v
	}
	return out
}

func isSecret(k string) bool {
	lk := strings.ToLower(k)
	return strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "password")
}

func mask(s string) string {
	if len(s) <= 4 {
		return "***"
	}
	return "***" + s[len(s)-4:]
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// applyResourceFacts 把插件上报的额度明细回写到账号池并差分记账。
//
// 两个维度：
//   - credits：只累加 `usable && !info_only` 且单位不是 token 的条目（积分口径）；
//   - expireAt：取全部有效条目里最早的到期日（YYYY-MM-DD）作为路由排序键
//     （旧版 earliestExpire 语义：临期先用，避免额度白过期）。
//
// 同时调 stat.DiffCredits 把本次明细与上次快照对比，产出 earn/spend/expire 流水
// （旧版积分流水就是这样来的——插件不报流水，只报事实，core 差分）。
// 条目本身仍由插件上报并原样透传给前端，这里只算路由摘要与流水。
func (s *Server) applyResourceFacts(uid, channelID string, raw json.RawMessage) {
	var items []meta.ResourceItem
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return
	}
	var sum float64
	var known bool
	for _, it := range items {
		if it.InfoOnly || !it.Usable {
			continue
		}
		if it.Unit != "token" && it.Remain != nil {
			sum += *it.Remain
			known = true
		}
	}
	earliest := stat.EarliestExpireDay(items, time.Now())
	if known {
		s.Pool.SetCreditSummary(uid, sum, earliest) // 未上报可用额度时不臆造 0，保留原值
	}
	// 差分记账：余额未知（known=false）时 after 传 nil，流水里 after 字段缺省
	var after *float64
	if known {
		after = &sum
	}
	if n := s.Stat.DiffCredits(channelID, uid, items, after); n > 0 {
		Logf("积分差分 %s：%d 条流水", uid, n)
	}
}

func shortID() string {
	return time.Now().Format("150405")
}

func atoiDefault(s string, d int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return d
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return d
	}
	return n
}

type errStr string

func (e errStr) Error() string { return string(e) }

func errf(format string, a ...any) error {
	return errStr(fmt.Sprintf(format, a...))
}

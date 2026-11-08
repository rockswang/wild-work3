package api

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/pool"
	"wild-work3/main/core/internal/runtime"
	"wild-work3/main/protocol/meta"
)

// newProviderServer 造一个「一个内建渠道插件 + 一个 multi_channels 插件」的最小 core。
func newProviderServer(t *testing.T) *Server {
	t.Helper()
	st, err := config.OpenFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(t.TempDir(), t.TempDir())
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "workbuddy", Kind: "channel",
		DefaultChannels: []meta.DefaultChannel{{ID: "workbuddy", Name: "WorkBuddy 国内版"}}}})
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "custom-api", Kind: "channel", Name: "自定义模型", MultiChannels: true}})
	return &Server{Store: st, RT: rt, Pool: pool.New(time.Second, time.Second)}
}

// 渠道 ID 即模型命名空间前缀，非法/保留/撞名的 ID 必须在落盘前拒掉（W3-44）。
func TestCheckChannelID(t *testing.T) {
	s := newProviderServer(t)
	for _, id := range []string{"stepfun", "a", "A_9-x", "deepseek-cn"} {
		if err := s.checkChannelID(id); err != nil {
			t.Errorf("合法 ID %q 被拒: %v", id, err)
		}
	}
	for _, id := range []string{"", "-x", "_x", "x-", "x y", "智谱", "test", "detail", "models_refresh_all",
		strings.Repeat("a", 33), "workbuddy"} {
		if err := s.checkChannelID(id); err == nil {
			t.Errorf("ID %q 应被拒绝", id)
		}
	}
	// 已存在的实例（含自建）不得重复
	if err := s.Store.Update(func(c *config.Config) error {
		c.Channels = append(c.Channels, &config.ChannelConfig{ID: "zhipu", PluginID: "custom-api"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.checkChannelID("zhipu"); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("重复 ID 报错不符: %v", err)
	}
}

// POST /api/channels 建 Provider：ID 校验生效、config.models 提取为模型清单、
// 无 auth.begin 的插件补一条 api_key 账号（custom-api 靠它出现在账号页）。
func TestProviderCreateViaHTTP(t *testing.T) {
	s := newProviderServer(t)
	rec := httptest.NewRecorder()
	body := `{"channel_id":"stepfun","plugin_id":"custom-api","display_name":"阶跃星辰",` +
		`"config":{"base_url":"https://api.stepfun.com/v1","api_key":"sk-abc","models":["step-3.5-flash","step-2"]}}`
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/channels", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["channel_id"] != "stepfun" || view["display_name"] != "阶跃星辰" || view["models"].(float64) != 2 {
		t.Fatalf("回显不符: %+v", view)
	}
	got := s.Store.Get()
	if len(got.Channels) != 1 || got.Channels[0].PluginID != "custom-api" || len(got.Channels[0].Models) != 2 {
		t.Fatalf("落盘不符: %+v", got.Channels)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].UID != "stepfun" || got.Accounts[0].LoginKind != "api_key" {
		t.Fatalf("账号补齐不符: %+v", got.Accounts)
	}
	if len(s.Pool.List()) != 1 {
		t.Fatalf("池未登记: %+v", s.Pool.List())
	}
	// 撞内建前缀：整条请求失败，不得留下半成品渠道
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/channels",
		strings.NewReader(`{"channel_id":"workbuddy","plugin_id":"custom-api"}`)))
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "同名") {
		t.Fatalf("撞名应被拒: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.Store.Get().Channels) != 1 {
		t.Fatalf("被拒的请求写入了配置: %+v", s.Store.Get().Channels)
	}
}

// 删除 Provider 只带走渠道与其全部账号，凭据/统计目录不在安装树也不在配置里（W3-39/W3-44）。
func TestProviderDeleteRemovesAccountsOnly(t *testing.T) {
	s := newProviderServer(t)
	if err := s.Store.Update(func(c *config.Config) error {
		c.Channels = append(c.Channels, &config.ChannelConfig{ID: "stepfun", PluginID: "custom-api", Enabled: true})
		c.Accounts = append(c.Accounts,
			&config.Account{UID: "stepfun", ChannelID: "stepfun"},
			&config.Account{UID: "stepfun-2", ChannelID: "stepfun"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := s.Pool
	p.Upsert(pool.Entry{UID: "stepfun", ChannelID: "stepfun"})
	p.Upsert(pool.Entry{UID: "stepfun-2", ChannelID: "stepfun"})
	p.Upsert(pool.Entry{UID: "other", ChannelID: "workbuddy"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/channels/stepfun", nil))
	if rec.Code != 200 {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	got := s.Store.Get()
	if len(got.Channels) != 0 || len(got.Accounts) != 0 {
		t.Fatalf("删除残留: %+v %+v", got.Channels, got.Accounts)
	}
	if len(p.List()) != 1 || p.List()[0].ChannelID != "workbuddy" {
		t.Fatalf("误删其他渠道账号: %+v", p.List())
	}
}

// 未落盘预检：插件必须已安装；带 channel_id 时把 "***" 字段回填成存量明文，
// 否则编辑已有 Provider 时点「测试连接」会拿掩码串当真 key 打上游。
func TestChannelPretestRequiresPlugin(t *testing.T) {
	s := newProviderServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/channels/test",
		strings.NewReader(`{"plugin_id":"nope","config":{}}`)))
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "未安装") {
		t.Fatalf("未安装插件应可读报错: %d %s", rec.Code, rec.Body.String())
	}
}

func TestFillMaskedSecrets(t *testing.T) {
	old := map[string]any{"api_key": "sk-real", "base_url": "https://a/v1"}
	newCfg := map[string]any{"api_key": "***-real", "base_url": "https://b/v1", "timeout": float64(60)}
	got := fillMaskedSecrets(newCfg, old)
	if got["api_key"] != "sk-real" || got["base_url"] != "https://b/v1" || got["timeout"] != float64(60) {
		t.Fatalf("回填不符: %+v", got)
	}
	// 老配置没有该键时保留原值（不臆造空串）
	got = fillMaskedSecrets(map[string]any{"token": "***"}, old)
	if got["token"] != "***" {
		t.Fatalf("无存量值不应改动: %+v", got)
	}
}

// multi_channels 插件在 /core/plugins 上必须带标志，前端据此只出一个入口按钮。
func TestPluginsViewCarriesMultiChannels(t *testing.T) {
	s := newProviderServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/plugins", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/plugins: %d %s", rec.Code, rec.Body.String())
	}
	var plugs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &plugs); err != nil {
		t.Fatal(err)
	}
	m := map[string]map[string]any{}
	for _, p := range plugs {
		m[p["plugin_id"].(string)] = p
	}
	if got := m["custom-api"]["multi_channels"]; got != true {
		t.Fatalf("custom-api multi_channels=%v", got)
	}
	if _, ok := m["workbuddy"]["multi_channels"]; ok {
		t.Fatalf("workbuddy 不应出现 multi_channels 字段（omitempty）: %+v", m["workbuddy"])
	}
}

// 渠道改名要迁移账号/池/账本/规则（W3-45）。这里覆盖配置、账号、池三处；
// 账本迁移由 stat.RenameChannel 自己的单测覆盖（rename_test.go）。
func TestChannelRenameCascades(t *testing.T) {
	s := newProviderServer(t)
	// 写一条渠道级规则（Pass B 按渠道 ID 命中），改名后必须跟着走
	if err := s.Store.SetRules([]byte(`{"my.rule":{"kind":"regex","regex":"a","replacement":"b","scope":{"channels":["stepfun"]}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Update(func(c *config.Config) error {
		c.Channels = append(c.Channels, &config.ChannelConfig{ID: "stepfun", PluginID: "custom-api", DisplayName: "阶跃"})
		c.Accounts = append(c.Accounts, &config.Account{UID: "stepfun", ChannelID: "stepfun"})
		c.ModelRoutes = map[string]string{"stepfun/*": "stepfun"}
		c.DefaultChannel = "stepfun"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Pool.Upsert(pool.Entry{UID: "stepfun", ChannelID: "stepfun"})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/api/channels/stepfun",
		strings.NewReader(`{"channel_id":"stepfun2","display_name":"阶跃二"}`)))
	if rec.Code != 200 {
		t.Fatalf("PUT 改名: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"channel_id":"stepfun2"`) {
		t.Fatalf("回显未带新 ID: %s", rec.Body.String())
	}
	c := s.Store.Get()
	if len(c.Channels) != 1 || c.Channels[0].ID != "stepfun2" || c.Channels[0].DisplayName != "阶跃二" {
		t.Fatalf("渠道未改名: %+v", c.Channels)
	}
	if len(c.Accounts) != 1 || c.Accounts[0].ChannelID != "stepfun2" || c.Accounts[0].UID != "stepfun2" {
		t.Fatalf("账号未迁移: %+v", c.Accounts)
	}
	if c.ModelRoutes["stepfun/*"] != "stepfun2" || c.DefaultChannel != "stepfun2" {
		t.Fatalf("路由引用未迁移: %+v default=%s", c.ModelRoutes, c.DefaultChannel)
	}
	if l := s.Pool.List(); len(l) != 1 || l[0].ChannelID != "stepfun2" || l[0].UID != "stepfun2" {
		t.Fatalf("池未迁移: %+v", l)
	}
	rs, err := s.Store.Rules()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.ID == "my.rule" && (len(r.Scope.Channels) != 1 || r.Scope.Channels[0] != "stepfun2") {
			t.Fatalf("规则 scope 未迁移: %+v", r.Scope.Channels)
		}
	}
}

// 改名的 ID 校验：非法/保留/撞其它渠道拒绝，且**改回自己当前 ID 不算冲突**。
func TestChannelRenameValidates(t *testing.T) {
	s := newProviderServer(t)
	if err := s.Store.Update(func(c *config.Config) error {
		c.Channels = append(c.Channels,
			&config.ChannelConfig{ID: "stepfun", PluginID: "custom-api"},
			&config.ChannelConfig{ID: "moonshot", PluginID: "custom-api"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"moonshot", "workbuddy", "test", "secret", "x y"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/api/channels/stepfun",
			strings.NewReader(`{"channel_id":"`+bad+`"}`)))
		if rec.Code == 200 {
			t.Fatalf("非法新 ID %q 应被拒: %s", bad, rec.Body.String())
		}
	}
	// 原名回传（前端每次保存都带 channel_id）必须放行
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/api/channels/stepfun",
		strings.NewReader(`{"channel_id":"stepfun","display_name":"新名字"}`)))
	if rec.Code != 200 {
		t.Fatalf("同名保存应放行: %d %s", rec.Code, rec.Body.String())
	}
}

// 明文密钥：仅 GET、仅密钥类字段、只回该字段（W3-45 第 3 条明确要求可取得明文）。
func TestChannelSecretEndpoint(t *testing.T) {
	s := newProviderServer(t)
	if err := s.Store.Update(func(c *config.Config) error {
		c.Channels = append(c.Channels, &config.ChannelConfig{ID: "stepfun", PluginID: "custom-api",
			Config: map[string]any{"api_key": "sk-live-123", "base_url": "https://api/v1"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/channels/stepfun/secret?key=api_key", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"value":"sk-live-123"`) {
		t.Fatalf("应返回明文: %d %s", rec.Code, rec.Body.String())
	}
	// 非密钥字段不给（否则等于「导出全部配置」通道）
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/channels/stepfun/secret?key=base_url", nil))
	if rec.Code == 200 {
		t.Fatalf("非密钥字段应被拒: %s", rec.Body.String())
	}
	// 写方法不给
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/channels/stepfun/secret?key=api_key", nil))
	if rec.Code == 200 {
		t.Fatalf("非 GET 应被拒: %s", rec.Body.String())
	}
	// 缺参数
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/channels/stepfun/secret", nil))
	if rec.Code == 200 {
		t.Fatalf("缺 key 应被拒: %s", rec.Body.String())
	}
}

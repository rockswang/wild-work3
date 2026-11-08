// Package config 定义 core 的持久化配置（渠道/账号/全局开关）。
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wild-work3/main/appconfig"
	"wild-work3/main/rules"
	"wild-work3/main/rules/ruleset"
)

// ChannelConfig 一个渠道实例（= 一个插件的一份配置）。
type ChannelConfig struct {
	ID          string         `json:"id"`        // 实例 ID，如 "custom-1"
	PluginID    string         `json:"plugin_id"` // 插件 ID，如 "custom-api"
	DisplayName string         `json:"display_name"`
	Enabled     bool           `json:"enabled"`
	Config      map[string]any `json:"config"` // 插件专属配置（base_url/api_key/...）
	Models      []string       `json:"models,omitempty"`
	// DefaultID 是「该实例由插件 default_channels 哪个 ID 预置」的印记（改名后仍保留原值）。
	// 不存这个印记，改名后的内建渠道会在下次启动被 seedDefaultChannels 当成缺失而重建一份，
	// 结果是同一个上游凭空多出一个渠道实例（W3-45）。
	DefaultID string `json:"default_id,omitempty"`
	// 运行时字段（不持久化到配置文件，但便于前端读取，落盘时清空）
	PluginVersion string `json:"plugin_version,omitempty"`
}

// Account 账号（API Key 渠道：一个渠道配置即一个账号；OAuth 渠道由插件上报）。
type Account struct {
	// UID 是 core 内部账号主键（<插件侧 uid>@<渠道ID>）：同一自然人可能同时登录两个渠道实例，
	// 插件侧 uid 相同也必须分开调度/冷却，故内部主键掺渠道 ID。
	UID string `json:"uid"`
	// RemoteUID 是插件侧上报的原始 uid（回传插件控制面时必须用它）。
	RemoteUID string   `json:"remote_uid,omitempty"`
	ChannelID string   `json:"channel_id"`
	Nickname  string   `json:"nickname,omitempty"`
	Disabled  bool     `json:"disabled,omitempty"`
	LoginKind string   `json:"login_kind,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Credits   *float64 `json:"credits,omitempty"`
	Unit      string   `json:"unit,omitempty"`
}

// Compat 下游兼容层参数（旧版 compat 段）：模型名映射 + max_tokens 封顶。
type Compat struct {
	// MaxTokensCap 转发上游前对 max_tokens 封顶（0 = 不限制）。
	MaxTokensCap int `json:"max_tokens_cap,omitempty"`
	// ModelMap 客户端模型名 → "channel/model"；key 以 * 结尾时按前缀通配匹配。
	ModelMap map[string]string `json:"model_map,omitempty"`
}

// Config 全量配置。
type Config struct {
	Listen    string           `json:"listen"`         // 管理面 HTTP 监听（127.0.0.1:7864）
	APIKey    string           `json:"api_key"`        // 模型鉴权 key（/v1/*）
	AdminPass string           `json:"admin_password"` // 管理口令（由 gateway 校验；core 只存）
	Channels  []*ChannelConfig `json:"channels"`
	Accounts  []*Account       `json:"accounts"`
	// 模型名路由：model -> channel_id（精确或通配 *）
	ModelRoutes    map[string]string `json:"model_routes,omitempty"`
	DefaultChannel string            `json:"default_channel,omitempty"`
	// 降级链：model -> [model...]
	ModelFallback map[string][]string `json:"model_fallback,omitempty"`
	// 冷却/
	CoolSecondsSoft int `json:"cool_seconds_soft,omitempty"` // 默认 60
	CoolSecondsHard int `json:"cool_seconds_hard,omitempty"` // 默认 43200（12h，旧版 Cooldown.HardCredit="12h"）
	// ErrCooldownSeconds 同账号累计错误达 ErrThreshold 后的短冷却秒数（旧版 err_cooldown="10m"）。
	ErrCooldownSeconds int `json:"err_cooldown_seconds,omitempty"` // 默认 600
	// ErrThreshold 触发 ErrCooldownSeconds 的连续错误次数（旧版 err_threshold）。
	ErrThreshold int    `json:"err_threshold,omitempty"` // 默认 3
	Proxy        string `json:"proxy,omitempty"`
	BodyLog      bool   `json:"body_log,omitempty"`
	// DailyGrantAt 每日额度发放时刻（UTC+8 墙钟 "HH:MM"）。非空时硬冷却（402/余额不足）
	// 截到「下一个发奖窗口 + 缓冲」而不跑满 12h：额度一到账就能用，不让固定 12h 把刚
	// 补发的额度白锁几小时（旧版 R73，dev/internal/config/config.go HardCooldownFor）。
	// 留空 = 不启用（发奖时刻不固定的渠道应保持空，如 traework/raccoon）。
	DailyGrantAt string `json:"daily_grant_at,omitempty"`
	// DailyGrantBufferSec 发奖窗口缓冲秒数（默认 1800=30m，吸收上游到账拖连）。
	DailyGrantBufferSec int `json:"daily_grant_buffer_sec,omitempty"`
	// Hooks 拦截器链（W3-25）：按 Order 升序执行，请求正向、响应事件反向。
	Hooks []*HookConfig `json:"hooks,omitempty"`

	// 自动签到 / 保活时间（旧版 schedule 段，由 core 内置调度器执行）。
	CheckinTimes   []string `json:"checkin_times,omitempty"`   // ["09:00","21:00"]
	KeepaliveHours []int    `json:"keepalive_hours,omitempty"` // [22]
	// CheckinRetryUntil 签到重试窗口上界（当日分钟数，旧版 CheckinRetryUntil）：
	// 时段失败后在 (start, until] 内按间隔重试；0 = 关闭重试（只试一次）。键缺失默认 720（12:00）。
	CheckinRetryUntil int `json:"checkin_retry_until,omitempty"`
	// 思考强度注入：客户端 **未带** reasoning_effort 时按模型名注入（"*" 为全局默认）。
	ReasoningEfforts map[string]string `json:"reasoning_efforts,omitempty"`
	// 下游兼容层（模型名映射 / max_tokens 封顶）。
	Compat Compat `json:"compat,omitempty"`
}

// HookConfig 一个拦截器实例（= 一个 hook 类插件的一份配置）。
type HookConfig struct {
	ID       string         `json:"id"`        // 实例 ID，如 "loopguard-1"
	PluginID string         `json:"plugin_id"` // 插件 ID，如 "loopguard"
	Enabled  bool           `json:"enabled"`
	Order    int            `json:"order,omitempty"`  // 链上顺序（小→大；相同则按 ID 字典序）
	Config   map[string]any `json:"config,omitempty"` // 插件专属配置（阈值等）
	// FailOpen 插件崩溃/超时时的处置：true=跳过该 hook 继续（观测类），false=中断当前流
	// （请求/响应变换类默认 fail-closed，AGENTS §2-E）。
	FailOpen bool `json:"fail_open,omitempty"`
}

// Store 配置存储（带锁 + 原子落盘）。
//
// 配置不再单独落 core.json，而是写进全项目唯一配置文件 <data>/config.json 的 core 段
// （见 main/appconfig）：两个进程各写自己那一段，跨进程文件锁内读-改-写，避免丢更新。
type Store struct {
	mu   sync.RWMutex
	file *appconfig.File
	cfg  *Config
}

// Open 加载（或创建默认）配置。
//
// dataDir 是 core 的运行数据目录（<data>/core，stat/pool-state/logs 都在这），
// 统一配置文件在上层数据根（<data>/config.json）。
//
// 旧布局（gateway.json / core/core.json）的搬运已删除（W3-37 定案 4：ww3 未发布过任何版本，
// 除 auths/ 凭据格式外无向前兼容义务）。
func Open(dataDir string) (*Store, error) {
	root := filepath.Dir(filepath.Clean(dataDir))
	return openFile(appconfig.OpenData(root))
}

// OpenFile 从指定统一配置文件加载 core 段。
func OpenFile(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return openFile(appconfig.Open(path))
}

func openFile(f *appconfig.File) (*Store, error) {
	s := &Store{file: f}
	b, err := s.file.Section(appconfig.SectionCore)
	if err != nil {
		return nil, err // 坏 JSON fail closed（不静默用默认值）
	}
	var c Config
	if len(b) == 0 {
		s.cfg = defaults()
		return s, s.Save()
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s 的 core 段解析失败: %w", f.Path(), err)
	}
	normalize(&c, b)
	// 管理面只听回环（W3-37 定案 2）：对外唯一 origin 是 gateway，core 不得直接暴露到局域网。
	// 非回环 host 一律改写为 127.0.0.1（保留端口）而不是拒绝启动——配置写错不该让进程起不来。
	if fixed, changed := forceLoopback(c.Listen); changed {
		c.Listen = fixed
	}
	s.cfg = &c
	return s, nil
}

// normalize 补全新字段的缺省值：老配置文件没有这些键时不能留零值。
// 用原始 JSON 判定「键是否存在」，以区分「未配置」与「显式 0 / 空」。
func normalize(c *Config, raw []byte) {
	var probe struct {
		Compat *struct {
			MaxTokensCap *int `json:"max_tokens_cap"`
		} `json:"compat"`
		CoolSecondsSoft     *int `json:"cool_seconds_soft"`
		CoolSecondsHard     *int `json:"cool_seconds_hard"`
		ErrCooldownSeconds  *int `json:"err_cooldown_seconds"`
		ErrThreshold        *int `json:"err_threshold"`
		DailyGrantBufferSec *int `json:"daily_grant_buffer_sec"`
		CheckinRetryUntil   *int `json:"checkin_retry_until"`
	}
	_ = json.Unmarshal(raw, &probe)
	// 冷却类参数：键缺失或非法值（≤0）一律补默认——显式 0 没有合理语义（等于不冷却），
	// 让它退化为默认比「配置里没写就变成 0 秒」安全（旧版 load() 也是 parse>0 才覆盖）。
	if probe.CoolSecondsSoft == nil || *probe.CoolSecondsSoft <= 0 {
		c.CoolSecondsSoft = 60
	}
	if probe.CoolSecondsHard == nil || *probe.CoolSecondsHard <= 0 {
		c.CoolSecondsHard = 43200
	}
	if probe.ErrCooldownSeconds == nil || *probe.ErrCooldownSeconds <= 0 {
		c.ErrCooldownSeconds = 600
	}
	if probe.ErrThreshold == nil || *probe.ErrThreshold <= 0 {
		c.ErrThreshold = 3
	}
	if probe.DailyGrantBufferSec == nil || *probe.DailyGrantBufferSec < 0 {
		c.DailyGrantBufferSec = 1800
	}
	// 签到重试窗口：键缺失 → 默认 720（旧版 CheckinRetryUntil）；显式 0 = 关闭重试（合法语义）。
	if probe.CheckinRetryUntil == nil {
		c.CheckinRetryUntil = 720
	} else if *probe.CheckinRetryUntil < 0 {
		c.CheckinRetryUntil = 720
	}
	if c.CheckinTimes == nil {
		c.CheckinTimes = []string{"09:00", "21:00"}
	}
	if c.KeepaliveHours == nil {
		c.KeepaliveHours = []int{22}
	}
	// max_tokens_cap：键缺失 → 默认 32000；显式 0/负数 → 0（不限）。
	if probe.Compat == nil || probe.Compat.MaxTokensCap == nil {
		c.Compat.MaxTokensCap = 32000
	} else if *probe.Compat.MaxTokensCap < 0 {
		c.Compat.MaxTokensCap = 0
	}
	if c.Compat.ModelMap == nil {
		c.Compat.ModelMap = map[string]string{}
	}
	if c.ReasoningEfforts == nil {
		c.ReasoningEfforts = map[string]string{}
	}
	if c.ModelRoutes == nil {
		c.ModelRoutes = map[string]string{}
	}
	if c.ModelFallback == nil {
		c.ModelFallback = map[string][]string{}
	}
}

func defaults() *Config {
	return &Config{
		Listen:          "127.0.0.1:7864",
		ModelRoutes:     map[string]string{},
		ModelFallback:   map[string][]string{},
		CoolSecondsSoft: 60,
		// 硬冷却 12h：额度耗尽/账号级封禁当天不会自愈，1h 后重试只会再撞一次
		// （旧版 dev/internal/config/config.go:260 Cooldown.HardCredit = "12h"）。
		CoolSecondsHard: 43200,
		// 连续错误冷却 10 分钟（旧版 dev/internal/config/config.go:263 Cooldown.ErrCooldown="10m"）。
		ErrCooldownSeconds: 600,
		ErrThreshold:       3,
		// 每日发奖窗口默认关闭：发奖时刻是渠道事实，需由使用者/插件按渠道填入（R73）。
		DailyGrantBufferSec: 1800,
		// 旧版缺省：签到 09:00/21:00（失败后重试到 12:00），保活 22 点，max_tokens 封顶 32000（0=不限）。
		CheckinTimes:      []string{"09:00", "21:00"},
		CheckinRetryUntil: 720,
		KeepaliveHours:    []int{22},
		ReasoningEfforts:  map[string]string{},
		Compat:            Compat{MaxTokensCap: 32000, ModelMap: map[string]string{}},
	}
}

// Cooling 返回生效的冷却参数（统一走这里，避免 app/api 两处各自 *time.Second）。
func (c *Config) Cooling() (soft, hard, errCool time.Duration, errThreshold int) {
	return time.Duration(nz(c.CoolSecondsSoft, 60)) * time.Second,
		time.Duration(nz(c.CoolSecondsHard, 43200)) * time.Second,
		time.Duration(nz(c.ErrCooldownSeconds, 600)) * time.Second,
		nz(c.ErrThreshold, 3)
}

func nz(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// grantLoc 每日发奖窗口按 UTC+8 墙钟解释（上游发奖口径，与旧版 hardCooldownLoc 同）。
var grantLoc = time.FixedZone("UTC+8", 8*3600)

// HardCooldownFor 返回本次硬冷却时长：配了 DailyGrantAt 时截到「下一个发奖窗口 + 缓冲」，
// 否则等于 base。上限保护：截断只会缩短不会延长（旧版同口径）。
func (c *Config) HardCooldownFor(now time.Time, base time.Duration) time.Duration {
	if c == nil || c.DailyGrantAt == "" || base <= 0 {
		return base
	}
	var hh, mm int
	if _, err := fmt.Sscanf(c.DailyGrantAt, "%d:%d", &hh, &mm); err != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return base // 配错不致阻断主流程，退回完整硬冷却
	}
	buf := time.Duration(c.DailyGrantBufferSec) * time.Second
	if buf < 0 {
		buf = 0
	}
	local := now.In(grantLoc)
	until := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, grantLoc).Add(buf)
	if !until.After(now) {
		until = until.AddDate(0, 0, 1)
	}
	if d := until.Sub(now); d > 0 && d < base {
		return d
	}
	return base
}

// Get 返回配置（只读快照，调用方不得修改）。
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update 在锁内修改并落盘。
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(s.cfg); err != nil {
		return err
	}
	return s.saveLocked()
}

// Save 落盘。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	// PluginVersion 是运行时字段（随插件发现刷新），不落盘。
	for _, ch := range s.cfg.Channels {
		ch.PluginVersion = ""
	}
	b, err := json.Marshal(s.cfg)
	if err != nil {
		return err
	}
	return s.file.Update(appconfig.SectionCore, func(json.RawMessage) (json.RawMessage, error) {
		return b, nil
	})
}

// forceLoopback 把非回环 host 改写为 127.0.0.1（保留端口）。W3-37 定案 2：core 管理面永远只听回环。
//
// localhost/::1/[::1]/空 host（=全网卡，属对外暴露）都视为需要修正；空串回默认值。
func forceLoopback(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1:7864", true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return addr, false
	}
	if host == "localhost" {
		return "127.0.0.1:" + port, true
	}
	return "127.0.0.1:" + port, true
}

// Rules 生效规则（ruleset 默认表 + config.json rules 段按 ID 覆盖）。
//
// 默认表在本包侧的 main/rules/ruleset 链接（W3-37 定案 3）：core 可自动更新，
// 新增上游指纹只需发 core；gateway 不持有默认表，靠 GET /api/config/rules 的 effective 拉取。
func (s *Store) Rules() ([]rules.Rule, error) {
	over, err := s.file.RuleOverrides()
	if err != nil {
		return nil, err
	}
	return rules.Merge(ruleset.Defaults, over)
}

// RulesJSON 返回生效规则的 {"rules":{...}} 形态（供面板展示/编辑与 gateway 拉取）。
func (s *Store) RulesJSON() (json.RawMessage, error) {
	rs, err := s.Rules()
	if err != nil {
		return nil, err
	}
	return appconfig.WriteRules(rs)
}

// SetRules 整体替换用户覆盖段（W3-35）。
//
// 语义：body 的每个键是规则 ID；值给对象 = 覆盖该规则的同名字段，给 {} = 删除该默认规则，
// 新 ID = 追加。给 null/空对象 = 回到全默认。core 的 Store 不缓存规则，
// 故写完后由调用方（api.Server）重建 rules.Engine 并挂到 Router。
func (s *Store) SetRules(overrides json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Update(appconfig.SectionRules, func(json.RawMessage) (json.RawMessage, error) {
		if len(overrides) == 0 || string(overrides) == "null" {
			return nil, nil // 删段 = 全默认
		}
		var f struct {
			Rules map[string]json.RawMessage `json:"rules"`
		}
		if err := json.Unmarshal(overrides, &f); err != nil {
			return nil, fmt.Errorf("rules 段解析失败: %w", err)
		}
		// 两种形态都收：{"rules":{...}} 外壳，或裸 {"<id>":{...}}。
		// 注意不能靠「解析失败就回退」判断：struct 解析会**忽略未知键**，裸形态
		// 解析也会成功且 f.Rules 为 nil，于是裸形态的规则会被静默丢弃。
		if f.Rules == nil {
			if err := json.Unmarshal(overrides, &f.Rules); err != nil {
				return nil, fmt.Errorf("rules 段解析失败: %w", err)
			}
		}
		// 先合并+编译一遍，坏规则（ID 缺失、正则不合法、路径不支持）当场报错，不落盘。
		merged, err := rules.Merge(ruleset.Defaults, f.Rules)
		if err != nil {
			return nil, err
		}
		if _, err := rules.New(merged); err != nil {
			return nil, err
		}
		if len(f.Rules) == 0 {
			return nil, nil
		}
		// 磁盘上直接存 ID→规则对象的映射（与 appconfig.ruleOverrides 同口径），
		// 不套一层 "rules" 外壳——否则外壳会被当成一个规则 ID。
		return json.Marshal(f.Rules)
	})
}

// Channel 按 ID 找渠道。
func (s *Store) Channel(id string) *ChannelConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.cfg.Channels {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// RenameChannel 渠道实例改名：同步改写渠道 ID 与全部账号主键/归属（W3-45）。
//
// 只改内存配置并落盘；池与账本的迁移由调用方（api.Server）紧接着做——
// 它们属于运行期状态，不在本包的职责里。
//
// 账号主键形态两种：无凭据渠道的虚拟账号 UID 恰为渠道 ID；其余为 `<插件侧uid>@<渠道ID>`。
func (s *Store) RenameChannel(oldID, newID string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cfg.Channels {
		if c.ID == oldID {
			c.ID = newID
		}
	}
	for _, a := range s.cfg.Accounts {
		if a.ChannelID != oldID {
			continue
		}
		a.ChannelID = newID
		if a.UID == oldID {
			a.UID = newID // 虚拟账号：UID 即渠道 ID
			continue
		}
		if suffix := "@" + oldID; len(a.UID) > len(suffix) && a.UID[len(a.UID)-len(suffix):] == suffix {
			a.UID = a.UID[:len(a.UID)-len(suffix)] + "@" + newID
		}
	}
	// 模型路由/降级链/兼容映射里引用旧渠道 ID 的也要跟着走，否则改名后路由凭空断掉。
	for model, ch := range s.cfg.ModelRoutes {
		if ch == oldID {
			s.cfg.ModelRoutes[model] = newID
		}
	}
	if s.cfg.DefaultChannel == oldID {
		s.cfg.DefaultChannel = newID
	}
	return s.saveLocked()
}

// RenameRuleChannel 把用户规则覆盖里 scope.channels 的旧渠道 ID 换成新的（W3-45）。
//
// 渠道级规则（W3-35 的 Pass B）按渠道实例 ID 命中。改名后若不改规则，规则会静默失效
// ——用户看不到任何报错，只觉得「我配的脱敏/改写规则突然不管用了」。
// 默认规则集内嵌只读，故只处理用户覆盖段。
func (s *Store) RenameRuleChannel(oldID, newID string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Update(appconfig.SectionRules, func(cur json.RawMessage) (json.RawMessage, error) {
		if len(cur) == 0 {
			return nil, nil
		}
		var over map[string]json.RawMessage
		if err := json.Unmarshal(cur, &over); err != nil {
			return nil, err // 坏段不猜：宁可报错也不覆盖用户规则
		}
		changed := false
		for id, raw := range over {
			var obj map[string]any
			if json.Unmarshal(raw, &obj) != nil {
				continue
			}
			sc, ok := obj["scope"].(map[string]any)
			if !ok {
				continue
			}
			list, ok := sc["channels"].([]any)
			if !ok {
				continue
			}
			hit := false
			for i, v := range list {
				if s, ok := v.(string); ok && s == oldID {
					list[i], hit = newID, true
				}
			}
			if !hit {
				continue
			}
			sc["channels"] = list
			if b, err := json.Marshal(obj); err == nil {
				over[id], changed = b, true
			}
		}
		if !changed {
			return cur, nil
		}
		return json.Marshal(over)
	})
}

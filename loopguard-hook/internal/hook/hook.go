// Package hook 拦截器插件的协议实现（W3-25 契约）。
//
// 拦截器插件的形态（与渠道插件不同）：
//   - 一个进程服务**一次对话请求**：core 收到 chat.generate 后按配置顺序拉起链上
//     每个 hook（一次一进程），把上游渠道的归一化事件用 `push` 帧喂进来，hook 把
//     处理后的事件用 `event` 帧吐出去，处理完写一帧 result 退出。
//   - 双向流方法名固定 `hook.stream`；帧方向用 op 区分（push=上游→hook，
//     event=hook→下游），见 main/protocol/jsonl 的 OpPush。
//
// 本插件（loopguard）只做一件事：盯 reasoning.delta（可选 text.delta），命中
// 思考链死循环时**返回错误中止整段生成**，错误事件由 core 统一发（终止事件只出现
// 一次，见 W3-10/R36）。
package hook

import (
	"context"
	"encoding/json"
	"fmt"

	"wild-work3/main/loopguard-hook/internal/guard"
	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/jsonl"
	"wild-work3/main/protocol/meta"
	"wild-work3/main/protocol/plugin"
	"wild-work3/main/protocol/rpc"
)

// PluginID 插件标识（插件商店/gateway 用它定位）。
const PluginID = "loopguard"

// Manifest 插件元数据。
//
// Kind = "hook"：gateway 的插件商店据此归类，core 据此决定把它当拦截器而不是渠道。
// Execution 里 control/conversation 都是 call：hook 也是**一请求一进程**（与渠道一致，
// W3-20/25）；stdio 形态保留给调试与将来的常驻复用。
func Manifest() meta.Manifest {
	return meta.Manifest{
		ID:      PluginID,
		Name:    "思考链死循环守卫",
		Version: "1.0.0",
		Kind:    "hook",
		Protocol: meta.ProtocolRng{
			Min: 1,
			Max: 1,
		},
		Execution: meta.Execution{
			Control:      plugin.ModeCall,
			Conversation: plugin.ModeCall,
			MaxWorkers:   8,
		},
		Capabilities: meta.Capabilities{
			Control:      []string{"config.schema", "config.get", "ping", "health.check"},
			Conversation: []string{"hook.stream"},
		},
		Icon:        "hook",
		Color:       "#faad14",
		Description: "检测思考链死循环（机械复读/空转），命中即中断生成并向下游报错。",
	}
}

// Config 是 core 通过 hook.stream 的 params.config 下发的插件配置。
type Config struct {
	// Enabled 关掉后本 hook 变成纯透传（方便对比排查）。
	Enabled bool `json:"enabled"`
	// Watch 监听的增量通道：reasoning（默认）/ text / both。
	Watch string `json:"watch,omitempty"`
	// 以下阈值留 0 表示用 guard.DefaultConfig。
	StreakLimit          int     `json:"streak_limit,omitempty"`
	MinLineChars         int     `json:"min_line_chars,omitempty"`
	WindowLines          int     `json:"window_lines,omitempty"`
	WindowMinLines       int     `json:"window_min_lines,omitempty"`
	UniqueRatioMin       float64 `json:"unique_ratio_min,omitempty"`
	ShortLineWindow      int     `json:"short_line_window,omitempty"`
	ShortLineMaxChars    int     `json:"short_line_max_chars,omitempty"`
	ShortLineRepeatLimit int     `json:"short_line_repeat_limit,omitempty"`
	ShortLineMinCount    int     `json:"short_line_min_count,omitempty"`
	InlineMinPeriod      int     `json:"inline_min_period,omitempty"`
	InlineMaxPeriod      int     `json:"inline_max_period,omitempty"`
}

// withDefaults 用 DefaultConfig 补齐零值（不能把 0 当阈值，否则等于关掉判据）。
func (c Config) guardConfig() guard.Config {
	g := guard.DefaultConfig
	if c.StreakLimit > 0 {
		g.StreakLimit = c.StreakLimit
	}
	if c.MinLineChars > 0 {
		g.MinLineChars = c.MinLineChars
	}
	if c.WindowLines > 0 {
		g.WindowLines = c.WindowLines
	}
	if c.WindowMinLines > 0 {
		g.WindowMinLines = c.WindowMinLines
	}
	if c.UniqueRatioMin > 0 {
		g.UniqueRatioMin = c.UniqueRatioMin
	}
	if c.ShortLineWindow > 0 {
		g.ShortLineWindow = c.ShortLineWindow
	}
	if c.ShortLineMaxChars > 0 {
		g.ShortLineMaxChars = c.ShortLineMaxChars
	}
	if c.ShortLineRepeatLimit > 0 {
		g.ShortLineRepeatLimit = c.ShortLineRepeatLimit
	}
	if c.ShortLineMinCount > 0 {
		g.ShortLineMinCount = c.ShortLineMinCount
	}
	if c.InlineMinPeriod > 0 {
		g.InlineMinPeriod = c.InlineMinPeriod
	}
	if c.InlineMaxPeriod > 0 {
		g.InlineMaxPeriod = c.InlineMaxPeriod
	}
	return g
}

// parseConfig 解 core 下发的配置；缺省为「启用 + 只看 reasoning」。
func parseConfig(p jsonl.RawMessage) (Config, jsonl.RawMessage) {
	var env struct {
		Config json.RawMessage `json:"config"`
	}
	cfg := Config{Enabled: true, Watch: "reasoning"}
	if len(p) > 0 {
		_ = json.Unmarshal(p, &env)
	}
	if len(env.Config) > 0 && string(env.Config) != "null" {
		// 显式 false 要生效：先解到临时结构判断键是否存在。
		var probe map[string]any
		_ = json.Unmarshal(env.Config, &probe)
		var c Config
		if err := json.Unmarshal(env.Config, &c); err == nil {
			if _, ok := probe["enabled"]; !ok {
				c.Enabled = true
			}
			if c.Watch == "" {
				c.Watch = "reasoning"
			}
			cfg = c
		}
	}
	return cfg, env.Config
}

// Daemon 是本插件的方法表宿主。
type Daemon struct{}

// New 构造插件实例。
func New() *Daemon { return &Daemon{} }

// Registry 注册全部方法。
func (d *Daemon) Registry() *plugin.Registry {
	r := plugin.New()
	r.Lifecycle(Manifest())
	r.Handle("config.schema", func(ctx context.Context, _ jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		return mustJSON(configSchema()), nil
	})
	r.Handle("config.get", func(ctx context.Context, p jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		_, raw := parseConfig(p)
		if len(raw) == 0 {
			raw = jsonl.RawMessage(`{}`)
		}
		return raw, nil
	})
	r.HandleDuplex("hook.stream", d.Stream)
	return r
}

// Stream 是 hook.stream 的实现：把上游事件逐条过一遍守卫，原样转发；
// 命中即返回错误，让 core 中断整段生成。
func (d *Daemon) Stream(ctx context.Context, p jsonl.RawMessage, s *rpc.Stream) (jsonl.RawMessage, *jsonl.ErrObj) {
	cfg, _ := parseConfig(p)
	if !cfg.Enabled {
		return passThrough(ctx, s, nil)
	}
	g := guard.New(cfg.guardConfig())
	watchText := cfg.Watch == "text" || cfg.Watch == "both"
	watchReason := cfg.Watch == "reasoning" || cfg.Watch == "both"

	for {
		m, err := s.Next(ctx)
		if err != nil {
			// 上游正常结束（core 发完 upstream.end 后关闭）或 ctx 取消。
			if hit := g.Mark(); hit != nil {
				return nil, hitErr(hit)
			}
			return mustJSON(map[string]any{"ok": true}), nil
		}
		// 帧形状只有两种：`upstream.end` 结束帧，以及事件名即上游归一化事件名
		// （reasoning.delta / text.delta / tool_call.delta / usage / finish_reason / ...）。
		if m.Event == "upstream.end" || m.Event == "upstream.done" {
			if hit := g.Mark(); hit != nil {
				return nil, hitErr(hit)
			}
			// 结束帧不往下游转发：它只是 core 给 hook 的收尾信号，不是对话事件。
			return mustJSON(map[string]any{"ok": true}), nil
		}
		// 只对正文类增量做检测；其余事件（tool_call/usage/finish_reason）直接透传。
		if (m.Event == "reasoning.delta" && watchReason) || (m.Event == "text.delta" && watchText) {
			if hit := g.Push(contentOf(m.Data)); hit != nil {
				return nil, hitErr(hit)
			}
		}
		if err := s.Event(m.Event, m.Data); err != nil {
			return nil, jsonl.Errorf(errkind.Conn, errkind.ScopeChannel, "写下游事件失败: %v", err)
		}
	}
}

// passThrough 关闭守卫时把上游事件原样转发（便于对比排查）。
func passThrough(ctx context.Context, s *rpc.Stream, _ *guard.Hit) (jsonl.RawMessage, *jsonl.ErrObj) {
	for {
		m, err := s.Next(ctx)
		if err != nil {
			return mustJSON(map[string]any{"ok": true}), nil
		}
		if m.Event == "upstream.end" || m.Event == "upstream.done" {
			return mustJSON(map[string]any{"ok": true}), nil
		}
		if err := s.Event(m.Event, m.Data); err != nil {
			return nil, jsonl.Errorf(errkind.Conn, errkind.ScopeChannel, "写下游事件失败: %v", err)
		}
	}
}

// hitErr 把命中转成终止错误：kind=reasoning_loop（不可换号）、scope=model。
//
// 为什么不让 hook 自己发 error 事件：终止事件只能出现一次（W3-10），统一由 core
// 在拿到这个错误后发，语义与「上游报错」完全一致，gateway 无需为 hook 特判。
func hitErr(hit *guard.Hit) *jsonl.ErrObj {
	msg := "检测到思考链死循环，已中断生成：" + hit.Reason
	return jsonl.Errorf(errkind.ReasoningLoop, errkind.ScopeModel, "%s", msg)
}

// contentOf 取增量事件的正文（{content} 或 {text}）。
func contentOf(raw jsonl.RawMessage) string {
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		return ""
	}
	if s, ok := d["content"].(string); ok {
		return s
	}
	if s, ok := d["text"].(string); ok {
		return s
	}
	return ""
}

// configSchema 面板动态表单（W3-17）。
func configSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"enabled": map[string]any{
				"type":        "boolean",
				"title":       "启用检测",
				"description": "关掉后本拦截器变成纯透传，可用于对比排查。",
				"default":     true,
			},
			"watch": map[string]any{
				"type":        "string",
				"title":       "检测范围",
				"description": "默认只盯思维链；正文也盯会更容易误杀正常输出。",
				"enum":        []string{"reasoning", "text", "both"},
				"enumNames":   []string{"仅思维链", "仅正文", "两者"},
				"default":     "reasoning",
			},
			"streak_limit": map[string]any{
				"type": "integer", "title": "同行连续重复次数", "default": guard.DefaultConfig.StreakLimit,
				"description": "同一行连续出现这么多次即判复读（行长需 ≥ 最短行长）。",
			},
			"min_line_chars": map[string]any{
				"type": "integer", "title": "计入复读的最短行长", "default": guard.DefaultConfig.MinLineChars,
			},
			"window_lines": map[string]any{
				"type": "integer", "title": "空转检测窗口行数", "default": guard.DefaultConfig.WindowLines,
			},
			"window_min_lines": map[string]any{
				"type": "integer", "title": "窗口最少行数", "default": guard.DefaultConfig.WindowMinLines,
			},
			"unique_ratio_min": map[string]any{
				"type": "number", "title": "不同行占比下限", "default": guard.DefaultConfig.UniqueRatioMin,
				"description": "低于该比例判为空转。",
			},
			"short_line_window": map[string]any{
				"type": "integer", "title": "短行窗口行数", "default": guard.DefaultConfig.ShortLineWindow,
			},
			"short_line_max_chars": map[string]any{
				"type": "integer", "title": "短行最大字符数", "default": guard.DefaultConfig.ShortLineMaxChars,
			},
			"short_line_repeat_limit": map[string]any{
				"type": "integer", "title": "短行重复上限", "default": guard.DefaultConfig.ShortLineRepeatLimit,
			},
			"short_line_min_count": map[string]any{
				"type": "integer", "title": "短行判定最少条数", "default": guard.DefaultConfig.ShortLineMinCount,
			},
		},
	}
}

// mustJSON 序列化失败即 panic（plugin.runGuarded 会转成 server 错误）。
func mustJSON(v any) jsonl.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("序列化失败: %v", err))
	}
	return b
}

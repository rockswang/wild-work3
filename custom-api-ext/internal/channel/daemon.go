// Package channel 实现 custom-api 渠道插件（自定义 OpenAI 兼容端点，API Key 认证）。
//
// 运行形态遵循 W3-20：`call`（一次性）与 `stdio`（常连）两种形态，
// 两者由同一份 registry() 方法表驱动，不存在“CLI 专用捷径”。core 一律用 call，
// 一次调用一个进程、结束即回收，不常驻。
package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/meta"
	"wild-work3/main/protocol/plugin"
	"wild-work3/main/protocol/stat"
)

// PluginID 插件标识。
const PluginID = "custom-api"

// Manifest 本插件元数据。
func Manifest() meta.Manifest {
	return meta.Manifest{
		ID: PluginID,
		// Name 即面板「＋ 自定义模型」按钮文案（账号页按插件 name 渲染入口），沿用旧版叫法。
		Name:    "自定义模型",
		Version: "1.0.0",
		Kind:    "channel",
		// 与旧版一致：Provider（渠道标识 + base_url + api_key）全部由用户自建，可任意多个，
		// 实例 ID 即模型命名空间前缀；因此不预置空配置占位渠道（面板「自定义模型」列表增删改）。
		MultiChannels: true,
		Protocol:      meta.ProtocolRng{Min: 1, Max: 1},
		Execution: meta.Execution{
			// call：core 每个调用起一个进程（控制面查模型、会话面 chat.generate）
			Control:            plugin.ModeCall,
			Conversation:       plugin.ModeCall,
			MaxWorkers:         4, // 同插件并发上限（防止面板批量操作瞬间拉起一堆进程）
			IdleTimeoutSeconds: 0, // 无常驻进程，无空闲回收概念
		},
		Capabilities: meta.Capabilities{
			Control:      []string{"models.list", "config.get", "config.set", "config.schema", "accounts.list", "credits.get", "stats.get", "health.check", "ping"},
			Conversation: []string{"chat.generate"},
		},
		Icon:        "custom",
		Color:       "#7c8cf8",
		Description: "自定义 OpenAI 兼容 API 端点（API Key 认证）",
	}
}

// Serve 运行插件（call 或 stdio 两种形态，共用同一份方法表）。
//
// data 是插件数据目录（<data>/{auths,logs,stat} 的父目录）；统计流水落 <data>/stat。
func Serve(ctx context.Context, mode, data string, in io.Reader, out io.Writer) error {
	r := registry(data)
	switch mode {
	case plugin.ModeCall:
		return r.ServeOnce(ctx, in, out)
	default:
		return r.ServeStdio(ctx, in, out)
	}
}

// registry 组装完整方法表（两种运行形态共用）。
func registry(data string) *plugin.Registry {
	r := plugin.New()
	st := stat.Open(data) // data 为空则返回 nil：统计静默缺失，不影响对话（fail-open）
	r.Lifecycle(Manifest())
	r.Handle("config.schema", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(configSchema()), nil
	})
	// 配置由 core 持有并随每次调用下发；插件侧无状态，故 config.get 回空对象。
	r.Handle("config.get", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(map[string]any{}), nil
	})
	r.Handle("config.set", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(map[string]any{"ok": true}), nil
	})
	r.Handle("models.list", handleModelsList)
	// API Key 渠道：一个 channel 配置即一个账号，账号由 core 管理，插件不上报账号。
	r.Handle("accounts.list", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(map[string]any{"accounts": []meta.AccountInfo{}}), nil
	})
	// 通用 OpenAI 兼容端点没有余额接口：null = 未提供（≠ 0）。
	r.Handle("credits.get", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(map[string]any{"balance": nil, "items": []meta.ResourceItem{}}), nil
	})
	// 渠道自记统计（W3-22）：插件知道上游真实缓存命中/积分消耗/耗时，core 汇总展示。
	r.Handle("stats.get", func(ctx context.Context, p Raw) (Raw, *Err) {
		var in struct {
			Days int `json:"days"`
		}
		_ = json.Unmarshal(p, &in)
		if in.Days <= 0 {
			in.Days = 7
		}
		if st == nil {
			return mustJSON(stat.Summary{Days: []stat.Day{}}), nil
		}
		return mustJSON(stat.Summarize(st.Query(in.Days))), nil
	})
	r.HandleStream("chat.generate", func(ctx context.Context, p Raw, emit plugin.Emit) (Raw, *Err) {
		return handleGenerate(ctx, p, emit, st)
	})
	return r
}

// configSchema 返回渠道配置 JSON Schema（Dashboard 自动生成表单）。
func configSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"base_url", "api_key"},
		"properties": map[string]any{
			"base_url": map[string]any{"type": "string", "title": "API 基址", "description": "如 https://api.openai.com/v1", "default": "https://api.openai.com/v1"},
			"api_key":  map[string]any{"type": "string", "title": "API Key", "description": "仅存本地，不回显", "format": "password"},
			"models":   map[string]any{"type": "array", "title": "模型列表", "items": map[string]any{"type": "string"}},
			"proxy":    map[string]any{"type": "string", "title": "代理", "description": "可选，如 http://127.0.0.1:10809"},
			"timeout":  map[string]any{"type": "integer", "title": "超时(秒)", "default": 300},
			"headers":  map[string]any{"type": "object", "title": "附加请求头"},
		},
	}
}

// errKindOf 从上游错误中取分类（当前统一按 channel 作用域）。
func errKindOf(err error) (errkind.Kind, errkind.Scope) {
	if se, ok := err.(*StatusError); ok {
		return se.Kind, se.Scope
	}
	return errkind.Conn, errkind.ScopeChannel
}

// newHTTPClient 按配置构造带超时与代理的客户端。
// 注意：流式请求必须 Timeout=0（整体超时由 ctx + idle 看门狗控制），只在这里设代理/Transport。
func newHTTPClient(cfg Config, timeout time.Duration) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, &StatusError{Kind: errkind.BadParams, Scope: errkind.ScopeChannel, Msg: "代理地址非法: " + cfg.Proxy}
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// MarshalPretty 输出缩进 JSON（CLI 直接调用时给人看）。
func MarshalPretty(v any) string {
	b, err := jsonMarshalIndent(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// envOr 读环境变量，缺失返回默认值。
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envInt 读整型环境变量。
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

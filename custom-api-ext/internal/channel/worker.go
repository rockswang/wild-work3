package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work3/main/protocol/chat"
	"wild-work3/main/protocol/cli"
	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/plugin"
	"wild-work3/main/protocol/stat"
)

// Log 是插件进程级日志器（main 里初始化；nil 时静默）。stdout 永远是协议，日志只能走这里/stderr。
var Log *cli.Logger

// Config 是 core 通过 channel_config 下发的渠道配置。
type Config struct {
	BaseURL string            `json:"base_url"`
	APIKey  string            `json:"api_key"`
	Models  []string          `json:"models,omitempty"`
	Proxy   string            `json:"proxy,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// StatusError 携带错误分类，供 daemon 侧转成统一 errObj。
type StatusError struct {
	Kind  errkind.Kind
	Scope errkind.Scope
	Msg   string
}

func (e *StatusError) Error() string { return e.Msg }

// handleGenerate 执行一次上游流式请求，逐事件转发（call/stdio 两种形态共用）。
//
// st 是渠道自记统计（W3-22）：每次调用追加一条 kind=chat 流水，供 core 聚合展示；
// 未上报的 token 记 -1（区分「上游没说」与「真的是 0」），写失败不影响对话。
func handleGenerate(ctx context.Context, p Raw, emit plugin.Emit, st *stat.Store) (Raw, *Err) {
	var req chat.GenerateRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请求体解析失败: %v", err)
	}
	var cfg Config
	if len(req.ChannelConfig) > 0 {
		if err := json.Unmarshal(req.ChannelConfig, &cfg); err != nil {
			return nil, errf(errkind.BadParams, errkind.ScopeRequest, "渠道配置解析失败: %v", err)
		}
	}
	// rec 在 return 前统一落盘（defer 保证错误路径也有记录）
	rec := stat.Record{
		Kind: stat.KindChat, Channel: PluginID, Account: req.AccountUID, Model: upstreamModel(req.Model),
		PromptTokens: -1, CompletionTokens: -1, TotalTokens: -1, CachedTokens: -1,
	}
	defer func() {
		if st != nil {
			st.Append(rec)
		}
	}()
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		return nil, errf(errkind.Auth, errkind.ScopeChannel, "渠道未配置 base_url 或 api_key")
	}
	upModel := upstreamModel(req.Model)

	start := time.Now()
	var firstTokenAt time.Time
	var usage *chat.Usage

	// 构造上游请求体：OpenAI Chat Completions 流式。
	// 以 **下游透传袋为起点**、强类型字段后覆（旧版 `chatRequest = map[string]any` 全透传的等价物）：
	// 否则 stop / top_p / n / seed / *_penalty / response_format / logit_bias / user 这些
	// 不在 IPC 强类型契约里的采样参数会在网关就丢，官方样板渠道自己先漏一大片。
	body := map[string]any{}
	if len(req.Extra) > 0 {
		if err := json.Unmarshal(req.Extra, &body); err != nil {
			return nil, errf(errkind.BadParams, errkind.ScopeRequest, "extra 透传字段没有是 JSON 对象: %v", err)
		}
	}
	body["model"] = upModel
	body["messages"] = req.Messages
	body["stream"] = true
	// 末帧 usage 靠它才下发；客户端显式给了 stream_options 就尊重客户端的（旧版 D7）。
	if _, ok := body["stream_options"]; !ok {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = req.ToolChoice
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	raw, _ := json.Marshal(body)

	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "构造上游请求失败: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, v := range cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	Log.Infof("上游请求 model=%s endpoint=%s stream=true msgs=%d tools=%d", upModel, endpoint, len(req.Messages), len(req.Tools))

	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	cli2, err := newHTTPClient(cfg, 0) // 流式：不设整体超时，用 ctx + idle 看门狗
	if err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeChannel, "%v", err)
	}
	resp, err := cli2.Do(httpReq)
	if err != nil {
		return nil, errf(errkind.Conn, errkind.ScopeChannel, "上游连接失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		kind, scope := classifyHTTP(resp.StatusCode, msg)
		Log.Errorf("上游非 200: status=%d kind=%s body=%s", resp.StatusCode, kind, cli.RedactJSON(truncate(string(msg), 500)))
		return nil, errf(kind, scope, "上游 %d: %s", resp.StatusCode, truncate(string(msg), 500))
	}

	// 逐行解析 SSE（SSE 非 JSONL：需按 "data: " 前缀剥离；本渠道不把 SSE 当内部协议）
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	idleTimeout := timeout
	lastAt := time.Now()
	var finish string
	var frames int

	for sc.Scan() {
		// idle 看门狗：单次等待超时即中断
		if time.Since(lastAt) > idleTimeout {
			return nil, errf(errkind.IdleTimeout, errkind.ScopeChannel, "上游空闲超时 %s", idleTimeout)
		}
		lastAt = time.Now()
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue // event:/id: 等行本渠道不用
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		frames++
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			// Usage 保留原始对象：缓存计量藏在渠道族各异的键里
			// （prompt_tokens_details / cache_read_input_tokens / cache_creation_input_tokens），
			// 用结构体接收未知键会被静默丢弃（面板缓存利用率恒为「—」）。
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			Log.Debugf("跳过无法解析的帧: %s", truncate(data, 200))
			continue // 容忍单帧坏数据，不中断主流
		}
		// 上游错误信封（部分兼容实现用 {"error":{...}}）
		var errEnv struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &errEnv) == nil && errEnv.Error != nil {
			kind := classifyMsg(errEnv.Error.Message, errEnv.Error.Type)
			Log.Errorf("上游错误信封 kind=%s msg=%s", kind, errEnv.Error.Message)
			return nil, errf(kind, errkind.ScopeChannel, "%s", errEnv.Error.Message)
		}

		for _, ch := range chunk.Choices {
			if c := ch.Delta.ReasoningContent; c != "" {
				_ = emit("reasoning.delta", map[string]any{"content": c})
			}
			if c := ch.Delta.Content; c != "" {
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				_ = emit("text.delta", map[string]any{"content": c})
			}
			for i, tc := range ch.Delta.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				frag := map[string]any{"index": idx}
				if tc.ID != "" {
					frag["id"] = tc.ID
				}
				if tc.Type != "" {
					frag["type"] = tc.Type
				}
				if tc.Function.Name != "" {
					frag["name"] = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					frag["arguments_fragment"] = tc.Function.Arguments
				}
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				_ = emit("tool_call.delta", frag)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
		if len(chunk.Usage) > 0 {
			var raw map[string]any
			if json.Unmarshal(chunk.Usage, &raw) == nil && len(raw) > 0 {
				usage = usageFromRaw(raw)
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		// 截断：不得伪装正常收尾（R36）
		return nil, errf(errkind.Truncated, errkind.ScopeChannel, "上游流中断: %v", err)
	}

	// 结束原因与用量
	if finish != "" {
		_ = emit("finish_reason", map[string]any{"reason": finish})
	}
	u := usage
	if u == nil {
		u = &chat.Usage{} // null = 上游未提供，不等于 0
	}
	u.TimeMs = time.Since(start).Milliseconds()
	if !firstTokenAt.IsZero() {
		u.FirstTokenMs = firstTokenAt.Sub(start).Milliseconds()
	}
	_ = emit("usage", u)
	_ = emit("completed", map[string]any{"finish_reason": finish})
	Log.Infof("上游完成 frames=%d finish=%s tokens=%d/%d 耗时=%dms 首字=%dms", frames, finish, u.PromptTokens, u.CompletionTokens, u.TimeMs, u.FirstTokenMs)

	// 统计流水：usage 为 nil 时各项保持 -1（未上报 ≠ 0）
	rec.OK, rec.TimeMs, rec.FirstTokenMs = true, int(u.TimeMs), int(u.FirstTokenMs)
	if usage != nil {
		rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens = u.PromptTokens, u.CompletionTokens, u.TotalTokens
		if u.Cache != nil && u.Cache.CachedReported {
			rec.CachedTokens, rec.CachedReported = u.Cache.Cached, true
		}
	}
	return mustJSON(map[string]any{"ok": true}), nil
}

// upstreamModel 剥掉 channel/ 前缀（模型命名空间见 IPC_CHAT §2.1）。
func upstreamModel(m string) string {
	if i := strings.Index(m, "/"); i > 0 {
		return m[i+1:]
	}
	return m
}

// classifyHTTP 按状态码粗分类（细分交给上游错误信封/消息）。
// 注意：401/403 也可能携带 "invalid_model / no access to it"（鉴权已过、但无模型授权）
// 这种是模型级错误而非账号级，否则 core 会误把唯一账号打成 dead 冷却。
func classifyHTTP(code int, body []byte) (errkind.Kind, errkind.Scope) {
	if code == 400 || code == 401 || code == 403 || code == 404 {
		if k := classifyMsg(string(body), ""); k != errkind.Client {
			if k == errkind.ModelBlocked || k == errkind.NotFound {
				return k, errkind.ScopeModel
			}
			return k, errkind.ScopeRequest
		}
	}
	switch {
	case code == 402:
		// 402 直判硬余额（旧版 dev/internal/upstream/client.go Classify）：
		// 上游用 402 表达余额/配额耗尽，与账号健康强相关，必须先于文案标记。
		return errkind.HardCredit, errkind.ScopeAccount
	case code == 401 || code == 403:
		return errkind.Auth, errkind.ScopeAccount
	case code == 429:
		return errkind.SoftRate, errkind.ScopeAccount
	case code == 404:
		return errkind.NotFound, errkind.ScopeModel
	case code >= 500:
		return errkind.Server, errkind.ScopeChannel
	case code == 400:
		return errkind.Client, errkind.ScopeRequest
	}
	return errkind.Client, errkind.ScopeChannel
}

// classifyMsg 按上游消息文本归类（沿用旧版关键字判据思路）。
// 顺序很重要：模型相关判据要排在鉴权之前，否则 "401 invalid_model" 会被误判为账号错误。
func classifyMsg(msg, typ string) errkind.Kind {
	m := strings.ToLower(msg + " " + typ)
	switch {
	case strings.Contains(m, "insufficient") || strings.Contains(m, "quota") || strings.Contains(m, "balance"):
		return errkind.HardCredit
	case strings.Contains(m, "rate limit") || strings.Contains(m, "too many requests") || strings.Contains(m, "overratelimit"):
		return errkind.SoftRate
	case strings.Contains(m, "context length") || strings.Contains(m, "too long") || strings.Contains(m, "maximum context"):
		return errkind.PromptTooLong
	case strings.Contains(m, "content") && (strings.Contains(m, "filter") || strings.Contains(m, "policy")):
		return errkind.ContentBlocked
	case strings.Contains(m, "invalid_model") || strings.Contains(m, "does not exist") || strings.Contains(m, "do not have access") ||
		(strings.Contains(m, "model") && (strings.Contains(m, "not found") || strings.Contains(m, "not exist") || strings.Contains(m, "unsupported"))):
		return errkind.ModelBlocked
	}
	return errkind.Client
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ = fmt.Sprintf

// usageFromRaw 把上游原始 usage 对象转成协议用量（W3-34 统一口径）。
//
// 缓存字段**渠道族各异**（prompt_tokens_details.cached_tokens / cache_read_input_tokens /
// cache_creation_input_tokens…），取数统一走 chat.CacheFromMap（与 main/protocol 的优先级
// 逐条一致）。三维 token 打 reported 标记：明确 0 也是有效实报，本地相加得来的 total 不算。
func usageFromRaw(raw map[string]any) *chat.Usage {
	u := &chat.Usage{
		PromptTokens:     rawInt(raw["prompt_tokens"]),
		CompletionTokens: rawInt(raw["completion_tokens"]),
		TotalTokens:      rawInt(raw["total_tokens"]),
	}
	derived := u.TotalTokens == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0)
	if derived {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	u.Cache = chat.CacheFromMap(raw)
	u.MarkReported()
	if derived {
		u.TotalReported = false
	}
	return u
}

// rawInt 取数值字段（JSON 数字统一是 float64）；缺失/非数值按 0。
func rawInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

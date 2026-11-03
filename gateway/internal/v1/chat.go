// Package v1 实现对外三协议 HTTP API：Chat Completions（基础）+ Responses + Anthropic Messages（转换兼容层）。
package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wild-work3/main/gateway/internal/ipc"
	"wild-work3/main/protocol/chat"
	"wild-work3/main/protocol/jsonl"
	"wild-work3/main/protocol/rpc"
)

// Handler 三协议入口。
type Handler struct {
	IPC      *ipc.CoreIPC
	APIKey   string        // 模型鉴权 key（空则不校验）
	KeyFn    func() string // 若设置则每次请求动态取 key（gateway 管理面可改）
	CoreBase string        // core 管理面基址，如 http://127.0.0.1:7864
	Version  map[string]string
}

// Routes 注册 /v1/* 路由。
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", h.auth(h.chatCompletions))
	mux.HandleFunc("/v1/completions", h.auth(h.chatCompletions)) // 兼容旧 completions
	mux.HandleFunc("/v1/responses", h.auth(h.responses))
	mux.HandleFunc("/v1/messages", h.auth(h.messages))
	mux.HandleFunc("/v1/messages/count_tokens", h.auth(h.countTokens))
	mux.HandleFunc("/v1/models", h.auth(h.models))
	// 标准可选业务能力（W3-30，非流式一次性调用；渠道不支持时 core 回 not_supported）
	mux.HandleFunc("/v1/systemone", h.auth(h.systemone))
	mux.HandleFunc("/v1/images/generations", h.auth(h.imagesGenerations))
	mux.HandleFunc("/v1/images/edits", h.auth(h.imagesEdits))
}

// auth 模型鉴权（Bearer / x-api-key）。
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := h.APIKey
		if h.KeyFn != nil {
			key = h.KeyFn()
		}
		if key == "" {
			next(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.Header.Get("x-api-key")
		}
		if tok != key {
			errJSON(w, http.StatusUnauthorized, "invalid api key", "auth")
			return
		}
		next(w, r)
	}
}

// chatCompletions 基础协议：OpenAI Chat Completions。
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed", "bad_params")
		return
	}
	var body map[string]any
	if err := decodeBody(w, r, &body); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			errJSON(w, http.StatusRequestEntityTooLarge, err.Error(), "request_too_large")
			return
		}
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), "bad_params")
		return
	}
	// 入站归一化 + 图片降采样（纯函数，见 normalize.go / imageslim.go）
	normalizeInbound(body)
	req, err := toGenerateRequest(body)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error(), "bad_params")
		return
	}
	stream, _ := body["stream"].(bool)
	id := newID()

	if !stream {
		// 下游非流式 → 在流式管线完成后聚合（W3-13 #9）
		h.aggregate(w, r, req, id, body)
		return
	}
	// 流式：SSE。响应头必须**惰性**写入：模型降级要在第一帧之前改 `X-Model-Fallback`，
	// 而降级结果在流开始前才能确定（旧版规则 3：降级必留痕）。
	sse := newSSE(w)
	fbModel := "" // 非空=本请求已降级到该模型（改写 chunk 的 model 字段）
	send := func(payload any) { sse.frame("", payload) }
	// outModel 下游看到的模型名（降级后写备胎全路径）
	outModel := func() string {
		if fbModel != "" {
			return fbModel
		}
		return req.Model
	}
	created := time.Now().Unix()
	// 客户端显式 stream_options.include_usage=false → 即使插件上报了 usage 也不发出去
	// （OpenAI 语义；插件若不遵守该开关，这里就是最后一道闸，旧版 D7 同口径）。
	wantUsage := includeUsageOf(body)

	_, _, err = h.IPC.ChatStream(r.Context(), req, func(name string, data json.RawMessage) error {
		var f map[string]any
		_ = json.Unmarshal(data, &f)
		switch name {
		case "model.fallback":
			// core 已决定改用备胎模型：记下并改写下游看到的 model（不写任何正文）
			fbModel = asString(f["to"])
			sse.setHeader("X-Model-Fallback", req.Model+" -> "+fbModel)
		case "text.delta":
			send(chunkDelta(id, created, outModel(), map[string]any{"content": asString(f["content"])}, nil))
		case "reasoning.delta":
			send(chunkDelta(id, created, outModel(), map[string]any{"reasoning_content": asString(f["content"])}, nil))
		case "tool_call.delta":
			tc := map[string]any{"index": f["index"]}
			if v, ok := f["id"]; ok {
				tc["id"] = v
			}
			if v, ok := f["type"]; ok {
				tc["type"] = v
			}
			fn := map[string]any{}
			if v, ok := f["name"]; ok {
				fn["name"] = v
			}
			if v, ok := f["arguments_fragment"]; ok {
				fn["arguments"] = v
			}
			tc["function"] = fn
			send(chunkDelta(id, created, outModel(), map[string]any{"tool_calls": []any{tc}}, nil))
		case "finish_reason":
			fr := asString(f["reason"])
			send(chunkDelta(id, created, outModel(), map[string]any{}, &fr))
		case "usage":
			if !wantUsage {
				return nil
			}
			send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": outModel(),
				"choices": []any{}, "usage": usageOut(f)})
		}
		return nil
	})
	if err != nil {
		// 已发出部分响应 → 只能报错终止，不得伪装正常收尾（R36）
		send(map[string]any{"error": map[string]any{
			"message": err.Error(), "type": kindOf(err), "code": kindOf(err), "scope": "channel",
		}})
		return
	}
	// 仅当未发出任何事件时才补一个空 delta，保证客户端有内容
	if !sse.wroteAny() {
		send(chunkDelta(id, created, outModel(), map[string]any{"role": "assistant", "content": ""}, nil))
	}
	sse.raw("data: [DONE]\n\n")
}

// aggregate 聚合流式结果后返回非流式响应。
func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request, req *chat.GenerateRequest, id string, body map[string]any) {
	acc := &chat.Accumulator{}
	var usage *chat.Usage
	fbModel := ""
	_, _, err := h.IPC.ChatStream(r.Context(), req, func(name string, data json.RawMessage) error {
		var f map[string]any
		_ = json.Unmarshal(data, &f)
		if name == "model.fallback" {
			fbModel = asString(f["to"])
			return nil
		}
		acc.Add(name, f)
		if name == "usage" {
			var u chat.Usage
			if json.Unmarshal(data, &u) == nil {
				usage = &u
			}
		}
		return nil
	})
	if fbModel != "" {
		w.Header().Set("X-Model-Fallback", req.Model+" -> "+fbModel) // 降级必留痕（规则 3）
	}
	if err != nil {
		kind := kindOf(err)
		errJSON(w, statusForKind(kind), err.Error(), kind)
		return
	}
	msg := map[string]any{"role": "assistant"}
	if acc.Text.Len() > 0 {
		msg["content"] = acc.Text.String()
	}
	if acc.Reasoning.Len() > 0 {
		msg["reasoning_content"] = acc.Reasoning.String()
	}
	if len(acc.ToolCalls) > 0 {
		var tcs []any
		for _, t := range acc.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id": t.ID, "type": orDefault(t.Type, "function"),
				"function": map[string]any{"name": t.Name, "arguments": t.ArgumentsBuilder.String()},
			})
		}
		msg["tool_calls"] = tcs
	}
	fr := orDefault(acc.FinishReason, "stop")
	resp := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": orDefault(fbModel, req.Model),
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": fr}},
	}
	if usage != nil {
		// 非流式也要保留缓存命中明细（与流式同口径，null ≠ 0）
		u := map[string]any{
			"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens,
		}
		if usage.Cache != nil && usage.Cache.CachedReported {
			u["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.Cache.Cached}
		}
		resp["usage"] = u
	}
	writeJSON(w, resp)
}

func chunkDelta(id string, created int64, model string, delta map[string]any, finish *string) map[string]any {
	ch := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		ch["finish_reason"] = *finish
	} else {
		ch["finish_reason"] = nil
	}
	return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{ch}}
}

func usageOut(f map[string]any) map[string]any {
	out := map[string]any{
		"prompt_tokens":     f["prompt_tokens"],
		"completion_tokens": f["completion_tokens"],
		"total_tokens":      f["total_tokens"],
	}
	if c, ok := f["cache"].(map[string]any); ok {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": c["cached"]}
	}
	return out
}

// toGenerateRequest 把下游 Chat 请求转成归一化 IPC 请求。
//
// 返回错误时调用方应回 400：类型不对（如 messages 不是数组）必须尽早报错，
// 静默变空历史会让上游回一个无关的默认回答，比直接报参数错误更难排查。
func toGenerateRequest(body map[string]any) (*chat.GenerateRequest, error) {
	req := &chat.GenerateRequest{}
	req.Model, _ = body["model"].(string)
	req.Stream = true
	if v, ok := body["temperature"].(float64); ok {
		req.Temperature = &v
	}
	if n, ok := asInt(body["max_tokens"]); ok && n > 0 {
		// 必须宽容取整：归一化把 max_completion_tokens 别名回写成 int64（去小数尾巴），
		// 早先的 .(float64) 断言会让「只发别名」的请求（OpenAI o-series / 新客户端）
		// MaxTokens 静默变 nil，上游按自身默认上限输出（成本与截断失控）。
		req.MaxTokens = &n
	}
	req.ToolChoice = body["tool_choice"]
	if s, ok := body["reasoning_effort"].(string); ok {
		req.ReasoningEffort = s
	}
	if b, ok := body["thinking"].(bool); ok {
		req.Thinking = b
	}
	if cid, ok := body["conversation_id"].(string); ok {
		req.ConversationID = cid
	}
	// messages 原样透传（最小超集，保留上游私有字段）
	// 类型不对时报错而非静默变空历史：空历史会让上游回一个无关的默认回答，
	// 比直接报参数错误更难排查（Fail Early）。
	if raw, ok := body["messages"]; ok && raw != nil {
		msgs, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("messages 必须是数组，实际为 %T", raw)
		}
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("messages 元素必须是对象，实际为 %T", m)
			}
			cm := chat.ChatMessage{}
			cm.Role, _ = mm["role"].(string)
			cm.Content = mm["content"]
			if cm.Role == "tool" {
				// R113：工具结果里的图片必须走 vision 通道。旧版在公共 chat 路径做，
				// ww3 此前只有 Responses/Anthropic 两入口接了 toolContentValue，Chat 原生
				// role=tool 的截图会被 json.Marshal 成超长 base64 文本（实测 1.6MB 图 ≈146 万 token）。
				cm.Content = toolContentValue(mm["content"])
			}
			cm.ToolCallID, _ = mm["tool_call_id"].(string)
			cm.Name, _ = mm["name"].(string)
			if tcs, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					c := chat.ToolCall{}
					c.ID, _ = tcm["id"].(string)
					c.Type, _ = tcm["type"].(string)
					if fnm, ok := tcm["function"].(map[string]any); ok {
						c.Function = &chat.Fn{Name: asString(fnm["name"]), Arguments: asString(fnm["arguments"])}
					}
					cm.ToolCalls = append(cm.ToolCalls, c)
				}
			}
			req.Messages = append(req.Messages, cm)
		}
	}
	// Extra = 下游未映射字段的透传袋（stop / top_p / top_k / *_penalty / seed /
	// response_format / stream_options / user …）。
	//
	// 为什么需要：IPC 的 GenerateRequest 是强类型，采样参数不在契约里；旧版靠
	// `type chatRequest = map[string]any` 全量透传，ww3 早期实现漏了接线，导致这些参数
	// 在网关就被丢弃——而插件侧（kilo/cline/traework…）早已按「以 Extra 为起点拼上游 body」
	// 实现。这里补上生产端，透传能力即刻恢复。
	// 不塞 model/messages/tools/stream：大数组已在强类型字段里，重复一遍只会让帧翻倍。
	if b, err := json.Marshal(passthroughFields(body)); err == nil {
		req.Extra = b
	}
	// tools 原样序列化（$ref 已在入站 normalizeInbound 中内联消毒）
	if tools, ok := body["tools"]; ok {
		if b, err := json.Marshal(tools); err == nil {
			var ts []chat.Tool
			if json.Unmarshal(b, &ts) == nil {
				req.Tools = ts
			}
		}
	}
	return req, nil
}

// models 聚合 core 已启用渠道的模型列表（core /api/channels 返回裸数组，模型明细需按渠道拉）。
//
// 思考档位（W3-32）：与旧版 dev buildModelEntry 同口径——上游目录**显式声明**了档位
// 才输出 `reasoning` 对象与 `supports_reasoning`，未声明则两者都不出现（不编造）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	data := []any{}
	if h.CoreBase != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, h.CoreBase+"/api/channels", nil)
		if err == nil {
			if resp, err := http.DefaultClient.Do(httpReq); err == nil {
				var chans []struct {
					ChannelID string `json:"channel_id"`
					Enabled   bool   `json:"enabled"`
					Models    int    `json:"models"`
				}
				if json.NewDecoder(resp.Body).Decode(&chans) == nil {
					for _, ch := range chans {
						if !ch.Enabled || ch.Models == 0 {
							continue
						}
						for _, mi := range h.channelModelInfos(ctx, ch.ChannelID) {
							data = append(data, buildModelEntry(ch.ChannelID, mi))
						}
					}
				}
				resp.Body.Close()
			}
		}
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

// modelDetail 是 core `/api/channels/{id}/models/detail` 的条目形状
// （meta.ModelInfo 的可消费子集；档位字段未声明时为 nil/空）。
type modelDetail struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Context            *int     `json:"context"`
	MaxTokens          *int     `json:"max_tokens"` // 上游声明的输出上限（meta.ModelInfo.MaxTokens，nil=未声明）
	Efforts            []string `json:"efforts"`
	DefaultEffort      string   `json:"default_effort"`
	CanDisableThinking *bool    `json:"can_disable_thinking"`
	// Capabilities 是 meta.ModelInfo.Capabilities（插件 models.list 上报 / custom-api 探测）。
	// 早先的视图没解析它，导致 /v1/models 只能硬编码 ["text"]、不宣告 tools。
	Capabilities struct {
		Tools     bool `json:"tools"`
		Vision    bool `json:"vision"`
		Reasoning bool `json:"reasoning"`
	} `json:"capabilities"`
}

// buildModelEntry 构造单条 /v1/models 条目。
//
// OpenAI 官方仅规定 id/object/created/owned_by，能力字段按 OpenRouter / llama.cpp
// 通行口径透传。思考档位沿用旧版 dev 的 reasoning 对象形状：
//
//	"reasoning": {"supported_efforts": [...], "default_effort": "...",
//	              "can_disable_thinking": bool, "mandatory": bool}
//
// 上游未声明档位时**整个 reasoning 对象与 supports_reasoning 都不输出**（R49 不编造）。
func buildModelEntry(owner string, m modelDetail) map[string]any {
	// created 用旧版同一常量：模型目录没有真实发布日期，0 会让客户端算出「1970 年发布」。
	entry := map[string]any{"id": m.ID, "object": "model", "created": modelCreatedEpoch, "owned_by": owner}
	if m.Context != nil && *m.Context > 0 {
		entry["context_length"] = *m.Context
	}
	// 输出上限：上游声明了才输出（旧版 buildModelEntry 同口径，R49 不编造）。
	if m.MaxTokens != nil && *m.MaxTokens > 0 {
		entry["max_output_tokens"] = *m.MaxTokens
	}
	// 模态按上游声明的 vision 能力推导（与旧版 ModelInfo.InputModalities()/Modality() 同口径：
	// 没有声明就只报 text，绝不编造 image）。
	mods, modality := []string{"text"}, "text->text"
	if m.Capabilities.Vision {
		mods, modality = []string{"text", "image"}, "text+image->text"
	}
	entry["architecture"] = map[string]any{"input_modalities": mods, "modality": modality}
	var params []string
	if m.Capabilities.Tools {
		entry["supports_tools"] = true
		params = append(params, "tools", "tool_choice")
	}
	if len(m.Efforts) > 0 || m.DefaultEffort != "" {
		reasoning := map[string]any{}
		if len(m.Efforts) > 0 {
			reasoning["supported_efforts"] = m.Efforts
		}
		if m.DefaultEffort != "" {
			reasoning["default_effort"] = m.DefaultEffort
		}
		if m.CanDisableThinking != nil {
			reasoning["can_disable_thinking"] = *m.CanDisableThinking
			reasoning["mandatory"] = !*m.CanDisableThinking
		}
		entry["reasoning"] = reasoning
		entry["supports_reasoning"] = true
		params = append(params, "reasoning", "reasoning_effort")
	}
	if len(params) > 0 {
		entry["supported_parameters"] = params
	}
	return entry
}

// modelCreatedEpoch 是旧版 dev/internal/server/handler.go buildModelEntry 的同一常量。
const modelCreatedEpoch = 1753600000

// channelModelInfos 拉单个渠道的模型明细（core `/api/channels/{id}/models/detail`）。
// 接口不可用（旧 core）时退化为只有 ID 的条目，不影响 /v1/models 可用性。
func (h *Handler) channelModelInfos(ctx context.Context, id string) []modelDetail {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.CoreBase+"/api/channels/"+url.PathEscape(id)+"/models/detail", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out []modelDetail
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out
}

// channelModels 拉单个渠道的模型 ID 列表（core GET /api/channels/{id} 的 models 字段）。
func (h *Handler) channelModels(ctx context.Context, id string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.CoreBase+"/api/channels/"+url.PathEscape(id), nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var v struct {
		Models []string `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return nil
	}
	return v.Models
}

// countTokens 估算输入 token 数（Anthropic count_tokens 兼容端点）。
//
// 不用真实 tokenizer：旧版实测同一套估算口径足够（CJK 1 字 ≈ 1 token，其他 ≈ 4 字节/token，
// 再按内容块数给下限），且避免为一条估算接口引入词表依赖。
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(w, r, &body); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			errJSON(w, http.StatusRequestEntityTooLarge, err.Error(), "request_too_large")
			return
		}
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), "bad_params")
		return
	}
	writeJSON(w, map[string]any{"input_tokens": estimateTokens(body)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg, kind string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": kind, "code": kind},
	})
}

func newID() string { return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()) }

// mappedFields 是已被 GenerateRequest 强类型字段承载、不必再进透传袋的键。
var mappedFields = map[string]bool{
	"model": true, "messages": true, "tools": true, "stream": true,
	"tool_choice": true, "temperature": true, "max_tokens": true,
	"reasoning_effort": true, "thinking": true,
	"conversation_id": true, "channel_config": true, "account_uid": true, "extra": true,
}

// passthroughFields 取 body 里未被强类型契约映射的剩余字段（浅拷贝，不改写原 body）。
func passthroughFields(body map[string]any) map[string]any {
	out := make(map[string]any, len(body))
	for k, v := range body {
		if !mappedFields[k] {
			out[k] = v
		}
	}
	return out
}

// asInt 宽容取整：JSON 解析产出 float64，而归一化回写（max_tokens 别名）产出 int64，
// 两条路径都必须读得到；json.Number 兜住改用 UseNumber 的未来改动。
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func asString(v any) string { s, _ := v.(string); return s }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// kindOf 统一提取错误分类（复用 main/protocol/rpc.KindOf，避免类型断言散落各处）。
func kindOf(err error) string {
	if k := rpc.KindOf(err); k != "" {
		return k
	}
	return "server"
}

func statusForKind(kind string) int {
	switch kind {
	case "auth":
		return http.StatusUnauthorized
	case "soft_rate":
		return http.StatusTooManyRequests
	case "bad_params", "prompt_too_long":
		return http.StatusBadRequest
	case "not_found", "model_blocked":
		return http.StatusNotFound
	case "server", "conn":
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

var _ = context.Background
var _ = jsonl.OpInvoke

// includeUsageOf 读客户端 stream_options.include_usage（缺省 true：多数客户端要 usage）。
// 显式 false 时上游用量仍会照常发（不影响计费与流水），只是不放进下游响应。
func includeUsageOf(body map[string]any) bool {
	so, ok := body["stream_options"].(map[string]any)
	if !ok {
		return true
	}
	if v, ok := so["include_usage"].(bool); ok {
		return v
	}
	return true
}

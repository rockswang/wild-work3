package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"wild-work3/main/protocol/chat"
)

// messages 实现 Anthropic Messages API（兼容层：Anthropic → Chat → 归一化 IPC）。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
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
	chatBody := anthropicToChat(body)
	// 入站归一化 + 图片降采样（Anthropic 的 tool_result 图片也在此转成 vision 通道）
	normalizeInbound(chatBody)
	req, err := toGenerateRequest(chatBody)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error(), "bad_params")
		return
	}
	stream, _ := body["stream"].(bool)
	msgID := "msg_" + shortNano()

	if !stream {
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
		if err != nil {
			anthErr(w, kindOf(err), err.Error())
			return
		}
		if fbModel != "" {
			w.Header().Set("X-Model-Fallback", req.Model+" -> "+fbModel) // 降级必留痕
		}
		writeJSON(w, buildAnthropicMessage(msgID, orDefault(fbModel, req.Model), acc, usage))
		return
	}

	// 流式：Anthropic 事件序列。header 惰性写入（降级头要赶在首帧前）
	sse := newSSE(w)
	emit := func(typ string, payload map[string]any) {
		payload["type"] = typ
		sse.frame(typ, payload)
	}
	// outModel 下游看到的模型名（降级后写备胎全路径）
	fbModel := ""
	outModel := func() string {
		if fbModel != "" {
			return fbModel
		}
		return req.Model
	}
	// message_start 也不能提前发：一发就把响应头锁定了，降级头就赶不上首帧。
	// 改为「首个真实事件到来时补发」，使 X-Model-Fallback 总能在流开始前落地。
	started := false
	ensureStart := func() {
		if started {
			return
		}
		started = true
		emit("message_start", map[string]any{"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": outModel(),
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		}})
	}
	blockIdx := 0
	inText := false
	inThink := false
	acc := &chat.Accumulator{}
	var usage *chat.Usage

	_, _, err = h.IPC.ChatStream(r.Context(), req, func(name string, data json.RawMessage) error {
		var f map[string]any
		_ = json.Unmarshal(data, &f)
		if name == "model.fallback" {
			fbModel = asString(f["to"])
			sse.setHeader("X-Model-Fallback", req.Model+" -> "+fbModel)
			return nil
		}
		acc.Add(name, f)
		if name == "text.delta" || name == "reasoning.delta" || name == "tool_call.delta" {
			ensureStart()
		}
		switch name {
		case "reasoning.delta":
			if !inThink {
				emit("content_block_start", map[string]any{"index": blockIdx, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
				inThink = true
			}
			emit("content_block_delta", map[string]any{"index": blockIdx, "delta": map[string]any{"type": "thinking_delta", "thinking": asString(f["content"])}})
		case "text.delta":
			if inThink && !inText {
				emit("content_block_stop", map[string]any{"index": blockIdx})
				blockIdx++
				inThink = false
			}
			if !inText {
				emit("content_block_start", map[string]any{"index": blockIdx, "content_block": map[string]any{"type": "text", "text": ""}})
				inText = true
			}
			emit("content_block_delta", map[string]any{"index": blockIdx, "delta": map[string]any{"type": "text_delta", "text": asString(f["content"])}})
		case "tool_call.delta":
			if inText {
				emit("content_block_stop", map[string]any{"index": blockIdx})
				blockIdx++
				inText = false
			}
			// 首片开 content_block（tool_use），后续只发 input_json_delta
			if v, ok := f["id"]; ok && asString(v) != "" {
				emit("content_block_start", map[string]any{
					"index":         blockIdx,
					"content_block": map[string]any{"type": "tool_use", "id": asString(f["id"]), "name": asString(f["name"]), "input": map[string]any{}},
				})
			}
			if frag := asString(f["arguments_fragment"]); frag != "" {
				emit("content_block_delta", map[string]any{
					"index": blockIdx, "delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
				})
			}
		case "usage":
			var u chat.Usage
			if json.Unmarshal(data, &u) == nil {
				usage = &u
			}
		}
		return nil
	})
	if err != nil {
		ensureStart()
		emit("error", map[string]any{"error": map[string]any{"type": kindOf(err), "message": err.Error()}})
		return
	}
	ensureStart()
	emit("content_block_stop", map[string]any{"index": blockIdx})
	emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": anthropicStop(acc.FinishReason), "stop_sequence": nil},
		"usage": anthropicUsage(usage),
	})
	emit("message_stop", map[string]any{})
}

// anthropicToChat 把 Anthropic Messages 请求转 Chat。
func anthropicToChat(body map[string]any) map[string]any {
	out := map[string]any{"model": body["model"]}
	var msgs []any
	// system：字符串或多块
	if sys, ok := body["system"]; ok && sys != nil {
		switch v := sys.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": "system", "content": v})
		case []any:
			var sb string
			for _, p := range v {
				if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
					sb += asString(pm["text"])
				}
			}
			if sb != "" {
				msgs = append(msgs, map[string]any{"role": "system", "content": sb})
			}
		}
	}
	if arr, ok := body["messages"].([]any); ok {
		for _, m := range arr {
			mm, _ := m.(map[string]any)
			if mm == nil {
				continue
			}
			msgs = append(msgs, convertAnthropicMessage(mm)...)
		}
	}
	out["messages"] = msgs
	if v, ok := body["temperature"]; ok {
		out["temperature"] = v
	}
	// Anthropic 必填 max_tokens → Chat max_tokens；缺省 4096（旧版语义）
	if v, ok := body["max_tokens"]; ok {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = 4096
	}
	// top_k / stop_sequences：旧版同样透传（不支持的上游会忽略未知字段）。
	// 漏接 stop_sequences 会把 Claude 客户端的停止串静默丢掉，模型越过停止点继续生成。
	if n, ok := asInt(body["top_k"]); ok && n > 0 {
		out["top_k"] = n
	}
	if ss, ok := body["stop_sequences"].([]any); ok && len(ss) > 0 {
		out["stop"] = ss
	}
	// metadata.user_id → Chat 的 user（旧版同口径；渠道插件按需在 Extra 透传袋里自取）
	if mm, ok := body["metadata"].(map[string]any); ok {
		if u := asString(mm["user_id"]); u != "" {
			out["user"] = u
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		var ts []any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if tm == nil {
				continue
			}
			fn := map[string]any{"name": tm["name"]}
			if d, ok := tm["description"]; ok {
				fn["description"] = d
			}
			if s, ok := tm["input_schema"]; ok {
				fn["parameters"] = s
			}
			ts = append(ts, map[string]any{"type": "function", "function": fn})
		}
		if len(ts) > 0 {
			out["tools"] = ts
		}
	}
	// 推理：thinking.budget_tokens → 内部 thinking 标记 + 保底 reasoning_effort
	// （旧版 dev/internal/gateway/anthropic.go:147-155 同口径：完整 thinking 对象本来透传给渠道，
	// 且在 type=enabled 时补 reasoning_effort=medium —— 渠道按 effort 启用思考时没有它就等于关思考）。
	if th, ok := body["thinking"].(map[string]any); ok && len(th) > 0 {
		out["thinking"] = true
		if th["budget_tokens"] != nil {
			out["thinking_budget_tokens"] = th["budget_tokens"]
		}
		if strings.EqualFold(asString(th["type"]), "enabled") {
			out["reasoning_effort"] = "medium"
		}
	}
	return out
}

// convertAnthropicMessage 把一条 Anthropic 消息转成 1..N 条 Chat 消息（tool_result 拆 role=tool）。
func convertAnthropicMessage(m map[string]any) []any {
	role := asString(m["role"])
	switch c := m["content"].(type) {
	case string:
		return []any{map[string]any{"role": role, "content": c}}
	case []any:
		var parts []any
		var out []any
		var tcs []any
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			switch pm["type"] {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": asString(pm["text"])})
			case "image":
				if src, ok := pm["source"].(map[string]any); ok {
					url := asString(src["url"])
					if url == "" && asString(src["type"]) == "base64" {
						url = "data:" + asString(src["media_type"]) + ";base64," + asString(src["data"])
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			case "tool_use":
				tcs = append(tcs, map[string]any{
					"id": pm["id"], "type": "function",
					"function": map[string]any{"name": pm["name"], "arguments": mustJSONStr(pm["input"])},
				})
			case "tool_result":
				// tool_result → 独立的 role=tool 消息。content 可能是纯文本，也可能是带图片的块
				// 数组：后者必须走 vision 通道（只取 text 块会丢图，stringify 会把 base64 变
				// 成超长文本），见 toolContentValue。
				out = append(out, map[string]any{
					"role": "tool", "tool_call_id": asString(pm["tool_use_id"]),
					"content": toolContentValue(pm["content"]),
				})
			case "thinking", "redacted_thinking":
				// 历史思考块不回放上游（旧版同口径）：上游不接受历史 thinking，对后续推理
				// 也没有信息增量。早先把它改写成 text 会把思考原文污染上下文。
			}
		}
		if len(tcs) > 0 {
			msg := map[string]any{"role": "assistant", "tool_calls": tcs}
			if len(parts) > 0 {
				msg["content"] = parts
			}
			out = append([]any{msg}, out...)
		} else if len(parts) > 0 {
			if len(parts) == 1 {
				if pm, ok := parts[0].(map[string]any); ok && pm["type"] == "text" {
					out = append([]any{map[string]any{"role": role, "content": pm["text"]}}, out...)
					return out
				}
			}
			out = append([]any{map[string]any{"role": role, "content": parts}}, out...)
		}
		return out
	}
	return nil
}

func anthropicToolResultText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				sb += asString(pm["text"])
			}
		}
		return sb
	}
	return mustJSONStr(v)
}

func buildAnthropicMessage(id, model string, acc *chat.Accumulator, u *chat.Usage) map[string]any {
	var content []any
	if acc.Reasoning.Len() > 0 {
		// signature 必填：Anthropic 官方 SDK 解析 thinking 块要求这个键存在（旧版同口径，
		// dev/internal/gateway/anthropic.go:326），否则客户端可能报块结构非法。
		content = append(content, map[string]any{"type": "thinking", "thinking": acc.Reasoning.String(), "signature": ""})
	}
	if acc.Text.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": acc.Text.String()})
	}
	for _, t := range acc.ToolCalls {
		var input any = map[string]any{}
		_ = json.Unmarshal([]byte(t.ArgumentsBuilder.String()), &input)
		content = append(content, map[string]any{"type": "tool_use", "id": t.ID, "name": t.Name, "input": input})
	}
	if content == nil {
		content = []any{}
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": anthropicStop(acc.FinishReason), "stop_sequence": nil,
		"usage": anthropicUsage(u),
	}
}

// anthropicUsage 互斥缓存语义：input_tokens = prompt − cached − write（旧版语义）。
func anthropicUsage(u *chat.Usage) map[string]any {
	if u == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	cached, write := 0, 0
	if u.Cache != nil {
		cached, write = u.Cache.Cached, u.Cache.Write
	}
	in := u.PromptTokens - cached - write
	if in < 0 {
		in = 0
	}
	out := map[string]any{
		"input_tokens": in, "output_tokens": u.CompletionTokens,
	}
	if cached > 0 {
		out["cache_read_input_tokens"] = cached
	}
	if write > 0 {
		out["cache_creation_input_tokens"] = write
	}
	return out
}

func anthropicStop(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop", "":
		return "end_turn"
	case "content_filter":
		return "refusal"
	}
	return reason
}

func anthErr(w http.ResponseWriter, kind, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusForKind(kind))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error", "error": map[string]any{"type": kind, "message": msg},
	})
}

func mustJSONStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var _ = time.Now

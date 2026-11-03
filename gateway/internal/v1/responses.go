package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wild-work3/main/protocol/chat"
)

// responses 实现 OpenAI Responses API（兼容层：Responses → Chat → 归一化 IPC）。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
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
	chatBody := responsesToChat(body)
	// 入站归一化 + 图片降采样（Responses 的 function_call_output 图片也在此转 vision 通道）
	normalizeInbound(chatBody)
	req, err := toGenerateRequest(chatBody)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error(), "bad_params")
		return
	}
	stream, _ := body["stream"].(bool)
	respID := "resp_" + shortNano()

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
			errJSON(w, statusForKind(kindOf(err)), err.Error(), kindOf(err))
			return
		}
		if fbModel != "" {
			w.Header().Set("X-Model-Fallback", req.Model+" -> "+fbModel) // 降级必留痕
		}
		writeJSON(w, buildResponsesResponse(respID, orDefault(fbModel, req.Model), acc, usage))
		return
	}

	// 流式：Responses 的事件序列（response.created / output_item.added / delta / completed）
	// header 惰性写入（降级头要赶在首帧前）
	sse := newSSE(w)
	seq := 0
	emit := func(typ string, payload map[string]any) {
		payload["type"] = typ
		payload["sequence_number"] = seq
		seq++
		sse.frame(typ, payload)
	}
	fbModel := ""
	outModel := func() string {
		if fbModel != "" {
			return fbModel
		}
		return req.Model
	}
	// created/output_item.added 改为首事件到来时补发（同 anthropic 的 reason）
	started := false
	ensureStart := func() {
		if started {
			return
		}
		started = true
		emit("response.created", map[string]any{"response": map[string]any{
			"id": respID, "object": "response", "created_at": time.Now().Unix(), "model": outModel(), "status": "in_progress",
		}})
		emit("response.output_item.added", map[string]any{
			"output_index": 0, "item": map[string]any{"id": "msg_" + respID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
		})
	}

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
		switch name {
		case "text.delta":
			ensureStart()
			emit("response.output_text.delta", map[string]any{
				"item_id": "msg_" + respID, "output_index": 0, "content_index": 0, "delta": asString(f["content"]),
			})
		case "reasoning.delta":
			ensureStart()
			emit("response.reasoning_summary_text.delta", map[string]any{
				"item_id": "rs_" + respID, "output_index": 0, "summary_index": 0, "delta": asString(f["content"]),
			})
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
		emit("response.failed", map[string]any{"response": map[string]any{
			"id": respID, "status": "failed", "error": map[string]any{"code": kindOf(err), "message": err.Error()},
		}})
		return
	}
	ensureStart()
	emit("response.output_text.done", map[string]any{
		"item_id": "msg_" + respID, "output_index": 0, "content_index": 0, "text": acc.Text.String(),
	})
	emit("response.output_item.done", map[string]any{
		"output_index": 0, "item": map[string]any{
			"id": "msg_" + respID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": acc.Text.String()}},
		},
	})
	final := buildResponsesResponse(respID, req.Model, acc, usage)
	emit("response.completed", map[string]any{"response": final})
}

// responsesToChat 把 Responses 请求转成 Chat 请求（响应体 → messages/tools）。
func responsesToChat(body map[string]any) map[string]any {
	out := map[string]any{"model": body["model"]}
	var msgs []any
	// instructions → system
	if ins, ok := body["instructions"].(string); ok && ins != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}
	// input：字符串或 item 数组
	switch v := body["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		msgs = append(msgs, convertResponsesItems(v)...)
	}
	out["messages"] = msgs
	if t, ok := body["temperature"]; ok {
		out["temperature"] = t
	}
	if m, ok := body["max_output_tokens"]; ok {
		out["max_tokens"] = m
	}
	if tools, ok := body["tools"].([]any); ok {
		var ts []any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if tm == nil || tm["type"] != "function" {
				continue
			}
			// Responses 的 function 工具字段是扁平的 → 转成 Chat 的嵌套形式
			fn := map[string]any{}
			for _, k := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := tm[k]; ok {
					fn[k] = v
				}
			}
			ts = append(ts, map[string]any{"type": "function", "function": fn})
		}
		if len(ts) > 0 {
			out["tools"] = ts
		}
	}
	if v, ok := body["tool_choice"]; ok {
		out["tool_choice"] = v
	}
	// 推理强度（reasoning.effort）透传
	if rs, ok := body["reasoning"].(map[string]any); ok {
		if e, ok := rs["effort"].(string); ok {
			out["reasoning_effort"] = e
		}
	}
	return out
}

// convertResponsesItems 把 Responses 的 input items 转成 Chat messages。
func convertResponsesItems(items []any) []any {
	var out []any
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "", "message":
			role, _ := m["role"].(string)
			if role == "" {
				role = "user"
			}
			switch c := m["content"].(type) {
			case string:
				out = append(out, map[string]any{"role": role, "content": c})
			case []any:
				var parts []any
				for _, p := range c {
					pm, _ := p.(map[string]any)
					if pm == nil {
						continue
					}
					switch pm["type"] {
					case "input_text", "output_text", "text", "refusal", "summary_text":
						// refusal/summary_text 也是正文载体（旧版 responses.go 同口径）：
						// 不认就会把客户端重放的文本静默丢掉。
						parts = append(parts, map[string]any{"type": "text", "text": asString(pm["text"])})
					case "input_image", "image_url", "image":
						// image_url 既可能是字符串，也可能是 {url,detail} 对象（Responses 两种形状都合法）；
						// 早先用 asString 取对象形态只会得到 "" → 图直接丢。统一走 imagePartURL。
						if u, detail := imagePartURL(pm); u != "" {
							parts = append(parts, imageURLBlock(u, detail))
						}
					}
				}
				if len(parts) == 1 {
					if pm, ok := parts[0].(map[string]any); ok && pm["type"] == "text" {
						out = append(out, map[string]any{"role": role, "content": pm["text"]})
						continue
					}
				}
				out = append(out, map[string]any{"role": role, "content": parts})
			}
		case "function_call":
			// 模型发起工具调用 → assistant.tool_calls
			tc := map[string]any{
				"id": m["call_id"], "type": "function",
				"function": map[string]any{"name": m["name"], "arguments": asString(m["arguments"])},
			}
			out = append(out, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{tc}})
		case "function_call_output":
			// 工具结果 → role:tool。output 可能是纯文本，也可能是 input_image 块数组：
			// 后者必须走 vision 通道（同图约 1K token），直接 stringify 会把整张 PNG 的
			// base64 变成一条超长文本消息（实测 1.6MB base64 ≈ 146 万 token）。
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": m["call_id"], "content": toolContentValue(m["output"]),
			})
		}
	}
	return out
}

// buildResponsesResponse 构造 Responses 响应体。
func buildResponsesResponse(id, model string, acc *chat.Accumulator, u *chat.Usage) map[string]any {
	var output []any
	if acc.Text.Len() > 0 {
		output = append(output, map[string]any{
			"id": "msg_" + id, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": acc.Text.String(), "annotations": []any{}}},
		})
	}
	for i, t := range acc.ToolCalls {
		output = append(output, map[string]any{
			"id": fmt.Sprintf("fc_%s_%d", id, i), "type": "function_call", "status": "completed",
			"call_id": t.ID, "name": t.Name, "arguments": t.ArgumentsBuilder.String(),
		})
	}
	resp := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "model": model,
		"status": "completed", "output": output,
	}
	if u != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  u.PromptTokens,
			"output_tokens": u.CompletionTokens,
			"total_tokens":  u.TotalTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens": cacheCached(u),
			},
		}
	}
	return resp
}

func cacheCached(u *chat.Usage) int {
	if u != nil && u.Cache != nil {
		return u.Cache.Cached
	}
	return 0
}

func shortNano() string {
	return strings.TrimSpace(fmt.Sprintf("%d", time.Now().UnixNano()%1e9))
}

// tokens.go 输入 token 估算（count_tokens 兼容端点用）。
//
// 沿用旧版 dev/internal/gateway/anthropic_stream.go:estimateAnthropicTokens 的口径：
// 遍历 system / messages / tools 的所有文本，CJK（r > 0x2E80）按 1 字 1 token、
// 其余按 4 字节 1 token，末尾 +1；再按内容块数给下限（每块至少 2 token），避免
// 极短多块请求估成 1。
package v1

import "encoding/json"

// estimateTokens 估算一份 Anthropic / Chat 形态请求体的输入 token 数。
func estimateTokens(body map[string]any) int {
	var cjk, other, blocks int
	var add func(any) // 递归引用自身，必须先用 var 声明再赋值
	add = func(v any) {
		switch t := v.(type) {
		case string:
			for _, r := range t {
				if r > 0x2E80 {
					cjk++
				} else {
					other++
				}
			}
		case []any:
			for _, e := range t {
				add(e)
			}
		case map[string]any:
			// 只数承载正文的键，避免把 id/type/role 等结构字段算进正文长度。
			// 键表 = 旧版（text/content/input/system/tools/name/description）∪
			// ww3 新增块型（thinking/input_text/output_text/summary_text）：
			// 旧版计入 tool_use.input 与 function 的 name/description，早先漏了会导致
			// 带大 schema 的工具请求明显低估。
			for _, k := range []string{"text", "content", "input", "system", "tools", "name", "description",
				"thinking", "input_text", "output_text", "summary_text"} {
				if v, ok := t[k]; ok {
					add(v)
				}
			}
			if typ := asString(t["type"]); typ == "text" || typ == "tool_result" || typ == "input_text" || typ == "output_text" {
				blocks++
			}
		}
	}
	if v, ok := body["system"]; ok {
		add(v)
	}
	if v, ok := body["messages"]; ok {
		add(v)
	}
	if v, ok := body["tools"]; ok {
		if b, err := json.Marshal(v); err == nil {
			// tools 的 JSON Schema 是纯文本，按字符数直接计入 other
			for _, r := range string(b) {
				if r > 0x2E80 {
					cjk++
				} else {
					other++
				}
			}
		}
	}
	n := cjk + other/4 + 1
	if min := blocks * 2; n < min {
		n = min
	}
	// 每条消息至少有角色/结构开销（旧版 anthropic_stream.go 同口径下限）
	if msgs, ok := body["messages"].([]any); ok {
		if min := len(msgs) * 2; n < min {
			n = min
		}
	}
	return n
}

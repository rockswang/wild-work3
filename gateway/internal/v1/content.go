// content.go tool 结果内容的形态转换（Responses function_call_output / Anthropic tool_result）。
//
// 为什么不能直接 stringify：Codex 的 view_image 把截图以 input_image 块放进
// function_call_output。旧实现取不到文本后回退 json.Marshal，整张 PNG 的 base64 就变成
// 一条超长**文本**消息——实测 1.6MB base64 ≈ 146 万 token，把 1M 上下文的模型直接打成
// `prompt is too long`。图片必须走 vision 通道（同图约 1K token），绝不能被序列化成文本。
package v1

import (
	"encoding/json"
	"strings"
)

// toolContentValue 转换 tool 结果的内容，返回 string（纯文本）或 []any（含图片时的内容块数组）。
func toolContentValue(v any) any {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var (
			texts  []string
			blocks []any
			hasImg bool
		)
		for _, p := range t {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(asString(part["type"])) {
			case "input_text", "output_text", "text", "refusal", "summary_text":
				if s := asString(part["text"]); s != "" {
					texts = append(texts, s)
					blocks = append(blocks, map[string]any{"type": "text", "text": s})
				}
			case "input_image", "image_url", "image":
				url, detail := imagePartURL(part)
				if url == "" {
					continue
				}
				hasImg = true
				blocks = append(blocks, imageURLBlock(url, detail))
			}
		}
		if hasImg {
			// 补一条文本占位：部分渠道（如 codearts）会丢弃非 text 块，若数组里只有图片，
			// 剥离后会变成空 content（上游 400）。同时让不支持 vision 的模型至少知道
			// 「这里曾有一张图」。
			if len(texts) == 0 {
				blocks = append([]any{map[string]any{"type": "text", "text": "[image]"}}, blocks...)
			}
			return blocks
		}
		if s := strings.Join(texts, "\n"); strings.TrimSpace(s) != "" {
			return s
		}
	}
	// 非文本/非数组（或数组里没有可用文本）：保持 JSON 字面量兜底。
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return ""
}

// imagePartURL 从 Responses / Anthropic 两种图片块形态里取 URL 与 detail：
//   - Responses: {"type":"input_image","image_url":"data:..."} 或 image_url 为 {url,detail}
//   - 通用:      {"type":"image_url","image_url":{"url":...}}
//   - Anthropic: {"type":"image","source":{"type":"base64","media_type":...,"data":...}}
func imagePartURL(part map[string]any) (url, detail string) {
	switch u := part["image_url"].(type) {
	case string:
		url = u
	case map[string]any:
		url = asString(u["url"])
		detail = asString(u["detail"])
	}
	if url == "" {
		url = asString(part["url"])
	}
	if url == "" {
		url = anthropicImageURL(part) // {"type":"image","source":{...}} → data URL
	}
	if detail == "" {
		detail = asString(part["detail"])
	}
	return url, detail
}

// anthropicImageURL 从 Anthropic image 块构造 data URL；非 base64/url 形态返回空。
func anthropicImageURL(block map[string]any) string {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return ""
	}
	if u := asString(src["url"]); u != "" {
		return u
	}
	if data := asString(src["data"]); data != "" {
		media := asString(src["media_type"])
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + data
	}
	return ""
}

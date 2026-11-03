// inbound.go 对外三协议共用的入站防护与归一化入口。
//
// 为什么在 gateway 做（W3-13 分层）：请求体上限、图片降采样、tool 结果图片转 vision
// 通道都属「面向下游的协议转换」，归 gateway；core 只认归一化后的 IPC 请求。
package v1

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"wild-work3/main/protocol/chat"
)

// maxRequestBody 请求体上限（旧版 DefaultMaxRequestBody 同值，AGENTS §2-I）。
// Codex 等无状态客户端每轮重放整段历史，内联截图会让 body 轻易上 30MiB。
const maxRequestBody = 128 << 20

// errBodyTooLarge 请求体超限（映射 413）。
var errBodyTooLarge = errors.New("请求体超过上限")

// decodeBody 读取并解析请求体，带大小上限；超限返回 errBodyTooLarge。
func decodeBody(w http.ResponseWriter, r *http.Request, dst *map[string]any) error {
	lr := http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(lr)
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		// io.EOF 之外的错误一律按解析失败处理（空 body 也走这里，与旧行为一致）
		if errors.Is(err, io.EOF) {
			return errors.New("请求体为空")
		}
		return err
	}
	return nil
}

// normalizeInbound 对下游 wire 形态的 chat 请求体做归一化 + 图片降采样。
//
// 归一化（developer→system、max_completion_tokens 翻译、tool_choice 归一、tool 配对、
// $ref 内联、指纹脱敏）是纯函数，见 main/protocol/chat/normalize.go；随后跑一遍
// 规则引擎的 pre-route 阶段（channel 无关规则；channel 相关规则由 core 在每个候选渠道调用前
// 用 post-route 阶段施加，见 W3-35）。
// 图片降采样只作用于内联 data URL（http(s) 直链不下载不改写），见 imageslim.go。
func normalizeInbound(body map[string]any) {
	chat.NormalizeBody(body)
	slimBodyImages(body)
	ApplyPreRules(body)
}

// ApplyPreRules 施加 channel 无关的入站规则（Pass A）。
//
// 规则来自统一配置 config.json 的 rules 段（默认规则内嵌在 main/rules）。
// 先归一化再改规则：规则看到的是标准 chat 形态，不必关心三协议差异。
// rules 为 nil（配置加载失败）时跳过——入站归一化本身仍生效，不阻断请求。
func ApplyPreRules(body map[string]any) {
	if r := Rules(); r != nil {
		r.PreRoute(body)
	}
}

// slimBodyImages 对 messages / tools 里出现的图片块做降采样（就地改写）。
//
// 只处理 `data:image/...;base64,` 形态；小图与直链原样放行（见 slimDataURLImage）。
func slimBodyImages(body map[string]any) {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if c, ok := msg["content"].([]any); ok {
			slimParts(c)
		}
	}
}

// slimParts 遍历 content part 数组，把 image 类 part 的 url 就地瘦身。
func slimParts(parts []any) {
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch asString(part["type"]) {
		case "image_url":
			switch u := part["image_url"].(type) {
			case string:
				part["image_url"] = slimDataURLImage(u)
			case map[string]any:
				if s := asString(u["url"]); s != "" {
					u["url"] = slimDataURLImage(s)
				}
			}
		case "input_image":
			// Responses 形态：image_url 可能是字符串或 {url,detail}
			switch u := part["image_url"].(type) {
			case string:
				part["image_url"] = slimDataURLImage(u)
			case map[string]any:
				if s := asString(u["url"]); s != "" {
					u["url"] = slimDataURLImage(s)
				}
			}
		}
	}
}

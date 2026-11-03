package channel

// 上游 /models 的宽容能力探测（W3-32 迁移旧版 dev/internal/custom/probe.go）。
//
// 为什么不能「就认 OpenAI 官方形状」：官方 /models 只返回 {id, object, created, owned_by}，
// 而第三方网关大量私扩字段（context_window / reasoning_efforts / capabilities.reasoning…），
// 用户想要的「能拿到能力就一并拉」只能靠别名表 + 宽容匹配实现。
//
// 口径红线（与旧版一致）：**解析不到就是 nil，绝不编造**。
// 尤其档位表：上游没给合法列表 → Efforts 留 nil（面板不显示下拉框），
// 不能因为「看起来像支持思考的模型」就塞一份 low/medium/high。

import (
	"encoding/json"
	"strconv"
	"strings"

	"wild-work3/main/protocol/meta"
)

// 上游字段别名表（取值为点分路径，支持嵌套对象；顺序 = 优先级，越靠前越权威，
// 避免 properties/ 里的猜测覆盖顶层官方声明）。
var (
	probeCtxKeys    = []string{"context_window", "context_length", "max_input_tokens", "architecture.input_context", "top_provider.context_length", "properties.context_window"}
	probeOutKeys    = []string{"max_tokens", "max_completion_tokens", "output_context", "architecture.output_context", "top_provider.max_completion_tokens", "properties.max_tokens"}
	probeImagesKeys = []string{"supports_images", "vision", "capabilities.image_input", "supports_vision"}
	probeToolsKeys  = []string{"supports_tools", "function_calling", "capabilities.tools"}
	probeRsnKeys    = []string{"supports_reasoning", "capabilities.reasoning"}
	probeEffortKeys = []string{"reasoning_efforts", "supported_reasoning_levels", "supported_reasoning_efforts", "capabilities.reasoning_efforts"}
	probeDefEffKeys = []string{"default_reasoning_effort", "reasoning.default_effort", "supported_reasoning_level"}
	// 参数/模态列表类：值本身是数组或 "text+image->text" 拼接串，靠子串匹配。
	probeListKeys = []string{"supported_parameters", "supported_features", "capabilities", "architecture.input_modalities", "features", "tags"}
	probeNameKeys = []string{"name", "display_name", "title", "alias"}
	probeIDKeys   = []string{"id", "model", "model_id", "slug"}
)

// parseModelInfos 从 /models 响应体里尽力提取模型信息（含档位）。
// 解析不到任何能力字段的条目仍会返回（ID 非空即可）——那只是「上游没声明」，不是失败。
func parseModelInfos(raw []byte) ([]meta.ModelInfo, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	list := modelList(v)
	out := make([]meta.ModelInfo, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue // 数组里混进字符串等异常形状：跳过而不是让整次探测失败
		}
		id := strings.TrimSpace(firstString(m, probeIDKeys))
		if id == "" {
			continue
		}
		mi := meta.ModelInfo{ID: id, Name: firstString(m, probeNameKeys)}
		if mi.Name == "" {
			mi.Name = id
		}
		if n := firstTokens(m, probeCtxKeys); n > 0 {
			c := int(n)
			mi.Context = &c
		}
		// 输出上限：上游声明了才填，否则留 nil（不编造）。probeOutKeys 混用
		// max_tokens 与 max_completion_tokens（OpenRouter 嵌套形态亦有别名）。
		if n := firstTokens(m, probeOutKeys); n > 0 {
			mt := int(n)
			mi.MaxTokens = &mt
		}
		// 档位：先清洗，再校验默认值必须落在表内（上游自相矛盾时宁可不写默认值，
		// 也不能拿它去覆盖一个有效档位）。
		efforts := meta.CleanEfforts(effortList(m, probeEffortKeys))
		def := strings.ToLower(strings.TrimSpace(firstString(m, probeDefEffKeys)))
		if def != "" && !containsFold(efforts, def) {
			def = ""
		}
		mi.Efforts, mi.DefaultEffort = efforts, def
		mi.Capabilities = meta.Capabilities2{
			Tools:     anyCapability(m, probeToolsKeys, probeListKeys, "tools", "tool_calls", "function_call", "function_calling"),
			Vision:    anyCapability(m, probeImagesKeys, probeListKeys, "image", "vision"),
			Reasoning: anyCapability(m, probeRsnKeys, probeListKeys, "reasoning", "thinking", "think") || len(efforts) > 0 || def != "",
		}
		out = append(out, mi)
	}
	return out, nil
}

// modelList 取出模型数组：`{"data":[...]}` / `{"models":[...]}` / `[...]` 三种外层形态都吃。
func modelList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		for _, k := range []string{"data", "models", "items", "result"} {
			switch sub := t[k].(type) {
			case []any:
				return sub
			case map[string]any: // 再套一层（如 {"result":{"data":[...]}}）
				if l := modelList(sub); len(l) > 0 {
					return l
				}
			}
		}
	}
	return nil
}

// dig 按点分路径取嵌套字段（"architecture.input_context"）。
func dig(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func firstString(m map[string]any, keys []string) string {
	for _, k := range keys {
		if v, ok := dig(m, k); ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// firstTokens 按别名顺序取第一个能解析成 token 数的字段（数字或 "1M"/"64k" 带单位串）。
func firstTokens(m map[string]any, keys []string) int64 {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok || v == nil {
			continue
		}
		if n, err := parseTokens(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// parseTokens 把数字或带单位串（"1M"/"64k"/"128000"）解析成 token 数。
func parseTokens(v any) (int64, error) {
	switch val := v.(type) {
	case float64:
		return int64(val), nil
	case string:
		s := strings.TrimSpace(strings.ToUpper(val))
		if s == "" {
			return 0, nil
		}
		mult := int64(1)
		if strings.HasSuffix(s, "M") {
			mult, s = 1000000, strings.TrimSuffix(s, "M")
		} else if strings.HasSuffix(s, "K") {
			mult, s = 1000, strings.TrimSuffix(s, "K")
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, err
		}
		return int64(f * float64(mult)), nil
	}
	return 0, strconv.ErrSyntax
}

// capabilityBlob 把 listKeys 里的数组/字符串/布尔字段拼成一段小写文本供子串匹配。
// 只取字符串元素与 true 的布尔键名——false 不参与匹配，
// 否则 "supports_images":false 会被子串命中而误判成「支持识图」。
func capabilityBlob(m map[string]any, listKeys []string) string {
	var sb strings.Builder
	for _, k := range listKeys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			sb.WriteString(strings.ToLower(t))
			sb.WriteByte(' ')
		case []any:
			for _, e := range t {
				if s, ok := e.(string); ok {
					sb.WriteString(strings.ToLower(s))
					sb.WriteByte(' ')
				}
			}
		case map[string]any:
			for name, val := range t {
				if b, ok := val.(bool); ok && b {
					sb.WriteString(strings.ToLower(name))
					sb.WriteByte(' ')
				}
			}
		}
	}
	return sb.String()
}

// boolAt 取显式布尔声明（true/false 都算「声明」，非布尔 = 未声明）。
func boolAt(m map[string]any, keys []string) (bool, bool) {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b, true
		case string:
			if parsed, err := strconv.ParseBool(strings.TrimSpace(b)); err == nil {
				return parsed, true
			}
		}
	}
	return false, false
}

// anyCapability 判「是否为真」：显式布尔为真，或列表字段里出现任一标记子串。
func anyCapability(m map[string]any, boolKeys, listKeys []string, markers ...string) bool {
	if b, ok := boolAt(m, boolKeys); ok {
		return b
	}
	blob := capabilityBlob(m, listKeys)
	for _, mk := range markers {
		if strings.Contains(blob, strings.ToLower(mk)) {
			return true
		}
	}
	return false
}

// effortList 取档位表：既接受 ["low","high"]，也接受 [{"name":"Low"},{"level":"high"}]。
// 统一小写——全仓档位取值都是 lowercase（gemini TierLow / 出站注入都是小写），
// 上游写 "Low" 也得归一，否则前端勾上后原样发出会被上游拒。
func effortList(m map[string]any, keys []string) []string {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		var out []string
		for _, e := range arr {
			switch t := e.(type) {
			case string:
				out = append(out, strings.ToLower(strings.TrimSpace(t)))
			case map[string]any:
				for _, kk := range []string{"type", "level", "value", "name", "id"} {
					if s, ok := t[kk].(string); ok && s != "" {
						out = append(out, strings.ToLower(s))
						break
					}
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func containsFold(list []string, s string) bool {
	for _, e := range list {
		if strings.EqualFold(e, s) {
			return true
		}
	}
	return false
}

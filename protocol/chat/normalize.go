// normalize.go 出站请求体归一化管线（纯函数，无 I/O、无状态）。
//
// 为什么是纯函数而不是拦截器插件（AGENTS §2-E / 决策记录）：
//   - 归一化是「必须永远生效」的协议转换，不是可选策略。hook 属请求变换类、默认
//     fail-closed，插件进程启动失败会中断全部对话——把必生效的逻辑放到会失败的地方
//     是错的。
//   - 零 I/O 零状态，每请求多一次 spawn + JSONL 往返毫无收益。
//   - `stream`/`stream_options`/`max_completion_tokens` 属协议转换，不属拦截。
//
// 管线顺序（沿用旧版 dev/internal/upstream/payload.go 的实测顺序）：
//  1. max_completion_tokens → max_tokens（显式 max_tokens 优先，别名只删不译）
//  2. tool_choice 归一（上游该字段是 string，对象形态会 400）
//  3. developer → system（上游 role 白名单不含 developer）
//  4. tool 配对：先 repack 重排，再 cleanup 删孤儿（见 tool_pairing.go）
//  5. tool schema $ref 全量内联（Kimi/Moonshot 系对任何 $ref 都 4027）
//  6. 出站脱敏（剥离上游内容审核黑名单指纹，见 sanitize.go）
//
// 有意**不在本函数内**做的两步（各有归属，别在此处重复实现）：
//   - `stream=true`：内部对话数据面只支持流式（W3-13 #9），由调用方在构造
//     GenerateRequest 时强制为 true（gateway `toGenerateRequest`），此处无 wire 字段可改。
//   - `stream_options={include_usage:true}`：旧版在 PrepareBody 集中注入，ww3 分层后
//     这属于「上游协议细节」，由各渠道插件自己拼 body 时注入（官方样板 custom-api-ext
//     与 11 家独立仓插件均已实现：workbuddy/qwenwork/lobsterai/traework/raccoon/kilo/
//     cline/oczen/loomy/officeace/codearts）。
//     注意插件应仅在客户端未显式给时才补（旧版 D7 口径），否则会覆盖 `include_usage:false`；
//     插件不遵守时 gateway 还会在最后一公里再守一次（`includeUsageOf`）。
package chat

import (
	"encoding/json"
	"strings"
)

// NormalizeBody 就地归一化一份 OpenAI wire 形态的请求体（map[string]any）。
// 无法识别的形态一律原样保留（容忍畸形输入，不制造新故障）。返回是否发生改动。
func NormalizeBody(obj map[string]any) bool {
	if len(obj) == 0 {
		return false
	}
	changed := false
	if translateMaxCompletionTokens(obj) {
		changed = true
	}
	if normalizeToolChoice(obj) {
		changed = true
	}
	if normalizeRoles(obj) {
		changed = true
	}
	if msgs, ok := obj["messages"].([]any); ok {
		// 先重排再清理：插在 tool 结果中间的非 tool 消息（Codex image_resize_notice）
		// 同样判配对断裂，先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
		msgs, c1 := repackToolResultBlocks(msgs)
		msgs, c2 := cleanupOrphanToolCalls(msgs)
		// 任一步重排/删除（哪怕后续步骤零改动）也必须回写——不能只在「最后一步改动」
		// 时回写，否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
		if c1 || c2 {
			changed = true
		}
	}
	if inlineToolSchemaRefs(obj) {
		changed = true
	}
	if msgs, ok := obj["messages"].([]any); ok {
		// 脱敏放在最后：所有结构改写都完成后，再对最终文本剥离指纹。
		if sanitizeMessages(msgs) {
			changed = true
		}
	}
	return changed
}

// NormalizeRoles 把 messages 里的 developer 角色归一为 system（导出给需要单独调用的渠道）。
//
// 背景：上游对 role 做白名单校验，developer 不在白名单内，命中即 HTTP 400 code=11-128。
// developer 是 OpenAI 新规范里 system 的别名，改写不丢语义。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为未实测，合并会引入新变量）。
func NormalizeRoles(obj map[string]any) { normalizeRoles(obj) }

// SanitizeText applies the legacy configurable text sanitization to one string.
// New integrations should prefer main/rules.Defaults; this export supports staged migration.
func SanitizeText(text string) string { return sanitizeText(text) }

// SanitizeMessages applies text sanitization to message content, reasoning, and tool arguments.
// New integrations should prefer main/rules.Defaults; this export supports staged migration.
func SanitizeMessages(messages []any) bool { return sanitizeMessages(messages) }

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为 max_tokens。
//
// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段（o-series 起），
// 新客户端只发别名；多数上游只认 max_tokens，别名透传会被忽略后回落默认输出上限，
// 长流任务被截。规则：显式 max_tokens 优先（别名只删不译）；别名非正数值（0/null/负数）
// 不翻译（0/null 语义是「未设置」，负数是非法值）；非数值别名不翻译。
func translateMaxCompletionTokens(obj map[string]any) bool {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（上游未知字段宽松，留着徒增体积）
	if !has {
		return false
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return true // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免科学计数法/小数尾巴进上游 body）；
	// 其他数值类型防御性兼容（手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
	return true
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system；返回是否改动。
func normalizeRoles(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			changed = true
		}
	}
	return changed
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice；返回是否改动。
//   - "none"                                      → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}                             → 同上
//   - {"type":"auto"/"required"}                  → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"（name 为空回 "auto"）
//   - 其他对象/非标量                              → 删 tool_choice
func normalizeToolChoice(obj map[string]any) bool {
	tc, present := obj["tool_choice"]
	if !present {
		return false
	}
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
		return true
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
		return true
	}
	delete(obj, "tool_choice")
	return true
}

// inlineToolSchemaRefs 把工具 schema（tools[].function.parameters 与顶层 functions[].parameters）
// 里的本地 $ref 全量内联；返回是否改动。
//
// Kimi/Moonshot 系后端对 schema 中任何 $ref 都返回 4027 "detected infinite recursion
// without termination condition"（保留非环 $ref 亦然），故不按模型名门控、统一内联；
// 对容忍递归的后端，展开后的有限 schema 依旧合法。语义损失仅限递归分支处（截断为
// 空 schema = 接受任意值）。
//
// 无 $ref 的 schema 一律不动原文：回写 Marshal 结果会重排键序、改写数字字面量，
// 白白污染上游 prompt 缓存前缀。
func inlineToolSchemaRefs(obj map[string]any) bool {
	changed := false
	if list, ok := obj["tools"].([]any); ok {
		for _, item := range list {
			t, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// OpenAI tools 的 schema 挂在 function 子对象下
			if fn, ok := t["function"].(map[string]any); ok {
				if inlineParams(fn) {
					changed = true
				}
			}
		}
	}
	// 早期 Chat 规范的顶层 `functions[]`（无 function 包裹层）同样必须消毒：
	// 漏掉它等于给走该形态的请求留一条 $ref 直通上游的路（Kimi/Moonshot 必 4027）。
	if list, ok := obj["functions"].([]any); ok {
		for _, item := range list {
			if fn, ok := item.(map[string]any); ok && inlineParams(fn) {
				changed = true
			}
		}
	}
	return changed
}

// inlineParams 就地内联 holder["parameters"] 里的本地 $ref（对象与预序列化字符串两种形态）。
func inlineParams(holder map[string]any) bool {
	params, present := holder["parameters"]
	if !present {
		return false
	}
	switch m := params.(type) {
	case map[string]any:
		if countRefs(m) == 0 {
			return false
		}
		holder["parameters"] = coerceObject(inlineSchemaRefs(m, m, map[string]bool{}, newInlineBudget()))
		return true
	case string:
		// parameters 预序列化为字符串的形态：原样透传会让消毒被绕过。
		var pm map[string]any
		if err := json.Unmarshal([]byte(m), &pm); err != nil || pm == nil {
			return false
		}
		if countRefs(pm) == 0 {
			return false
		}
		cleaned := coerceObject(inlineSchemaRefs(pm, pm, map[string]bool{}, newInlineBudget()))
		if b, err := json.Marshal(cleaned); err == nil {
			holder["parameters"] = string(b)
			return true
		}
	}
	return false
}

// inlineBudget 限制单个 tool 的 $ref 内联展开次数，防止菱形引用组合爆炸。
const inlineBudget = 4096

func newInlineBudget() *int { b := inlineBudget; return &b }

// coerceObject 保证消毒结果为 object schema（parameters 约定顶层是对象）；
// 内联目标若是标量/布尔 schema 等非常规形态，回落为空 schema。
func coerceObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// inlineSchemaRefs 返回 node 的消毒副本：所有 $ref 就地内联（以目标内容为基底继续内联，
// $ref 的兄弟键按 draft-07 语义丢弃），chain 记录当前展开链上已入栈的 $ref 目标指针
// （分叉时复制，互不污染），链上重现即递归环——截断为空 schema 终止。root 为整个
// parameters 文档（$ref 解析基准）。budget 防组合爆炸，耗尽后剩余 $ref 一律截断。不改写入参。
func inlineSchemaRefs(node, root any, chain map[string]bool, budget *int) any {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if ptr, isLocal := localRefPtr(ref); isLocal && ptr != "" && !chain[ptr] {
				if target := resolveLocalRef(root, ptr); target != nil && *budget > 0 {
					*budget--
					child := make(map[string]bool, len(chain)+1)
					for k := range chain {
						child[k] = true
					}
					child[ptr] = true
					return inlineSchemaRefs(target, root, child, budget)
				}
			}
			return map[string]any{} // "#" 必环 / 展开链成环 / 非本地 / 不可达 / 预算耗尽
		}
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = inlineSchemaRefs(v, root, chain, budget)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = inlineSchemaRefs(v, root, chain, budget)
		}
		return out
	}
	return node
}

// countRefs 统计 schema 子树中 $ref 键的个数（预检用）。
func countRefs(node any) int {
	n := 0
	switch t := node.(type) {
	case map[string]any:
		if _, ok := t["$ref"]; ok {
			n++
		}
		for k, v := range t {
			if k == "$ref" {
				continue
			}
			n += countRefs(v)
		}
	case []any:
		for _, v := range t {
			n += countRefs(v)
		}
	}
	return n
}

// localRefPtr 解析本地 $ref："#/a/b" → "/a/b"；"#" → ""（必环）；非本地引用
// （外链 / 非 JSON Pointer 形态）返回 false 交由内联层截断。
func localRefPtr(ref string) (string, bool) {
	r := strings.TrimSpace(ref)
	if !strings.HasPrefix(r, "#") {
		return "", false
	}
	p := strings.TrimPrefix(r, "#")
	if p == "" {
		return "", true
	}
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

// resolveLocalRef 按 JSON Pointer（RFC 6901）在文档内取目标节点；不可达返回 nil。
func resolveLocalRef(root any, ptr string) any {
	cur := root
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			nxt, ok := c[seg]
			if !ok {
				return nil
			}
			cur = nxt
		case []any:
			i, err := json.Number(seg).Int64()
			if err != nil || i < 0 || int(i) >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
	}
	return cur
}

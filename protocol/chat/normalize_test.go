package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, s)
	}
	return m
}

// developer 角色归一成 system（旧版 normalizeRoles：只认 developer，不合并不重排）。
func TestNormalizeRolesDeveloper(t *testing.T) {
	obj := parse(t, `{"messages":[{"role":"Developer","content":"x"},{"role":"user","content":"y"}]}`)
	NormalizeBody(obj)
	msgs := obj["messages"].([]any)
	if r := msgs[0].(map[string]any)["role"]; r != "system" {
		t.Fatalf("Developer 应归一为 system，实际 %v", r)
	}
	if r := msgs[1].(map[string]any)["role"]; r != "user" {
		t.Fatalf("user 不应被改动，实际 %v", r)
	}
}

// max_completion_tokens 翻译成 max_tokens 并删除别名。
func TestNormalizeMaxCompletionTokens(t *testing.T) {
	obj := parse(t, `{"max_completion_tokens":1024}`)
	NormalizeBody(obj)
	if v, ok := obj["max_tokens"]; !ok || v.(int64) != 1024 {
		t.Fatalf("应翻译成 max_tokens=1024，实际 %v", obj["max_tokens"])
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Fatal("别名必须删除")
	}
	// 显式 max_tokens 优先：别名只删不译
	obj = parse(t, `{"max_tokens":512,"max_completion_tokens":1024}`)
	NormalizeBody(obj)
	if obj["max_tokens"].(float64) != 512 {
		t.Fatalf("已有 max_tokens 时不应被覆盖，实际 %v", obj["max_tokens"])
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Fatal("别名必须删除")
	}
}

// tool_choice:"none" 同时删掉 tools（上游见到 tools 会忽略 none）。
func TestNormalizeToolChoiceNone(t *testing.T) {
	obj := parse(t, `{"tool_choice":"none","tools":[{"type":"function","function":{"name":"f"}}]}`)
	NormalizeBody(obj)
	if _, ok := obj["tool_choice"]; ok {
		t.Fatal("tool_choice 应被删除")
	}
	if _, ok := obj["tools"]; ok {
		t.Fatal("tools 应一并删除")
	}
	// {"type":"function","function":{"name":"f"}} 降为字符串 "f"
	obj = parse(t, `{"tool_choice":{"type":"function","function":{"name":"f"}}}`)
	NormalizeBody(obj)
	if obj["tool_choice"] != "f" {
		t.Fatalf("应降为字符串 f，实际 %v", obj["tool_choice"])
	}
}

// 孤儿 tool 调用与无主 tool 结果必须成对裁剪（上游 11148）。
func TestNormalizeToolPairing(t *testing.T) {
	obj := parse(t, `{"messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"a","content":"ok"}
	]}`)
	NormalizeBody(obj)
	msgs := obj["messages"].([]any)
	asst := msgs[0].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("孤儿 tool_call b 应被裁掉，实际剩 %d 个", len(tcs))
	}
	if id := tcs[0].(map[string]any)["id"]; id != "a" {
		t.Fatalf("应保留 a，实际 %v", id)
	}
}

// tools 里的本地 $ref 必须内联（Kimi 等上游不支持 $ref）。
func TestNormalizeInlineRefs(t *testing.T) {
	obj := parse(t, `{"tools":[{"type":"function","function":{"name":"f","parameters":{
		"type":"object",
		"properties":{"a":{"$ref":"#/definitions/Item"}},
		"definitions":{"Item":{"type":"object","properties":{"x":{"type":"string"}}}}
	}}}]}`)
	NormalizeBody(obj)
	params := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	prop := params["properties"].(map[string]any)["a"].(map[string]any)
	if _, ok := prop["$ref"]; ok {
		t.Fatalf("$ref 应被内联，实际仍有: %v", prop)
	}
	if prop["type"] != "object" {
		t.Fatalf("内联后应是原定义内容，实际 %v", prop)
	}
}

// 自引用（成环）必须安全终止，不能栈溢出。
func TestNormalizeInlineRefsCycle(t *testing.T) {
	obj := parse(t, `{"tools":[{"type":"function","function":{"name":"f","parameters":{
		"type":"object","properties":{"a":{"$ref":"#/definitions/Node"}},
		"definitions":{"Node":{"type":"object","properties":{"next":{"$ref":"#/definitions/Node"}}}}
	}}}]}`)
	NormalizeBody(obj) // 不 panic 即通过
	if !strings.Contains(string(mustMarshal(t, obj)), "properties") {
		t.Fatal("成环后应保留可用结构")
	}
}

// 出站脱敏：Anthropic 计费头等指纹必须被清掉。
//
// 字面量一律取自 sanitize.go 自己的特征/改写表，不在此重复抄写——
// 手抄长串指纹极易漏掉一个连字符或冠词，测试就变成"验证我抄的东西"而非实现。
func TestNormalizeSanitize(t *testing.T) {
	key := sanitizeFeatures[0] // x-anthropic-billing-hdr
	body := `{"messages":[{"role":"user","content":"hello ` + key + `: abc123; cc_foo=bar; world"}]}`
	obj := parse(t, body)
	NormalizeBody(obj)
	got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	// 键值形态整段删除（键与值一起），cc_ 裸键值清尾，正文两侧保留。
	if strings.Contains(got, "abc123") || strings.Contains(got, "cc_foo") {
		t.Fatalf("计费头/cc_ 键值应被清除，实际 %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("正文应保留，实际 %q", got)
	}
}

// content 为 null 时仍必须净化 tool_calls（早期 bug：continue 导致漏净化）。
//
// 用改写表里的**完整身份句**：裸 "You are Claude Code" 刻意不改写
// （避免误伤零散引用文本），只有整句模板才进改写表。
func TestNormalizeSanitizeNullContent(t *testing.T) {
	identity := sanitizeRewrites[0][0]
	args, err := json.Marshal(map[string]string{"q": identity})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"messages":[{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"f","arguments":` + string(mustMarshal(t, string(args))) + `}}]},` +
		`{"role":"tool","tool_call_id":"a","content":"ok"}]}`
	obj := parse(t, body)
	NormalizeBody(obj)
	s := string(mustMarshal(t, obj))
	if strings.Contains(s, identity) {
		t.Fatalf("content=null 时 tool_calls 也必须净化: %s", s)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

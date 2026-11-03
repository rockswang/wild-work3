package chat

// rules_equiv_test.go 规则管线与旧 sanitizeText 的行为等价性验证。
//
// 存在意义：main/rules/ruleset/default_rules.json 里的字面量（上游指纹串）无法可靠地手工抄写/审阅，
// 只能靠「两条实现跑同一批输入、输出必须逐字节相同」来证明迁移没走样。
// 输入直接取自 sanitize.go 自己的表（不在此重复抄字面量）。

import (
	"strings"
	"testing"

	"wild-work3/main/rules"
	"wild-work3/main/rules/ruleset"
)

// applyRules 用规则引擎处理单段文本（放进 user 消息，模拟真实请求形状）。
func applyRules(t *testing.T, e *rules.Engine, text string) string {
	t.Helper()
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}}
	e.PreRoute(body)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 结构被破坏: %#v", body)
	}
	got, ok := msgs[0].(map[string]any)["content"].(string)
	if !ok {
		t.Fatalf("content 不是字符串: %#v", msgs[0])
	}
	return got
}

// TestRulesDefaultsMatchSanitizeText 逐条改写项：规则输出 == sanitizeText 输出。
//
// sanitizeText 末尾有 strings.TrimSpace，规则管线刻意不做（静默裁剪用户文本是缺陷），
// 故比较前对旧实现结果做同样的期望归一。
func TestRulesDefaultsMatchSanitizeText(t *testing.T) {
	e, err := rules.New(ruleset.Defaults)
	if err != nil {
		t.Fatalf("编译默认规则失败: %v", err)
	}
	for i, rw := range sanitizeRewrites {
		in := "前 " + rw[0] + " 后"
		want := sanitizeText(in)
		got := applyRules(t, e, in)
		if got != want {
			t.Fatalf("rewrite[%d] 不等价: want=%q got=%q", i, want, got)
		}
		if want == in {
			t.Fatalf("rewrite[%d] 旧实现未改写，用例失效", i)
		}
		if !strings.Contains(got, rw[1]) {
			t.Fatalf("rewrite[%d] 输出缺少替换串", i)
		}
	}
}

// TestRulesDefaultsMatchSanitizeTextHeaders header/cc_ 剥离层：整段删除后正文保留。
//
// 口径说明：旧 sanitizeText 的 hasFingerprint 预检要求「cc_ 与某个指纹同时出现」才动手，
// 因此单独的 cc_ 键值在旧实现里原样返回；规则管线用 Anchor=cc_ 预检，会直接把它剥掉。
// 这是刻意的行为改进（cc_ 键值本身即上游指纹，留着没有好处），故本用例不再要求
// 与旧实现逐字节相同，改为断言「键值不残留 + 正文保留」。
func TestRulesDefaultsMatchSanitizeTextHeaders(t *testing.T) {
	e, err := rules.New(ruleset.Defaults)
	if err != nil {
		t.Fatalf("编译默认规则失败: %v", err)
	}
	key := sanitizeFeatures[0]
	cases := []string{
		"hello " + key + ": abc123; world",
		"hello " + key + "=abc123\nworld",
		"a cc_entrypoint=cli; b",
		"x cc_a=1;cc_b=2; y",
		"裸 " + key + " 引用",
	}
	for _, in := range cases {
		got := applyRules(t, e, in)
		// 键值形态必须被剥掉（key: / key= 都不允许残留）
		if strings.Contains(got, key+":") || strings.Contains(got, key+"=") {
			t.Fatalf("键值未剥离: in=%q got=%q", in, got)
		}
		// 正文必须保留（只允许删指纹本身及其尾随 ;/空白）
		if strings.Contains(in, "hello") && !strings.Contains(got, "hello") {
			t.Fatalf("正文被误删: in=%q got=%q", in, got)
		}
		// 不含指纹的文本：与旧实现逐字节一致（含 cc_ 单独出现，此处刻意允许差异）
		if !strings.Contains(in, key) && !strings.Contains(in, "cc_") && got != sanitizeText(in) {
			t.Fatalf("无指纹文本应与旧实现一致: in=%q want=%q got=%q", in, sanitizeText(in), got)
		}
	}
}

// TestRulesDefaultsSkipClean 不含指纹的普通文本零改动。
func TestRulesDefaultsSkipClean(t *testing.T) {
	e, err := rules.New(ruleset.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"你好，世界", "  leading and trailing  ", "cc_ 但不是键值", "普通英文 hello world"} {
		if got := applyRules(t, e, in); got != in {
			t.Fatalf("干净文本被改动: in=%q got=%q", in, got)
		}
	}
}

// TestRulesDefaultsRoleNormalized developer → system（含大小写/空白变体）。
func TestRulesDefaultsRoleNormalized(t *testing.T) {
	e, err := rules.New(ruleset.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": " Developer ", "content": "x"},
		map[string]any{"role": "user", "content": "y"},
	}}
	if !e.PreRoute(body) {
		t.Fatal("应报告改动")
	}
	msgs := body["messages"].([]any)
	if r := msgs[0].(map[string]any)["role"]; r != "system" {
		t.Fatalf("role=%v", r)
	}
	if r := msgs[1].(map[string]any)["role"]; r != "user" {
		t.Fatalf("user 不该被改: %v", r)
	}
}

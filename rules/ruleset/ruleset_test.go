// ruleset_test.go 默认规则表本体的回归（W3-37 定案 3 把它从 main/rules 移到本包）。
//
// 引擎语义（Merge/Anchor/JSONPath/作用域）在 rules/rules_test.go；这里只盯「内嵌表能不能
// 编译成引擎、关键改写行为有没有走样」。字面量比对一律引用表项本身，不在此重复抄指纹串（陷阱 28）。
package ruleset_test

import (
	"testing"

	"wild-work3/main/rules"
	"wild-work3/main/rules/ruleset"
)

func TestDefaultsCompile(t *testing.T) {
	e, err := rules.New(ruleset.Defaults)
	if err != nil {
		t.Fatalf("默认表编译成引擎失败: %v", err)
	}
	if len(ruleset.Defaults) < 10 {
		t.Errorf("默认规则条数异常（应 10+）: %d", len(ruleset.Defaults))
	}
	if e == nil {
		t.Fatal("nil engine")
	}
}

// TestDefaultRulesJSONParseable 面板 diff 依赖原始 JSON 形态（defaults 字段），必须可独立解析。
func TestDefaultRulesJSONParseable(t *testing.T) {
	rs, err := rules.ParseRules(ruleset.DefaultRulesJSON())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != len(ruleset.Defaults) {
		t.Errorf("原始 JSON 与 Defaults 条数不一致: %d vs %d", len(rs), len(ruleset.Defaults))
	}
}

// TestDefaultsDeveloperAndSanitize 结构归一 + 指纹剥离 + header 剥离三条主链路。
func TestDefaultsDeveloperAndSanitize(t *testing.T) {
	e, err := rules.New(ruleset.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	b := map[string]any{"messages": []any{map[string]any{"role": " Developer ", "content": "You are Claude Code, Anthropic's official CLI for Claude 11128"}, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"arguments": "x-anthropic-billing-header: secret; ok"}}}}}}
	if !e.PreRoute(b) {
		t.Fatal("unchanged")
	}
	m := b["messages"].([]any)
	if m[0].(map[string]any)["role"] != "system" || m[0].(map[string]any)["content"] != "You are Claude Code, Anthropic's official CLI tool for Claude 11-128" {
		t.Fatalf("%#v", m[0])
	}
	args := m[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
	if args != "ok" {
		t.Fatalf("args=%q", args)
	}
}

// TestDefaultOrderFirst role.developer（Order 10）必须排在脱敏规则之前。
func TestDefaultOrderFirst(t *testing.T) {
	if ruleset.Defaults[0].ID != "role.developer" {
		t.Errorf("首条应为 role.developer，实际 %q", ruleset.Defaults[0].ID)
	}
}

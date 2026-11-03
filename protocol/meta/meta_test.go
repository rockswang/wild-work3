package meta

// 思考档位三字段的回归（W3-32）。
//
// 口径来自旧版 dev/internal/provider/provider.go 的 CleanEfforts 与
// handler.buildModelEntry：「上游没给合法列表就不编档」（Buddy2api v2.1.11
// 的 never invent effort lists 原则）。

import (
	"encoding/json"
	"testing"
)

// CleanEfforts 去空白、去重保序、丢空串；全部无效返回 nil（不是空切片）。
func TestCleanEfforts(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"", "  ", "\t"}, nil},
		{[]string{"low"}, []string{"low"}},
		{[]string{" low ", "medium", "low"}, []string{"low", "medium"}}, // 去空白 + 去重保序
		{[]string{"", "high", "", "low"}, []string{"high", "low"}},      // 丢空串但保序
	}
	for _, c := range cases {
		got := CleanEfforts(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("CleanEfforts(%q) = %q，期望 %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("CleanEfforts(%q) = %q，期望 %q", c.in, got, c.want)
			}
		}
		// nil 与空切片必须能区分：全部无效时必须是 nil（omitempty 才不输出）
		if len(c.want) == 0 && got != nil {
			t.Fatalf("CleanEfforts(%q) 应返回 nil，实际是非 nil 空切片", c.in)
		}
	}
}

// ModelInfo 未声明档位时三个字段都必须从 JSON 里消失（omitempty + nil）。
func TestModelInfoEffortsOmitempty(t *testing.T) {
	raw, err := json.Marshal(ModelInfo{ID: "chatgpt/gpt-6"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"efforts", "default_effort", "can_disable_thinking"} {
		if _, ok := m[k]; ok {
			t.Fatalf("未声明档位时不该输出 %s：%s", k, raw)
		}
	}
}

// 声明了档位时三字段都能序列化，且 can_disable_thinking 的三态保留（false 不等于未声明）。
func TestModelInfoEffortsMarshal(t *testing.T) {
	no := false
	mi := ModelInfo{
		ID:                 "gemini/gemini-3.8-flash",
		Efforts:            []string{"low", "medium", "high", "tiered"},
		DefaultEffort:      "medium",
		CanDisableThinking: &no,
	}
	raw, err := json.Marshal(mi)
	if err != nil {
		t.Fatal(err)
	}
	var back ModelInfo
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Efforts) != 4 || back.Efforts[3] != "tiered" {
		t.Fatalf("Efforts 未正确往返：%+v", back.Efforts)
	}
	if back.DefaultEffort != "medium" {
		t.Fatalf("DefaultEffort 未正确往返：%q", back.DefaultEffort)
	}
	if back.CanDisableThinking == nil || *back.CanDisableThinking {
		t.Fatalf("CanDisableThinking=false 必须能往返（nil 会被当成未声明）：%v", back.CanDisableThinking)
	}
}

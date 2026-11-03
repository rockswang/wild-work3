package channel

// 上游 /models 宽容解析的单测（W3-32）。
//
// 样本都是第三方网关真实会出现的形状：官方 OpenAI 形状（无任何能力字段）、
// OpenRouter 式（architecture/top_provider 嵌套）、自造 flat 字段、
// 以及「上游自相矛盾」（默认档位不在档位表里）这种必须宁可不写的场景。

import "testing"

// 官方 OpenAI 形状：只有 id/object/created/owned_by → 三字段必须全空（不编造）。
func TestParseModelInfosOfficialShape(t *testing.T) {
	raw := []byte(`{"object":"list","data":[
		{"id":"gpt-4o","object":"model","created":1,"owned_by":"openai"}]}`)
	ms, err := parseModelInfos(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != "gpt-4o" {
		t.Fatalf("应解出 1 个模型：%+v", ms)
	}
	m := ms[0]
	if m.Context != nil {
		t.Fatalf("上游没给上下文，不该臆造：%v", *m.Context)
	}
	if len(m.Efforts) != 0 || m.DefaultEffort != "" || m.CanDisableThinking != nil {
		t.Fatalf("上游没给档位，不该臆造：%+v", m)
	}
	if m.Name != "gpt-4o" {
		t.Fatalf("无 name 时应回落成 ID：%q", m.Name)
	}
}

// 第三方网关常见形状：flat 字段 + 嵌套 architecture/top_provider + 档位对象数组。
func TestParseModelInfosRichShape(t *testing.T) {
	raw := []byte(`{"data":[{
		"id":"acme/thinker",
		"name":"Acme Thinker",
		"context_length":"1M",
		"top_provider":{"max_completion_tokens":64000},
		"architecture":{"input_modalities":["text","image"]},
		"supported_parameters":["tools","reasoning"],
		"reasoning_efforts":[{"level":"Low"},{"level":"high"}],
		"default_reasoning_effort":"HIGH"
	}]}`)
	ms, err := parseModelInfos(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := ms[0]
	if m.Context == nil || *m.Context != 1000000 {
		t.Fatalf("1M 应解析成 1000000：%v", m.Context)
	}
	if len(m.Efforts) != 2 || m.Efforts[0] != "low" || m.Efforts[1] != "high" {
		t.Fatalf("档位对象数组应取 level 并小写：%+v", m.Efforts)
	}
	// 默认档位 "HIGH" 小写后落在表内，必须保住
	if m.DefaultEffort != "high" {
		t.Fatalf("默认档位应小写后保留：%q", m.DefaultEffort)
	}
	if !m.Capabilities.Vision || !m.Capabilities.Tools || !m.Capabilities.Reasoning {
		t.Fatalf("能力位应全部识别：%+v", m.Capabilities)
	}
}

// 上游自相矛盾：默认档位不在档位表里 → 默认值清空，但档位表保留。
func TestParseModelInfosDefaultNotInList(t *testing.T) {
	raw := []byte(`{"data":[{"id":"m1","reasoning_efforts":["low","medium"],"default_reasoning_effort":"ultra"}]}`)
	ms, err := parseModelInfos(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms[0].Efforts) != 2 {
		t.Fatalf("档位表应保留：%+v", ms[0].Efforts)
	}
	if ms[0].DefaultEffort != "" {
		t.Fatalf("默认档位不在表内必须清空（不得拿它覆盖有效档位）：%q", ms[0].DefaultEffort)
	}
}

// 只有档位表、没有 supports_reasoning：档位非空本身就说明支持思考（Reasoning 置真）。
func TestParseModelInfosEffortsImplyReasoning(t *testing.T) {
	raw := []byte(`{"data":[{"id":"m1","reasoning_efforts":["low","high"]}]}`)
	ms, _ := parseModelInfos(raw)
	if !ms[0].Capabilities.Reasoning {
		t.Fatalf("档位表非空应视为支持思考：%+v", ms[0])
	}
}

// 显式 false 不能被当成「支持」：supports_images:false 不得被子串匹配命中。
func TestParseModelInfosExplicitFalse(t *testing.T) {
	raw := []byte(`{"data":[{"id":"m1","supports_images":false,"supports_tools":false,"supports_reasoning":false}]}`)
	ms, _ := parseModelInfos(raw)
	if ms[0].Capabilities.Vision || ms[0].Capabilities.Tools || ms[0].Capabilities.Reasoning {
		t.Fatalf("显式 false 不该被判成支持：%+v", ms[0].Capabilities)
	}
}

// 三种外层形态 + 数组里混字符串（异常形状跳过，不让整次解析失败）。
func TestParseModelInfosOuterShapes(t *testing.T) {
	cases := []string{
		`[{"id":"a"}]`,
		`{"models":[{"id":"a"}]}`,
		`{"result":{"data":[{"id":"a"}]}}`,
		`{"data":["junk",{"id":"a"}]}`,
	}
	for _, c := range cases {
		ms, err := parseModelInfos([]byte(c))
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if len(ms) != 1 || ms[0].ID != "a" {
			t.Fatalf("%s 应解出 id=a，实际 %+v", c, ms)
		}
	}
}

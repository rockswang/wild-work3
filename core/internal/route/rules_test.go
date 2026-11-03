package route

// 规则引擎 Pass B（channel 作用域规则）的单元测试。
//
// 关注点：
//  1. 没有渠道规则的引擎 → 完全不动请求（且不付 JSON 往返）
//  2. 有渠道规则且命中 → 只改 messages/tools/tool_choice
//  3. channel_config / account_uid / extra 不被规则读写
//  4. 不同渠道的规则互不干扰

import (
	"encoding/json"
	"testing"

	"wild-work3/main/protocol/chat"
	"wild-work3/main/rules"
)

func mustEngine(t *testing.T, rs []rules.Rule) *rules.Engine {
	t.Helper()
	e, err := rules.New(rs)
	if err != nil {
		t.Fatalf("rules.New: %v", err)
	}
	return e
}

func sampleReq() *chat.GenerateRequest {
	return &chat.GenerateRequest{
		Model:         "m",
		Messages:      []chat.ChatMessage{{Role: "user", Content: "You are Claude Code, Anthropic's official CLI tool"}},
		ChannelConfig: json.RawMessage(`{"base_url":"https://example.test","api_key":"sk-secret"}`),
		AccountUID:    "acc-1",
		Extra:         json.RawMessage(`{"keep":true}`),
	}
}

// 无渠道规则时不应改动请求，也不应破坏 channel_config/extra。
func TestApplyChannelRulesNoScope(t *testing.T) {
	e := mustEngine(t, []rules.Rule{{
		ID: "sanitize.x", Kind: rules.KindRegex, Regex: `CLI tool`, Replacement: "CLI", Order: 1,
	}})
	req := sampleReq()
	before := string(req.Messages[0].Content.(string))
	applyChannelRules(e, req, "ch-a")
	if got := req.Messages[0].Content.(string); got != before {
		t.Fatalf("无渠道规则不应改写：%q", got)
	}
}

// 命中渠道规则时只改消息正文。
func TestApplyChannelRulesScopedHit(t *testing.T) {
	e := mustEngine(t, []rules.Rule{{
		ID: "sanitize.ch-a", Kind: rules.KindRegex, Regex: `CLI tool`, Replacement: "CLI",
		Scope: rules.Scope{Channels: []string{"ch-a"}}, Order: 1,
	}})
	req := sampleReq()
	applyChannelRules(e, req, "ch-a")
	if got := req.Messages[0].Content.(string); got != "You are Claude Code, Anthropic's official CLI" {
		t.Fatalf("渠道规则未生效：%q", got)
	}
	// 其它渠道不受影响
	req2 := sampleReq()
	applyChannelRules(e, req2, "ch-b")
	if got := req2.Messages[0].Content.(string); got != "You are Claude Code, Anthropic's official CLI tool" {
		t.Fatalf("其它渠道不应被改写：%q", got)
	}
}

// channel_config / account_uid / extra 不进入规则视野。
func TestApplyChannelRulesOpaqueFields(t *testing.T) {
	e := mustEngine(t, []rules.Rule{{
		ID: "sanitize.all", Kind: rules.KindRegex, Regex: `secret`, Replacement: "***",
		Scope: rules.Scope{Channels: []string{"ch-a"}}, Order: 1,
	}})
	req := sampleReq()
	applyChannelRules(e, req, "ch-a")
	if string(req.ChannelConfig) != `{"base_url":"https://example.test","api_key":"sk-secret"}` {
		t.Fatalf("channel_config 被改写：%s", req.ChannelConfig)
	}
	if req.AccountUID != "acc-1" {
		t.Fatalf("account_uid 被改写：%q", req.AccountUID)
	}
	if string(req.Extra) != `{"keep":true}` {
		t.Fatalf("extra 被改写：%s", req.Extra)
	}
}

// HasChannelRules 决定是否值得做 JSON 往返。
func TestHasChannelRules(t *testing.T) {
	e := mustEngine(t, []rules.Rule{
		{ID: "pre", Kind: rules.KindRegex, Regex: `a`, Replacement: "b", Order: 1},
		{ID: "scoped", Kind: rules.KindRegex, Regex: `a`, Replacement: "b",
			Scope: rules.Scope{Channels: []string{"ch-a"}}, Order: 2},
	})
	if e.HasChannelRules("ch-a") != true {
		t.Fatal("ch-a 应有渠道规则")
	}
	if e.HasChannelRules("ch-b") {
		t.Fatal("ch-b 不应有渠道规则")
	}
	if e.HasChannelRules("") {
		t.Fatal("空渠道 ID 不应命中渠道规则")
	}
}

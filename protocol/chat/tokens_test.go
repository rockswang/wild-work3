package chat

import "testing"

func TestMergeUsageKeepsPresenceAndMultipleFrames(t *testing.T) {
	var u *Usage
	MergeUsage(&u, map[string]any{"prompt_tokens": float64(0), "cache": map[string]any{"cached": float64(0)}})
	MergeUsage(&u, map[string]any{"completion_tokens": float64(7)})
	if u == nil || !u.PromptReported || u.PromptTokens != 0 {
		t.Fatalf("明确 0 未保留: %+v", u)
	}
	if !u.CompletionReported || u.CompletionTokens != 7 {
		t.Fatalf("多帧未合并: %+v", u)
	}
	if u.TotalReported {
		t.Fatalf("缺失 total 不应标记实报: %+v", u)
	}
	if u.Cache == nil || !u.Cache.CachedReported {
		t.Fatalf("cache 明确 0 未保留: %+v", u.Cache)
	}
}

func TestMergeUsageTreatsLegacyEmptyAsUnknown(t *testing.T) {
	var u *Usage
	MergeUsage(&u, map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0), "total_tokens": float64(0)})
	if u.PromptReported || u.CompletionReported || u.TotalReported {
		t.Fatalf("旧插件空 usage 不应视为实报: %+v", u)
	}
	MergeUsage(&u, map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0), "total_tokens": float64(0), "prompt_reported": true, "completion_reported": true, "total_reported": true})
	if !u.PromptReported || !u.CompletionReported || !u.TotalReported {
		t.Fatalf("带来源的明确 0 应视为实报: %+v", u)
	}
}

func TestFinalizeUsageOnlyFillsMissingDimensions(t *testing.T) {
	u := &Usage{PromptTokens: 0, UsageSource: UsageSource{PromptReported: true}}
	req := &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "你好 world"}}}
	got := FinalizeUsage(req, u, "回答")
	if got.PromptTokens != 0 || !got.PromptReported || got.PromptEstimated {
		t.Fatalf("覆盖了上游明确 0: %+v", got)
	}
	if got.CompletionTokens <= 0 || !got.CompletionEstimated {
		t.Fatalf("未估算 completion: %+v", got)
	}
	if got.TotalTokens != got.CompletionTokens || !got.TotalEstimated {
		t.Fatalf("未推导 total: %+v", got)
	}
}

func TestMergeUsageRejectsInvalidNumbers(t *testing.T) {
	for _, v := range []any{-1.0, 1.5, 1e100} {
		var u *Usage
		MergeUsage(&u, map[string]any{"prompt_tokens": v})
		if u.PromptReported {
			t.Fatalf("非法 token 被接受: %v => %+v", v, u)
		}
	}
}

func TestEstimatePromptIgnoresImagePayload(t *testing.T) {
	req := &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "看图"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + string(make([]byte, 10000))}},
	}}}}
	if n := EstimatePromptTokens(req); n > 20 {
		t.Fatalf("图片 payload 不应按正文计 token: %d", n)
	}
}

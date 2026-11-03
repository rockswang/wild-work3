package runtime

import (
	"testing"

	"wild-work3/main/protocol/chat"
)

func TestPluginUsageRoundTripLegacyEmptyIsUnknown(t *testing.T) {
	// 独立仓插件当前仍使用旧 chat.Usage：三个字段固定序列化为 0。
	var u *chat.Usage
	chat.MergeUsage(&u, map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0), "total_tokens": float64(0), "time_ms": float64(12)})
	if u == nil || u.PromptReported || u.CompletionReported || u.TotalReported {
		t.Fatalf("空 usage 被误判为实报: %+v", u)
	}
	if u.TimeMs != 12 {
		t.Fatalf("时间字段应保留: %+v", u)
	}
}

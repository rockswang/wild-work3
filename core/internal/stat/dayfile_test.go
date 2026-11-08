package stat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 写入必须落到「按日 + 按渠道」文件，且行内不再带 channel_id（省行宽）。
func TestWritesGoToDayChannelFile(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.AppendUsage(UsageRecord{ChannelID: "ch", Model: "m", PromptTokens: 10, OK: true})
	l.AppendCredit(CreditRecord{ChannelID: "ch", AccountUID: "u", Delta: f64(1)})
	l.Close()

	day := time.Now().Format(dayLayout)
	for _, prefix := range []string{subUsage, subCredit} {
		p := filepath.Join(dir, prefix, day, "ch.jsonl")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("应写入 %s: %v", p, err)
		}
		if len(raw) == 0 {
			t.Fatalf("%s 不应为空", p)
		}
		var m map[string]any
		if err := json.Unmarshal(raw[:len(raw)-1], &m); err != nil {
			t.Fatalf("%s 坏行: %v", p, err)
		}
		if _, ok := m["channel_id"]; ok {
			t.Errorf("行内不应再带 channel_id（归属在文件名上）: %s", raw)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "usage.jsonl")); !os.IsNotExist(err) {
		t.Error("不应再写旧单文件名")
	}
}

// 无法归渠道的记录落 `_none.jsonl`，查询时 `ChannelID` 保持空（面板不计入渠道维度）。
func TestUnknownChannelFile(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.AppendUsage(UsageRecord{Model: "m", OK: false, ErrKind: "not_found"})
	p := filepath.Join(dir, subUsage, time.Now().Format(dayLayout), unknownChannel+".jsonl")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("无渠道记录应落 %s: %v", p, err)
	}
	got := l.QueryUsage(7)
	if len(got) != 1 || got[0].ChannelID != "" {
		t.Fatalf("_none 应回空 ChannelID，得到 %+v", got)
	}
}

// 渠道归属从文件名回填：同一渠道多天、同一天多渠道都要能正确聚合。
func TestQueryRestoresChannelFromFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, d := range []int{0, -1, -3} {
		day := now.AddDate(0, 0, d)
		writeRaw(t, l, subUsage, day, "chA", UsageRecord{TS: day.Format(time.RFC3339), Model: "a", TotalTokens: 1, OK: true})
		writeRaw(t, l, subUsage, day, "chB", UsageRecord{TS: day.Format(time.RFC3339), Model: "b", TotalTokens: 2, OK: true})
	}
	got := l.QueryUsage(7)
	tot := map[string]int{}
	for _, r := range got {
		tot[r.ChannelID] += r.TotalTokens
	}
	if tot["chA"] != 3 || tot["chB"] != 6 {
		t.Fatalf("渠道聚合错: %+v（明细 %+v）", tot, got)
	}
	// 1 天窗口只应读到今天
	if one := l.QueryUsage(1); len(one) != 2 {
		t.Fatalf("1 天窗口应只与今天 2 条，得到 %d", len(one))
	}
}

// 超过 keepDays 的日目录在 Open 时清理；窗口内的必须保留。
func TestCleanupOldDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	oldDay, freshDay := now.AddDate(0, 0, -30), now.AddDate(0, 0, -1)
	writeRaw(t, l, subUsage, oldDay, "ch", UsageRecord{TS: oldDay.Format(time.RFC3339), Model: "old", OK: true})
	writeRaw(t, l, subUsage, freshDay, "ch", UsageRecord{TS: freshDay.Format(time.RFC3339), Model: "fresh", OK: true})
	// 非日目录（请求体摘要分片所在根目录）不能被日清理误删
	bt := filepath.Join(dir, "body-trace-0123456789.jsonl")
	if err := os.WriteFile(bt, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(dir) // 第二次 Open 触发 cleanupDays
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, err := os.Stat(filepath.Join(dir, subUsage, oldDay.Format(dayLayout))); !os.IsNotExist(err) {
		t.Error("30 天前的日目录应被清理")
	}
	if _, err := os.Stat(filepath.Join(dir, subUsage, freshDay.Format(dayLayout))); err != nil {
		t.Errorf("窗口内的日目录不应被删: %v", err)
	}
	if _, err := os.Stat(bt); err != nil {
		t.Errorf("请求体摘要分片不应被删: %v", err)
	}
}

// 失败日志：落 fail/YYYYMMDD.jsonl，渠道放在行内（允许为空），查询最新在前。
func TestFailRecords(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.AppendFail(FailRecord{ChannelID: "chA", Model: "m1", ErrKind: "server", Message: "boom"})
	l.AppendFail(FailRecord{Model: "nomodel", ErrKind: "not_found"})
	day := time.Now().Format(dayLayout)
	p := filepath.Join(dir, subFail, day+".jsonl")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("失败日志应落 %s: %v", p, err)
	}
	got := l.QueryFails(7, 0)
	if len(got) != 2 {
		t.Fatalf("应读到 2 条失败，得到 %d", len(got))
	}
	if got[0].ErrKind != "not_found" || got[1].ErrKind != "server" {
		t.Fatalf("应最新在前，得到 %+v", got)
	}
	if lim := l.QueryFails(7, 1); len(lim) != 1 || lim[0].ErrKind != "not_found" {
		t.Fatalf("limit 应生效，得到 %+v", lim)
	}
}

// 失败摘要必须截断（上游可能回整页 HTML）。
func TestFailMessageTruncated(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.AppendFail(FailRecord{Model: "m", Message: string(make([]byte, failMessageCap+200))})
	got := l.QueryFails(7, 0)
	if len(got) != 1 || len(got[0].Message) != failMessageCap {
		t.Fatalf("摘要应截断到 %d，得到 %d", failMessageCap, len(got[0].Message))
	}
}

// daysInRange 逐日枚举含端点，跨月/跨年都要连续。
func TestDaysInRange(t *testing.T) {
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.Local)
	got := daysInRange(now.AddDate(0, 0, -3), now) // 2/27 → 3/2
	want := []string{"20260227", "20260228", "20260301", "20260302"}
	if len(got) != len(want) {
		t.Fatalf("应得 %v，得到 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应得 %v，得到 %v", want, got)
		}
	}
}

// 旧布局（单文件/按月分段）只提示不删（用户数据，删不删由人决定），也不读取。
func TestLegacyLayoutNotReadNotDeleted(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "usage-202601.jsonl")
	if err := os.WriteFile(legacy, []byte(`{"ts":"2026-01-01T00:00:00Z","channel_id":"ch","model":"old"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := l.QueryUsage(3650); len(got) != 0 {
		t.Fatalf("旧布局不应再被读取，得到 %+v", got)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("旧文件不应被删（可能含用户数据）: %v", err)
	}
}

// ---- 辅助 ----

func f64(v float64) *float64 { return &v }

// writeRaw 直接往「按日 + 按渠道」布局写一条记录（绕过 AppendUsage 便于造历史数据）。
func writeRaw(t *testing.T, l *Book, prefix string, day time.Time, channel string, rec UsageRecord) {
	t.Helper()
	p := l.channelFile(prefix, day, channel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	rec.ChannelID = ""
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

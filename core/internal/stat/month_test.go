package stat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 写入必须落到当月分段 usage-YYYYMM.jsonl，而不是旧单文件。
func TestWritesGoToMonthSegment(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.AppendUsage(UsageRecord{ChannelID: "ch", Model: "m", PromptTokens: 10, OK: true})
	l.AppendCredit(CreditRecord{AccountUID: "u", Delta: f64(1)})
	l.Close()

	month := time.Now().Format("200601")
	for _, prefix := range []string{"usage", "credit"} {
		p := filepath.Join(dir, prefix+"-"+month+".jsonl")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("应写入当月分段 %s: %v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "usage.jsonl")); !os.IsNotExist(err) {
		t.Error("不应再写旧单文件名")
	}
}

// 旧单文件（ww3 早期版本）必须迁移进当月分段，否则升级后统计清零。
func TestMigrateLegacySingleFile(t *testing.T) {
	dir := t.TempDir()
	rec := UsageRecord{TS: time.Now().UTC().Format(time.RFC3339), ChannelID: "ch", Model: "m", PromptTokens: 7, OK: true}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := l.QueryUsage(7)
	l.Close()
	if len(got) != 1 || got[0].PromptTokens != 7 {
		t.Fatalf("迁移后应能查到 1 条记录，得到 %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage.jsonl")); !os.IsNotExist(err) {
		t.Error("迁移成功后旧单文件应删除")
	}
}

// 跨月窗口必须两个分段都读（旧版曾因月份枚举写错，每月 1~6 号看不到本月数据）。
func TestQuerySpansMonths(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 上月与本月各造一条（用 UTC 便于跨时区测试稳定）
	prev := now.AddDate(0, -1, 0)
	writeRaw(t, dir, monthFile("usage", prev), UsageRecord{TS: prev.UTC().Format(time.RFC3339), ChannelID: "ch", Model: "prev", PromptTokens: 3, OK: true})
	writeRaw(t, dir, monthFile("usage", now), UsageRecord{TS: now.UTC().Format(time.RFC3339), ChannelID: "ch", Model: "cur", PromptTokens: 5, OK: true})

	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// 35 天窗口必然同时覆盖上月与本月
	got := l.QueryUsage(35)
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.Model] = true
	}
	if !seen["prev"] || !seen["cur"] {
		t.Errorf("跨月查询应同时读到两段，得到 %+v", got)
	}
}

// 超过 keepMonths 的月分段在 Open 时清理，当月分段保留。
func TestCleanupOldMonths(t *testing.T) {
	dir := t.TempDir()
	old := monthFile("usage", time.Now().AddDate(0, -3, 0))
	cur := monthFile("usage", time.Now())
	writeRaw(t, dir, old, UsageRecord{TS: time.Now().UTC().Format(time.RFC3339), ChannelID: "ch", Model: "old", OK: true})
	writeRaw(t, dir, cur, UsageRecord{TS: time.Now().UTC().Format(time.RFC3339), ChannelID: "ch", Model: "cur", OK: true})
	// 非月分段文件（请求体摘要分片）不能被月清理误删
	bt := filepath.Join(dir, "body-trace-0123456789.jsonl")
	if err := os.WriteFile(bt, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Error("3 个月前的分段应被清理")
	}
	for _, name := range []string{cur, "body-trace-0123456789.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s 不应被删除: %v", name, err)
		}
	}
}

// 当月分段内仍按 keepDays 裁剪条目。
func TestTrimCurrentMonthKeepsRecent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := UsageRecord{TS: now.AddDate(0, 0, -10).UTC().Format(time.RFC3339), ChannelID: "ch", Model: "old", OK: true}
	fresh := UsageRecord{TS: now.UTC().Format(time.RFC3339), ChannelID: "ch", Model: "fresh", OK: true}
	writeRaw(t, dir, monthFile("usage", now), old, fresh)

	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := l.QueryUsage(35)
	if len(got) != 1 || got[0].Model != "fresh" {
		t.Fatalf("10 天前的条目应被裁掉，得到 %+v", got)
	}
}

// monthsInRange 必须逐月枚举（含端点），不能漏月。
func TestMonthsInRange(t *testing.T) {
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	got := monthsInRange(now.AddDate(0, 0, -7), now) // 2/26 → 3/5
	want := []string{"202602", "202603"}
	if len(got) != len(want) {
		t.Fatalf("应得 %v，得到 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应得 %v，得到 %v", want, got)
		}
	}
}

// ---- 辅助 ----

func f64(v float64) *float64 { return &v }

func writeRaw(t *testing.T, dir, name string, recs ...UsageRecord) {
	t.Helper()
	var buf []byte
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf = append(append(buf, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

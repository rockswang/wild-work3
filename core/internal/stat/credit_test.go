package stat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wild-work3/main/protocol/meta"
)

func f(v float64) *float64 { return &v }

// mk 构造一条 ResourceItem（积分口径）。
func mk(name string, remain, used, total float64, expire string) meta.ResourceItem {
	return meta.ResourceItem{Name: name, Key: name, Remain: f(remain), Used: f(used),
		Total: f(total), ExpireAt: expire, Usable: true, Unit: "credits"}
}

func openTemp(t *testing.T) *Book {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "stat"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// 首次见某账号：只记一条「存量额度」baseline，不逐条展开。
func TestDiffCreditsBaseline(t *testing.T) {
	l := openTemp(t)
	n := l.DiffCredits("ch1", "u1", []meta.ResourceItem{
		mk("pack-a", 100, 0, 100, ""), mk("pack-b", 50, 0, 50, ""),
	}, f(150))
	if n != 1 {
		t.Fatalf("首次应只记 1 条 baseline，实际 %d", n)
	}
	recs := l.QueryCredits(1)
	if len(recs) != 1 || recs[0].Kind != "earn" || recs[0].Note != "存量额度" {
		t.Fatalf("baseline 记录不符: %+v", recs)
	}
	if recs[0].Delta == nil || *recs[0].Delta != 150 {
		t.Fatalf("baseline 金额应为 150: %+v", recs[0].Delta)
	}
}

// 余额上涨 → earn；下降 → spend；两者都按 key 聚合。
func TestDiffCreditsEarnSpend(t *testing.T) {
	l := openTemp(t)
	_ = l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 100, 0, 100, "")}, f(100))
	// 上涨 40
	if n := l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 140, 0, 140, "")}, f(140)); n != 1 {
		t.Fatalf("上涨应记 1 条，实际 %d", n)
	}
	// 下降 30
	if n := l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 110, 0, 140, "")}, f(110)); n != 1 {
		t.Fatalf("下降应记 1 条，实际 %d", n)
	}
	recs := l.QueryCredits(1)
	// 倒序不保证（AppendCredit 按发生顺序），这里按 Kind 统计
	var earn, spend float64
	for _, r := range recs {
		switch r.Kind {
		case "earn":
			earn += *r.Delta
		case "spend":
			spend += *r.Delta
		}
	}
	if earn != 140 || spend != -30 { // 100 baseline + 40 上涨 = 140；-30 消耗
		t.Fatalf("earn=%v spend=%v，期望 earn=140 spend=-30", earn, spend)
	}
}

// 到期日已过且余额下降 → 归因为 expire 而不是 spend。
func TestDiffCreditsExpire(t *testing.T) {
	l := openTemp(t)
	_ = l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 100, 0, 100, "2020-01-01")}, f(100))
	l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 0, 0, 100, "2020-01-01")}, f(0))
	recs := l.QueryCredits(1)
	last := recs[len(recs)-1]
	if last.Kind != "expire" {
		t.Fatalf("已过期条目余额下降应记 expire，实际 %q", last.Kind)
	}
}

// 发放与消耗同窗：remain 持平但 used 上涨 → 记为 earn（issue #67）。
func TestDiffCreditsGrantWhileConsuming(t *testing.T) {
	l := openTemp(t)
	_ = l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 100, 10, 110, "")}, f(100))
	l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 100, 30, 130, "")}, f(100))
	recs := l.QueryCredits(1)
	last := recs[len(recs)-1]
	if last.Kind != "earn" || *last.Delta != 20 {
		t.Fatalf("同窗发放应记 earn +20，实际 %q %v", last.Kind, *last.Delta)
	}
}

// 池子缩水：未上报到期日 + 渠道分别记 total/used + total 下降 → expire 而非 spend（R101）。
func TestDiffCreditsPoolShrank(t *testing.T) {
	l := openTemp(t)
	// 先用一个带 used 的账号把渠道标记为「分别上报 total/used」
	_ = l.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 100, 5, 105, "")}, f(100))
	_ = l.DiffCredits("ch1", "u2", []meta.ResourceItem{mk("pack-a", 80, 0, 80, "")}, f(80))
	// total 从 80 降到 60（池子缩水），未上报到期日
	l.DiffCredits("ch1", "u2", []meta.ResourceItem{mk("pack-a", 60, 0, 60, "")}, f(60))
	recs := l.QueryCredits(1)
	last := recs[len(recs)-1]
	if last.Kind != "expire" {
		t.Fatalf("池子缩水应记 expire，实际 %q", last.Kind)
	}
}

// 快照落盘后可跨进程复用（重启后仍能差分）。
func TestDiffCreditsSnapshotPersist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stat")
	l1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = l1.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 100, 0, 100, "")}, f(100))
	l1.Close()
	if _, err := os.Stat(filepath.Join(dir, "credit-snapshot.json")); err != nil {
		t.Fatalf("快照文件应存在: %v", err)
	}
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	// 第二次打开后余额上涨 20，应只记 1 条（说明读到了旧快照，不是当成首次）
	if n := l2.DiffCredits("ch1", "u1", []meta.ResourceItem{mk("pack-a", 120, 0, 120, "")}, f(120)); n != 1 {
		t.Fatalf("跨进程差分应记 1 条，实际 %d", n)
	}
	recs := l2.QueryCredits(1)
	if recs[len(recs)-1].Note == "存量额度" {
		t.Fatal("读到旧快照后不应再记 baseline")
	}
}

// 到期日当天仍可用，次日 00:00 失效。
func TestExpireInstant(t *testing.T) {
	got, ok := ExpireInstant("2026-11-01")
	if !ok || got.Format("2006-01-02 15:04") != "2026-11-02 00:00" {
		t.Fatalf("墙钟日应次日 00:00 失效，实际 %v ok=%v", got, ok)
	}
	if _, ok := ExpireInstant(""); ok {
		t.Fatal("空串应 ok=false")
	}
	if _, ok := ExpireInstant("not-a-date"); ok {
		t.Fatal("非法串应 ok=false")
	}
	if d := ExpireDay("2026-11-01"); d != "2026-11-01" {
		t.Fatalf("ExpireDay 应为 2026-11-01，实际 %q", d)
	}
}

// EarliestExpireDay 只认可用、非信息项、有余额、未过期的条目。
func TestEarliestExpireDay(t *testing.T) {
	items := []meta.ResourceItem{
		mk("a", 10, 0, 10, "2026-12-01"),
		mk("b", 10, 0, 10, "2026-11-15"),
		{Name: "c", Remain: f(10), ExpireAt: "2026-10-01", Usable: true, Unit: "credits"}, // 已过期
		{Name: "d", Remain: f(10), ExpireAt: "2026-10-20", Usable: true, InfoOnly: true, Unit: "credits"},
		{Name: "e", Remain: f(0), ExpireAt: "2026-10-21", Usable: true, Unit: "credits"},
	}
	now := mustTime(t, "2026-11-01T00:00:00Z")
	if got := EarliestExpireDay(items, now); got != "2026-11-15" {
		t.Fatalf("最早有效到期日应为 2026-11-15，实际 %q", got)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("时间解析失败 %s: %v", s, err)
	}
	return v
}

package pool

import (
	"path/filepath"
	"testing"
	"time"
)

// 时钟注入辅助：池内所有判据都走 p.now，测试里换成固定时刻。
func newClockPool(t *testing.T, soft, hard time.Duration) (*Pool, *time.Time) {
	t.Helper()
	cur := time.Date(2026, 1, 1, 19, 7, 0, 0, time.UTC)
	p := New(soft, hard)
	p.now = func() time.Time { return cur }
	return p, &cur
}

func ptrf(v float64) *float64 { return &v }

// newPoolAt 新建一个时钟固定的池：恢复侧必须用同一时钟，否则冷却会被当成已过期而丢掉。
func newPoolAt(at time.Time) *Pool {
	p := New(time.Minute, 12*time.Hour)
	p.now = func() time.Time { return at }
	return p
}

func TestCooldownResetsErrCount(t *testing.T) {
	p, _ := newClockPool(t, time.Minute, 12*time.Hour)
	p.Upsert(Entry{UID: "u1@ch", ChannelID: "ch"})
	p.NoteError("u1@ch")
	p.NoteError("u1@ch")
	if e := p.entries["u1@ch"]; e.ErrCount != 2 {
		t.Fatalf("NoteError 计数 = %d, want 2", e.ErrCount)
	}
	p.Cooldown("u1@ch", "m", CoolHard)
	e := p.entries["u1@ch"]
	if e.ErrCount != 0 || e.Cool != CoolHard {
		t.Fatalf("冷却后 ErrCount=%d Cool=%s，期望 0/hard（旧版 Cooldown 里 errCount=0）", e.ErrCount, e.Cool)
	}
	if e.LastReason == "" {
		t.Fatal("LastReason 未写入，面板只能显示笼统的「冷却中」")
	}
	// 终止态不看时钟：CoolDead 的 CoolUntil 必须为零值，否则 Status 会算成 cooling
	p.Cooldown("u1@ch", "m", CoolDead)
	if e := p.entries["u1@ch"]; !e.CoolUntil.IsZero() || e.Cool != CoolDead {
		t.Fatalf("CoolDead 后 CoolUntil=%v Cool=%s，期望零值/dead", e.CoolUntil, e.Cool)
	}
}

func TestCooldownModelEscalates(t *testing.T) {
	p, _ := newClockPool(t, time.Minute, 12*time.Hour)
	p.Upsert(Entry{UID: "u@c", ChannelID: "c"})
	p.NoteError("u@c")
	p.CooldownModel("u@c", "m1", 60*time.Second)
	p.CooldownModel("u@c", "m2", 90*time.Second)
	if e := p.entries["u@c"]; e.Cool != CoolNone {
		t.Fatalf("两个模型冷却就升级了：Cool=%s", e.Cool)
	}
	p.CooldownModel("u@c", "m3", 30*time.Second) // 第三个（仍有效）→ 升级
	e := p.entries["u@c"]
	if e.Cool != CoolSoft {
		t.Fatalf("升级后 Cool=%s, want soft", e.Cool)
	}
	if got, want := e.CoolUntil, p.now().Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("解冻时刻=%v, want 最晚的模型冷却 %v", got, want)
	}
	if len(e.ModelCool) != 0 || e.ErrCount != 0 {
		t.Fatalf("升级后未清空：modelCool=%d errCount=%d", len(e.ModelCool), e.ErrCount)
	}
}

func TestCooldownModelPrunesExpired(t *testing.T) {
	p, cur := newClockPool(t, time.Minute, 12*time.Hour)
	p.Upsert(Entry{UID: "u@c", ChannelID: "c"})
	p.CooldownModel("u@c", "m1", 10*time.Second)
	*cur = cur.Add(time.Minute) // m1 已解冻
	p.CooldownModel("u@c", "m2", time.Minute)
	p.CooldownModel("u@c", "m3", time.Minute)
	if e := p.entries["u@c"]; e.Cool != CoolNone {
		t.Fatalf("过期模型冷却被计入升级（旧版同款缺陷）：Cool=%s", e.Cool)
	}
}

// 升级整号冷却不得把已有的硬冷却（402）截短或降级——否则充值后 SetCreditSummary 不再解锁。
func TestCooldownModelKeepsHardKind(t *testing.T) {
	p, _ := newClockPool(t, time.Minute, 12*time.Hour)
	p.Upsert(Entry{UID: "u@c", ChannelID: "c"})
	p.Cooldown("u@c", "m", CoolHard)
	before := p.entries["u@c"].CoolUntil
	for _, m := range []string{"a", "b", "c"} {
		p.CooldownModel("u@c", m, time.Minute)
	}
	e := p.entries["u@c"]
	if e.Cool != CoolHard {
		t.Fatalf("硬冷却被降级成 %s", e.Cool)
	}
	if !e.CoolUntil.Equal(before) {
		t.Fatalf("硬冷却被截短：%v -> %v", before, e.CoolUntil)
	}
}

func TestClearChannelPenaltyOnlySingleAccount(t *testing.T) {
	p, _ := newClockPool(t, time.Minute, 12*time.Hour)
	p.Upsert(Entry{UID: "solo@a", ChannelID: "a"})
	p.Upsert(Entry{UID: "m1@b", ChannelID: "b"})
	p.Upsert(Entry{UID: "m2@b", ChannelID: "b"})
	p.Cooldown("solo@a", "", CoolHard)
	p.Cooldown("m1@b", "", CoolHard)
	p.ClearChannelPenalty("a")
	p.ClearChannelPenalty("b")
	if e := p.entries["solo@a"]; e.Cool != CoolNone {
		t.Fatalf("单账号渠道未自愈：Cool=%s", e.Cool)
	}
	if e := p.entries["m1@b"]; e.Cool != CoolHard {
		t.Fatalf("多账号渠道的冷却被误清：Cool=%s", e.Cool)
	}
}

func TestPersistRoundtrip(t *testing.T) {
	// 状态文件放 t.TempDir（Go 测试惯例，结束自动回收，不留在仓库里）
	path := filepath.Join(t.TempDir(), "pool-state.json")
	p, cur := newClockPool(t, time.Minute, 12*time.Hour)
	if err := p.SetPersist(path); err != nil {
		t.Fatalf("SetPersist: %v", err)
	}
	p.Upsert(Entry{UID: "u@c", ChannelID: "c", Nickname: "n", Credits: ptrf(10)})
	p.CooldownModel("u@c", "m1", time.Minute)
	p.Cooldown("u@c", "m1", CoolHard)
	p.Upsert(Entry{UID: "d@c", ChannelID: "c"})
	p.Cooldown("d@c", "", CoolDead)
	p.Upsert(Entry{UID: "x@c", ChannelID: "c"})
	p.Cooldown("x@c", "", CoolSoft) // 60s 软冷却，下面推进时钟让它过期

	p2 := newPoolAt(*cur)
	p2.Upsert(Entry{UID: "u@c", ChannelID: "c", Nickname: "n", Credits: ptrf(10)})
	p2.Upsert(Entry{UID: "d@c", ChannelID: "c"})
	p2.Upsert(Entry{UID: "x@c", ChannelID: "c"})
	if err := p2.SetPersist(path); err != nil {
		t.Fatalf("恢复 SetPersist: %v", err)
	}
	if e := p2.entries["u@c"]; e.Cool != CoolHard || e.CoolUntil.IsZero() || e.LastReason == "" {
		t.Fatalf("硬冷却未恢复：%+v", e)
	}
	if e := p2.entries["d@c"]; e.Cool != CoolDead || !e.CoolUntil.IsZero() {
		t.Fatalf("终止态未按原样恢复（时钟无关）：%+v", e)
	}
	if e := p2.entries["x@c"]; e.Cool != CoolSoft {
		t.Fatalf("未过期软冷却应恢复，得 %+v", e)
	}
	// 身份字段以 config 为准，恢复只回填运行时状态
	if e := p2.entries["u@c"]; e.Nickname != "n" || e.Credits == nil || *e.Credits != 10 {
		t.Fatalf("恢复时冲掉了身份字段：%+v", e)
	}
}

// 过期冷却不复活：重启后不应继续锁着一个早就到点的账号。
func TestPersistSkipsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool-state.json")
	base := time.Date(2026, 1, 1, 19, 7, 0, 0, time.UTC)
	p := New(time.Minute, 12*time.Hour)
	cur := base
	p.now = func() time.Time { return cur }
	p.SetPersist(path)
	p.Upsert(Entry{UID: "u@c", ChannelID: "c"})
	p.Cooldown("u@c", "", CoolSoft) // until = base+60s

	p2 := New(time.Minute, 12*time.Hour)
	later := base.Add(time.Hour)
	p2.now = func() time.Time { return later }
	if err := p2.SetPersist(path); err != nil {
		t.Fatalf("SetPersist: %v", err)
	}
	if e := p2.entries["u@c"]; e.Cool != CoolNone || !e.CoolUntil.IsZero() {
		t.Fatalf("过期软冷却被复活：%+v", e)
	}
}

func TestSetCreditSummaryOnlyClearsHard(t *testing.T) {
	p, _ := newClockPool(t, time.Minute, 12*time.Hour)
	p.Upsert(Entry{UID: "u@c", ChannelID: "c"})
	p.Cooldown("u@c", "", CoolSoft)
	p.SetCreditSummary("u@c", 100, "")
	if e := p.entries["u@c"]; e.Cool != CoolSoft {
		t.Fatalf("软冷却被充值解除（应只解 CoolHard）：%s", e.Cool)
	}
	p.Cooldown("u@c", "", CoolHard)
	p.SetCreditSummary("u@c", 100, "")
	if e := p.entries["u@c"]; e.Cool != CoolNone || e.ErrCount != 0 {
		t.Fatalf("充值后硬冷却未解除：%+v", e)
	}
}

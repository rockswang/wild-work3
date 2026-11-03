package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"wild-work3/main/core/internal/config"
)

func TestParseClock(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"09:00", 540, true}, {"9:05", 545, true}, {" 21:30 ", 1290, true},
		{"24:00", 0, false}, {"12:60", 0, false}, {"", 0, false}, {"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := parseClock(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("parseClock(%q) = %d,%v 期望 %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSlotKeyDistinct(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local)
	// 一天两个时段必须互不吞并（键含起始分钟）
	if slotKey(now, 540) == slotKey(now, 1260) {
		t.Fatal("不同时段的 slotKey 不能相同")
	}
	// 不同天同起始分钟必须不同
	if slotKey(now, 540) == slotKey(now.AddDate(0, 0, 1), 540) {
		t.Fatal("不同天的 slotKey 不能相同")
	}
}

func TestPruneKeepsTodayAndYesterday(t *testing.T) {
	s := New(nil, nil, nil, nil)
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	s.done["2026-11-01@540"] = true
	s.done["2026-10-31@540"] = true
	s.done["2026-10-30@540"] = true
	s.mu.Lock()
	s.pruneLocked(now)
	s.mu.Unlock()
	if !s.done["2026-11-01@540"] || !s.done["2026-10-31@540"] {
		t.Fatal("今天与昨天的键必须保留")
	}
	if s.done["2026-10-30@540"] {
		t.Fatal("前天的键必须清理")
	}
}

// 签到时段：到点触发一次；成功（failed==0）后当天不再触发。
func TestCheckinTickFiresOncePerSlot(t *testing.T) {
	var calls int32
	fake := &fakeTasks{checkin: func() (int, int) { atomic.AddInt32(&calls, 1); return 1, 0 }}
	st, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Get()
	cfg.CheckinTimes = []string{"09:00"}
	_ = st.Save()

	s := New(st, nil, nil, fake)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local)
	s.checkinTick(context.Background(), at)
	if calls != 1 {
		t.Fatalf("到点应触发 1 次，实际 %d", calls)
	}
	// 同一时段再来一次：已完成，不触发
	s.checkinTick(context.Background(), at.Add(30*time.Second))
	if calls != 1 {
		t.Fatalf("已完成的时段不应再触发，实际 %d", calls)
	}
}

// 签到失败后会在窗口内重试，但受 checkinRetryGap 节流。
func TestCheckinRetryThrottled(t *testing.T) {
	var calls int32
	fake := &fakeTasks{checkin: func() (int, int) { atomic.AddInt32(&calls, 1); return 0, 1 }}
	st, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Get().CheckinTimes = []string{"09:00"}
	_ = st.Save()
	s := New(st, nil, nil, fake)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local)
	s.checkinTick(context.Background(), at)                  // 首次
	s.checkinTick(context.Background(), at.Add(time.Minute)) // 太近，被节流
	if calls != 1 {
		t.Fatalf("重试应被节流，实际触发 %d 次", calls)
	}
	s.checkinTick(context.Background(), at.Add(checkinRetryGap+time.Second)) // 允许重试
	if calls != 2 {
		t.Fatalf("超过间隔应重试，实际触发 %d 次", calls)
	}
}

// 窗口外（下午）不重试。
func TestCheckinNoRetryOutsideWindow(t *testing.T) {
	var calls int32
	fake := &fakeTasks{checkin: func() (int, int) { atomic.AddInt32(&calls, 1); return 0, 1 }}
	st, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Get().CheckinTimes = []string{"09:00"}
	_ = st.Save()
	s := New(st, nil, nil, fake)
	s.checkinTick(context.Background(), time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local))
	before := calls
	s.checkinTick(context.Background(), time.Date(2026, 11, 1, 15, 0, 0, 0, time.Local))
	if calls != before {
		t.Fatalf("窗口外不应重试，实际多触发 %d 次", calls-before)
	}
}

type fakeTasks struct {
	checkin func() (int, int)
}

func (f *fakeTasks) RefreshAccounts(context.Context) (int, int) { return 0, 0 }
func (f *fakeTasks) Checkin(context.Context) (int, int) {
	if f.checkin == nil {
		return 0, 0
	}
	return f.checkin()
}
func (f *fakeTasks) RefreshCredits(context.Context) (int, int) { return 0, 0 }

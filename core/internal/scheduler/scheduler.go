// Package scheduler 定时任务调度：每日签到、凭据保活、积分明细自动刷新。
//
// 设计边界（W3-13 边界 A）：调度器只负责「什么时候做」，不关心「怎么做」——
// 具体动作由 core 管理面（api.Server）通过 Tasks 接口提供。这样调度逻辑可以
// 单独测试，也不会把 api 包拖进调度器形成循环依赖。
//
// 移植自旧版 dev/internal/scheduler/scheduler.go + dev/internal/app/app.go 的
// StartCreditAutoRefresh。差异：
//   - 旧版按渠道各起一个 scheduler（渠道进程内持有凭据）；ww3 里凭据归插件，
//     core 只按「渠道实例」批量调用，故收敛成一个全局调度器；
//   - 签到重试间隔从「每分钟」放宽到 10 分钟：真实上游下每次重试要拉起
//     N 个一次性进程，每分钟重试等于对同一上游持续加压（§9 红线）。
package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/pool"
	"wild-work3/main/protocol/cli"
)

// Tasks 调度器需要的三类动作（由 api.Server 实现）。
type Tasks interface {
	// RefreshAccounts 凭据续期（保活）。返回成功/失败数。
	RefreshAccounts(ctx context.Context) (ok, failed int)
	// Checkin 每日签到（只对声明了 accounts.checkin 的渠道发）。返回成功/失败数。
	Checkin(ctx context.Context) (ok, failed int)
	// RefreshCredits 拉取额度明细 → 差分记账 + 回写选号摘要。返回成功/失败数。
	RefreshCredits(ctx context.Context) (ok, failed int)
}

const (
	// CreditRefreshInterval 积分/额度明细自动刷新间隔（旧版 CreditRefreshInterval=10min，
	// 2026-10-20 由 30min 调至 10min：签到发放与消耗的刷新窗越短，差分越准）。
	CreditRefreshInterval = 10 * time.Minute
	// tick 调度精度：每 30s 检查一次「有没有到点」，比精确 timer 更抗系统休眠/时钟跳变。
	tick = 30 * time.Second
	// checkinRetryGap 同一签到时段两次重试之间的最小间隔（旧版是每分钟，见包注释）。
	checkinRetryGap = 10 * time.Minute
	// checkinRetryUntilDefault 签到重试窗口上界缺省值（当日分钟数，旧版 CheckinRetryUntil 默认 12:00）。
	// 可经 core.json 的 checkin_retry_until 覆盖；0 = 关闭重试。见 config.Config.CheckinRetryUntil。
	checkinRetryUntilDefault = 12 * 60
	// checkinTimeout 单次签到批量调用的超时。
	checkinTimeout = 10 * time.Minute
	// creditTimeout 单轮积分刷新的超时。
	creditTimeout = 5 * time.Minute
)

// Scheduler 全局定时任务。
type Scheduler struct {
	store *config.Store
	pool  *pool.Pool
	log   *cli.Logger
	tasks Tasks

	mu       sync.Mutex
	done     map[string]bool      // 已完成/已尝试的签到时段键（"YYYY-MM-DD@起始分钟"）
	lastTry  map[string]time.Time // 上次尝试时刻（节流用）
	keepFire map[string]bool      // 已执行的保活小时键（"YYYY-MM-DD@15"）
}

// New 创建调度器。log 可为 nil（静默）。
func New(store *config.Store, p *pool.Pool, log *cli.Logger, tasks Tasks) *Scheduler {
	return &Scheduler{store: store, pool: p, log: log, tasks: tasks,
		done: map[string]bool{}, lastTry: map[string]time.Time{}, keepFire: map[string]bool{}}
}

// Run 启动全部定时任务，阻塞直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) {
	cfg := s.store.Get()
	s.logf("调度器启动：签到时段 %v，保活小时 %v，积分刷新 %s", cfg.CheckinTimes, cfg.KeepaliveHours, CreditRefreshInterval)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); s.runCredits(ctx) }()
	go func() { defer wg.Done(); s.runKeepalive(ctx) }()
	go func() { defer wg.Done(); s.runCheckin(ctx) }()
	wg.Wait()
}

// runCredits 启动立即刷一次，之后每 CreditRefreshInterval 一次（旧版 StartCreditAutoRefresh）。
func (s *Scheduler) runCredits(ctx context.Context) {
	run := func() {
		c, cancel := context.WithTimeout(ctx, creditTimeout)
		defer cancel()
		ok, failed := s.tasks.RefreshCredits(c)
		if ok+failed > 0 {
			s.logf("积分自动刷新：成功 %d，失败 %d", ok, failed)
		}
	}
	run()
	t := time.NewTicker(CreditRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// runCheckin 每 tick 检查一次签到时段是否到点。
func (s *Scheduler) runCheckin(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.checkinTick(ctx, now)
		}
	}
}

// checkinTick 处理「当前这一分钟」应当执行的签到时段。
//
// 时段语义（沿用旧版）：start 时刻先试一次；失败则在 [start+1, retryUntil] 内
// 每 checkinRetryGap 重试，直到成功（failed==0）或窗口关闭。当天已完成的时段
// 不再触发——键含起始分钟，故一天多个时段互不吞并。
func (s *Scheduler) checkinTick(ctx context.Context, now time.Time) {
	starts := parseTimes(s.store.Get().CheckinTimes)
	if len(starts) == 0 {
		return
	}
	minute := now.Hour()*60 + now.Minute()
	for _, start := range starts {
		if start < 0 {
			continue
		}
		slot := slotKey(now, start)
		s.mu.Lock()
		done := s.done[slot]
		last := s.lastTry[slot]
		s.mu.Unlock()
		if done {
			continue
		}
		// 重试窗口上界取配置（checkin_retry_until，分钟）：显式 0 = 关闭重试；负值退回默认 720。
		until := s.store.Get().CheckinRetryUntil
		if until < 0 {
			until = checkinRetryUntilDefault
		}
		due := minute == start ||
			(until > start && minute > start && minute <= until)
		if !due {
			continue
		}
		if !last.IsZero() && now.Sub(last) < checkinRetryGap {
			continue
		}
		s.mu.Lock()
		s.lastTry[slot] = now
		s.mu.Unlock()

		c, cancel := context.WithTimeout(ctx, checkinTimeout)
		ok, failed := s.tasks.Checkin(c)
		cancel()
		s.logf("签到时段 %02d:%02d：成功 %d，失败 %d", start/60, start%60, ok, failed)
		if failed == 0 {
			s.mu.Lock()
			s.done[slot] = true
			s.pruneLocked(now)
			s.mu.Unlock()
		}
	}
}

// runKeepalive 每 tick 检查一次「当前小时是否在保活列表里」。
func (s *Scheduler) runKeepalive(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			hours := s.store.Get().KeepaliveHours
			hit := false
			for _, h := range hours {
				if h == now.Hour() {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			key := now.Format("2006-01-02") + "@" + strconv.Itoa(now.Hour())
			s.mu.Lock()
			fired := s.keepFire[key]
			if !fired {
				s.keepFire[key] = true
			}
			s.pruneLocked(now)
			s.mu.Unlock()
			if fired {
				continue
			}
			c, cancel := context.WithTimeout(ctx, creditTimeout)
			ok, failed := s.tasks.RefreshAccounts(c)
			cancel()
			s.logf("保活续期：成功 %d，失败 %d", ok, failed)
		}
	}
}

// pruneLocked 只保留今天与昨天的时段键（旧版 markSlotDone 语义），防止 map 无界增长。
func (s *Scheduler) pruneLocked(now time.Time) {
	today := now.Format("2006-01-02")
	yday := now.AddDate(0, 0, -1).Format("2006-01-02")
	for k := range s.done {
		if !strings.HasPrefix(k, today) && !strings.HasPrefix(k, yday) {
			delete(s.done, k)
		}
	}
	for k := range s.lastTry {
		if !strings.HasPrefix(k, today) && !strings.HasPrefix(k, yday) {
			delete(s.lastTry, k)
		}
	}
	for k := range s.keepFire {
		if !strings.HasPrefix(k, today) && !strings.HasPrefix(k, yday) {
			delete(s.keepFire, k)
		}
	}
}

// slotKey 签到时段键：日期 + 起始分钟（一天多个时段互不吞并）。
func slotKey(now time.Time, start int) string {
	return now.Format("2006-01-02") + "@" + strconv.Itoa(start)
}

// parseTimes 把 ["09:00","21:00"] 解析成当日分钟数（-1 = 格式非法，跳过）。
func parseTimes(ss []string) []int {
	out := make([]int, 0, len(ss))
	for _, s := range ss {
		if m, ok := parseClock(s); ok {
			out = append(out, m)
		}
	}
	return out
}

// parseClock 解析 "HH:MM"（也接受 "H:MM"），返回当日分钟数。
func parseClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ':')
	if i <= 0 || i > 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:i])
	m, err2 := strconv.Atoi(s[i+1:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func (s *Scheduler) logf(format string, a ...any) {
	if s.log != nil {
		s.log.Infof(format, a...)
	}
}

// Describe 生成一行人类可读的调度摘要（供 /api/state 展示）。
func (s *Scheduler) Describe() string {
	cfg := s.store.Get()
	return fmt.Sprintf("签到 %v / 保活 %v 时 / 积分刷新 %s",
		cfg.CheckinTimes, cfg.KeepaliveHours, CreditRefreshInterval)
}

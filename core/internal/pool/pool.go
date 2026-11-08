// Package pool 账号池：冷却状态机 + 账号选择（沿用旧版 pool 语义）。
package pool

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// CoolKind 冷却类型。
type CoolKind string

const (
	CoolNone    CoolKind = ""
	CoolSoft    CoolKind = "soft"     // 短冷却（限流/偶发）
	CoolHard    CoolKind = "hard"     // 长冷却（余额不足）
	CoolDead    CoolKind = "dead"     // 登录态失效，需重新登录
	CoolDisable CoolKind = "disabled" // 人工停用
)

// Entry 一个账号的池内状态。
type Entry struct {
	UID       string
	ChannelID string
	Nickname  string
	Credits   *float64 // nil = 未知
	Unit      string
	ExpireAt  string // earliestExpire（路由键之一，YYYY-MM-DD）

	Cool      CoolKind
	CoolUntil time.Time
	// (账号,模型) 级冷却/负缓存
	ModelCool map[string]time.Time
	// ErrCount 连续错误计数（旧版 pool.NoteError）：达 threshold 后打一次短冷却并清零。
	ErrCount int
	// LastReason 最近一次冷却/停用的可读原因（仅展示与日志，不参与调度）。
	LastReason string
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	entries map[string]*Entry // uid -> entry
	soft    time.Duration
	hard    time.Duration
	// errCool 连续错误达 errThreshold 次的短冷却时长（旧版 ErrCooldown，默认 10m）
	errCool      time.Duration
	errThreshold int
	// sticky 粘性路由：渠道实例 ID -> 上次选中的账号（旧版 pickWithSticky 语义）
	sticky map[string]*stickyEntry
	now    func() time.Time
	// path 状态文件（旧版 state.json）；空 = 不持久化（单测/CLI）
	path string
}

// stickyEntry 粘性记录：上次路由账号 + 已连续成功请求数。
//
// 不用 credits 做轮换依据：池内余额只在签到/手动刷新时更新，对话后是 stale 数据。
// 改用请求计数：连续 maxReqs 次后主动换号，让同渠道多账号的额度消耗更均匀。
type stickyEntry struct {
	uid      string
	reqCount int
	maxReqs  int
}

// DefaultStickyMaxReqs 粘性路由的默认轮换阈值（旧版 defaultMaxReqs = 50）。
const DefaultStickyMaxReqs = 50

// New 新建池（冷却时长）。
func New(soft, hard time.Duration) *Pool {
	return &Pool{entries: map[string]*Entry{}, soft: soft, hard: hard,
		errCool: 10 * time.Minute, errThreshold: 3, sticky: map[string]*stickyEntry{}, now: time.Now}
}

// SetErrCooldown 设置「连续错误达阈值」的短冷却时长与阈值（≤0 保留默认）。
func (p *Pool) SetErrCooldown(d time.Duration, threshold int) {
	p.Configure(p.soft, p.hard, d, threshold)
}

// Configure 应用全部冷却参数（启动时与面板改动后都走这里）；≤0 视为未提供，保留当前值。
// 池子原本只在 New 时读一次配置，面板改了不重启就不生效，故必须有个可变入口。
func (p *Pool) Configure(soft, hard, errCool time.Duration, errThreshold int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if soft > 0 {
		p.soft = soft
	}
	if hard > 0 {
		p.hard = hard
	}
	if errCool > 0 {
		p.errCool = errCool
	}
	if errThreshold > 0 {
		p.errThreshold = errThreshold
	}
}

// Upsert 添加/更新账号。
func (p *Pool) Upsert(e Entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old, ok := p.entries[e.UID]
	if !ok {
		if e.ModelCool == nil {
			e.ModelCool = map[string]time.Time{}
		}
		p.entries[e.UID] = &e
		p.saveLocked()
		return
	}
	// 只刷新身份/额度类字段，**不动** Cool/CoolUntil/ModelCool/ErrCount：
	// 面板每几分钟就 SyncAccounts 一次，洗掉冷却会重启刚刚碰壁的重试风暴。
	old.Nickname, old.Credits, old.Unit, old.ExpireAt, old.ChannelID = e.Nickname, e.Credits, e.Unit, e.ExpireAt, e.ChannelID
	p.saveLocked()
}

// Remove 移除账号。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, uid)
	p.saveLocked()
}

// List 返回快照（拷贝，避免外部改内部状态）。
func (p *Pool) List() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Entry, 0, len(p.entries))
	for _, e := range p.entries {
		c := *e
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// healthy 报告账号整体是否可用。
func (p *Pool) healthy(e *Entry, t time.Time) bool {
	if e.Cool == CoolDisable || e.Cool == CoolDead {
		return false
	}
	if e.Cool != CoolNone && t.Before(e.CoolUntil) {
		return false
	}
	return true
}

// healthyForModel 报告账号在某模型上是否可用。
func (p *Pool) healthyForModel(e *Entry, model string, t time.Time) bool {
	if !p.healthy(e, t) {
		return false
	}
	if until, ok := e.ModelCool[model]; ok && t.Before(until) {
		return false
	}
	return true
}

// Pick 选一个健康账号（同渠道优先、额度高者优先、再到期早者优先）。
func (p *Pool) Pick(channelID, model string, exclude map[string]bool) *Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pickLocked(channelID, model, exclude, p.now())
}

// PickSticky 带粘性的选号（旧版 pickWithSticky）：
//
//   - 优先返回该渠道上次选中的账号，直到它冷却/禁用/登录态失效/对该模型模型级冷却，
//     或已连续成功 DefaultStickyMaxReqs 次（此时主动换号均摊额度）；
//   - 否则降级为 Pick 并按新账号重建粘性记录。
//
// 粘性解决的是「同一会话的多轮请求命中不同账号 ⇒ 上游会话态/计费口径乱跳」，
// 但**不可无限粘住**：stale 余额会导致某个账号被反复抽干。
func (p *Pool) PickSticky(channelID, model string, exclude map[string]bool) *Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.now()
	if st := p.sticky[channelID]; st != nil && st.uid != "" && st.reqCount < st.maxReqs {
		if e := p.entries[st.uid]; e != nil && e.ChannelID == channelID && !exclude[e.UID] && p.healthyForModel(e, model, t) {
			c := *e
			return &c
		}
		// 粘住的账号已不可用：清掉记录，下面重新选
		delete(p.sticky, channelID)
	}
	e := p.pickLocked(channelID, model, exclude, t)
	if e != nil {
		p.sticky[channelID] = &stickyEntry{uid: e.UID, maxReqs: DefaultStickyMaxReqs}
	}
	return e
}

// NoteSuccess 成功一次：粘性请求计数 +1，并清零连续错误计数。
func (p *Pool) NoteSuccess(uid string, channelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		e.ErrCount = 0
	}
	if st := p.sticky[channelID]; st != nil && st.uid == uid {
		st.reqCount++
	}
}

// ClearSticky 清除某渠道的粘性记录（选中的账号失败/冷却时调用，下次强制重选）。
func (p *Pool) ClearSticky(channelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sticky, channelID)
}

// NoteError 记一次账号级错误；连续达阈值后打一次短冷却并清零计数。
//
// 只有「默认类」错误（既非确定性结论也非请求自身问题）才该累计——否则一次客户端
// 重试风暴就能在几分钟内把全池账号记满冷却（旧版实测事故，见 handler.go:929 注释）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	e.ErrCount++
	if e.ErrCount >= p.errThreshold {
		e.Cool, e.CoolUntil, e.ErrCount = CoolSoft, p.now().Add(p.errCool), 0
		e.LastReason = fmt.Sprintf("连续错误达 %d 次", p.errThreshold)
	}
	p.saveLocked()
}

// SetCreditSummary 用插件上报的额度明细回写选号摘要（额度合计 + 最早到期日）。
//
// 与 SetCreditDetail 的区别：这里只覆盖 credits 与 expireAt 两个**路由排序键**，
// 不碰错误计数，也不会把人工停用（CoolDisable）或登录态失效（CoolDead）洗掉。
// 唯一例外：硬冷却（402 余额不足）在确认有额度后自动解除（旧版 ReenableIfCredits）
// ——否则充值后还要白等 12h。
func (p *Pool) SetCreditSummary(uid string, credits float64, expireAt string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		c := credits
		e.Credits, e.ExpireAt = &c, expireAt
		// 余额已恢复：解除硬冷却（只针对 CoolHard，停用/登录态失效不在此列）
		if credits > 0 && e.Cool == CoolHard {
			e.Cool, e.CoolUntil, e.ErrCount, e.LastReason = CoolNone, time.Time{}, 0, ""
		}
		p.saveLocked()
	}
}

// CountChannel 返回某渠道实例下的账号数（换号次数上限用它，不写死常数）。
func (p *Pool) CountChannel(channelID string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.entries {
		if e.ChannelID == channelID {
			n++
		}
	}
	return n
}

// HasCapacity 报告该渠道在该模型上是否还有可承载账号（旧版 modelHasCapacity）。
//
// 用途：① 入口降级判定「模型在本渠道是否已全冷」；② 恢复探针判定「原模型是否已恢复」。
// channelID 为空时跨全部渠道判定（降级备胎可能落在别的渠道）。
func (p *Pool) HasCapacity(channelID, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t := p.now()
	for _, e := range p.entries {
		if channelID != "" && e.ChannelID != channelID {
			continue
		}
		if p.healthyForModel(e, model, t) {
			return true
		}
	}
	return false
}

// pickLocked 选号主体（调用方持锁）。
func (p *Pool) pickLocked(channelID, model string, exclude map[string]bool, t time.Time) *Entry {
	var cands []*Entry
	for _, e := range p.entries {
		if channelID != "" && e.ChannelID != channelID {
			continue
		}
		if exclude[e.UID] {
			continue
		}
		if !p.healthyForModel(e, model, t) {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return better(cands[i], cands[j], t) })
	c := *cands[0]
	return &c
}

// better：有效到期日早者优先 → uid asc → credits desc（沿用旧版 entryBetter）。
//
// ⚠️ 空串/已过期的 ExpireAt 必须**排最后**：早期实现直接字符串比较，空串最小，
// 于是「没有额度到期信息」的账号恒排第一，每次请求都先白打一次它。
func better(a, b *Entry, now time.Time) bool {
	ae, be := expireOK(a, now), expireOK(b, now)
	if ae != be {
		return ae
	}
	if ae {
		if a.ExpireAt != b.ExpireAt {
			return a.ExpireAt < b.ExpireAt
		}
		return a.UID < b.UID
	}
	ac, bc := credits(a), credits(b)
	if ac != bc {
		return ac > bc
	}
	return a.UID < b.UID // map 迭代序随机，无 tie-break 则选号不可复现
}

// expireOK 报告账号是否有**仍然有效**的额度最早到期日（YYYY-MM-DD）。
func expireOK(e *Entry, now time.Time) bool {
	if e.ExpireAt == "" {
		return false
	}
	d, err := time.Parse("2006-01-02", e.ExpireAt)
	if err != nil {
		return false
	}
	// 按当天结束算有效：到期日当天额度仍然可用
	return !d.Add(24 * time.Hour).Before(now)
}

func credits(e *Entry) float64 {
	if e.Credits == nil {
		return -1 // 未知额度排最后
	}
	return *e.Credits
}

// Cooldown 按错误分类给账号（或账号+模型）打冷却。
func (p *Pool) Cooldown(uid, model string, kind CoolKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	t := p.now()
	switch kind {
	case CoolDead, CoolDisable:
		e.Cool, e.CoolUntil, e.ErrCount = kind, time.Time{}, 0 // 终止态不看时钟，计数同时归零
		e.LastReason = reasonOf(kind)
	default:
		d := p.soft
		if kind == CoolHard {
			d = p.hard
		}
		e.Cool, e.CoolUntil = kind, t.Add(d)
		e.LastReason = reasonOf(kind)
		// 旧版口径：打冷却即视为本轮错误已处置，连错计数归零（旧版 Cooldown 里 errCount=0），
		// 否则解冻后的第一次失败就会立刻再触达阈值。
		e.ErrCount = 0
	}
	p.saveLocked()
}

// reasonOf 冷却类型对应的可读原因（面板/日志用，不参与调度）。
func reasonOf(k CoolKind) string {
	switch k {
	case CoolHard:
		return "额度/权益不足"
	case CoolSoft:
		return "限流或临时失败"
	case CoolDead:
		return "登录态失效，需重新登录"
	case CoolDisable:
		return "人工停用"
	}
	return ""
}

// CooldownFor 同 Cooldown，但可指定时长（R73「硬冷却截到下一个每日发奖窗口」用）。
// d<=0 时退化为按 kind 取默认时长。
func (p *Pool) CooldownFor(uid, model string, kind CoolKind, d time.Duration) {
	if d <= 0 {
		p.Cooldown(uid, model, kind)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	if kind == CoolDead || kind == CoolDisable { // 终止态不接受时长（否则会被自动解冻）
		e.Cool, e.CoolUntil, e.ErrCount, e.LastReason = kind, time.Time{}, 0, reasonOf(kind)
		p.saveLocked()
		return
	}
	e.Cool, e.CoolUntil, e.ErrCount, e.LastReason = kind, p.now().Add(d), 0, reasonOf(kind)
	p.saveLocked()
}

// ClearPenalty 清除一个账号的**时间性**冷却（硬/软），人工停用与登录态失效不受影响。
//
// 用途：单账号渠道（API Key / 匿名池）启动时自愈——这类渠道任何账号级惩罚都等于整条
// 渠道下线，上次进程留下的脏冷却（甚至同一时刻误判的 402）不应继续锁死（旧版 pool.ClearPenalty）。
// ClearChannelPenalty 清除某渠道实例下所有账号的时间性冷却（硬/软）。
// 仅当该渠道只有一个账号时才生效：单账号渠道（API Key/匿名）任何惩罚都等于整条渠道下线，
// 冷却没有信息量且会跨重启锁死（旧版 pool.ClearPenalty 的动机，oczen 类实测）。
// 多账号渠道的冷却是真实限流信号，清掉会重启重试风暴，故不动。
// 注：不能转调 p.CountChannel（RWMutex 不可重入，这里已持写锁），故就地计数。
func (p *Pool) ClearChannelPenalty(channelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.entries {
		if e.ChannelID == channelID {
			n++
		}
	}
	if n != 1 {
		return
	}
	changed := false
	for _, e := range p.entries {
		if e.ChannelID != channelID || (e.Cool != CoolHard && e.Cool != CoolSoft) {
			continue
		}
		e.Cool, e.CoolUntil, e.ErrCount, e.ModelCool, e.LastReason = CoolNone, time.Time{}, 0, nil, ""
		changed = true
	}
	if changed {
		p.saveLocked()
	}
}

// Now 返回池子的当前时刻（测试注入的时钟），供上层做「截到发奖窗口」这类时长计算。
func (p *Pool) Now() time.Time { return p.now() }

// HardCooldown 返回一次硬冷却（402 余额/权益不足）的时长。
func (p *Pool) HardCooldown() time.Duration { return p.hard }

// SoftCooldown 返回一次软冷却（429 限流）的时长。
func (p *Pool) SoftCooldown() time.Duration { return p.soft }

// CooldownModel 给 (账号,模型) 打负缓存；**仍然在未来**的模型级冷却达 3 个 → 升级整账号冷却。
//
// 与旧版 dev/internal/pool/pool.go CooldownModel 同口径：先剪掉过期条目再计数，
// 升级时清空 modelCool 与连错计数（旧版 modelCool=nil / errCount=0）。
// 刻意偏离旧版的一处：解冻时刻取 max(现有, latest)——旧版无条件写 until=latest，
// 会把一个更长的既有硬冷却截短（等于提前解冻），这里不接受该退化。
func (p *Pool) CooldownModel(uid, model string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	now := p.now()
	if e.ModelCool == nil {
		e.ModelCool = map[string]time.Time{}
	}
	e.ModelCool[model] = now.Add(d)
	// 统计仍在未来的条目并顺手清理过期项（不用单调递增计数器：
	// 那会把已解冻的模型也算进去，导致偶发撞几次的账号被误升级整号冷却）。
	active, latest := 0, time.Time{}
	for m, until := range e.ModelCool {
		if !now.Before(until) {
			delete(e.ModelCool, m)
			continue
		}
		active++
		if until.After(latest) {
			latest = until
		}
	}
	if active < 3 {
		return
	}
	// 升级整账号冷却；人工停用/登录态失效是终止态，不得被降级成软冷却
	if e.Cool == CoolDisable || e.Cool == CoolDead {
		return
	}
	if latest.After(e.CoolUntil) {
		e.CoolUntil = latest
	}
	// 升级时不把已有的硬冷却（402）降级成软冷却：否则充值后 SetCreditSummary 不再解锁，
	// 且「多模型限流」这个信息量更低的理由会掩盖额度不足。只有无冷却/软冷却才转 CoolSoft。
	if e.Cool != CoolHard {
		e.Cool = CoolSoft
		e.LastReason = "多模型接连限流，升级整号冷却"
	}
	e.ModelCool = nil
	e.ErrCount = 0
	p.saveLocked()
}

// ClearSessionDead 登录态恢复后清除 dead 标记（自愈）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok && e.Cool == CoolDead {
		e.Cool, e.CoolUntil, e.LastReason = CoolNone, time.Time{}, ""
		p.saveLocked()
	}
}

// Status 返回账号对外状态字符串。
func Status(e Entry, now time.Time) string {
	switch {
	case e.Cool == CoolDisable:
		return "disabled"
	case e.Cool == CoolDead:
		return "session_dead"
	case e.Cool != CoolNone && now.Before(e.CoolUntil):
		return "cooling"
	}
	return "ok"
}

// RenameChannel 渠道实例改名时整体迁移池内归属（W3-45）：条目 UID、ChannelID 与粘性记录。
//
// 必须在**改配置之前或之后立刻**调用：池里的账号主键与配置里的 UID 必须始终一致，
// 否则重启后 SyncAccounts 加载配置能对上、而本进程内存里的旧 UID 会被当成「配置里已
// 不存在」而清掉，导致冷却状态丢失、积分账号变幽灵。
//
// 池状态会被持久化（persist.go），故这里改完即落盘：崩溃后恢复到新键，不会留半迁移状态。
func (p *Pool) RenameChannel(oldID, newID string) {
	if oldID == "" || newID == "" || oldID == newID {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make(map[string]*Entry, len(p.entries))
	for uid, e := range p.entries {
		if e.ChannelID == oldID {
			e.ChannelID = newID
			uid = renamingUID(uid, oldID, newID)
			e.UID = uid // Entry.UID 是独立字段，不跟着改会让 List/Pick 返回旧主键（冷却归错人）
		}
		next[uid] = e
	}
	p.entries = next
	if st := p.sticky[oldID]; st != nil {
		delete(p.sticky, oldID)
		p.sticky[newID] = st
	}
	p.saveLocked()
}

// renamingUID 账号主键迁移：虚拟账号 UID 恰为渠道 ID，其余形如 `<remoteUID>@<channelID>`。
func renamingUID(uid, oldID, newID string) string {
	if uid == oldID {
		return newID
	}
	if len(uid) > len(oldID)+1 && uid[len(uid)-len(oldID)-1] == '@' && uid[len(uid)-len(oldID):] == oldID {
		return uid[:len(uid)-len(oldID)] + newID
	}
	return uid
}

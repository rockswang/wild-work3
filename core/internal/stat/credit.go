// credit.go 积分条目快照差分：把每次余额刷新看到的 ResourceItem 列表与上次快照对比，
// 产出 earn/spend/expire 三类流水事件（移植自旧版 dev/internal/ledger/credit.go）。
//
// 分类规则（与旧版定稿一致，不追求逐笔精确）：
//   - 同 key 条目余额上升          → earn（签到/活动发放/套餐充值）
//   - 同 key 条目余额下降且到期日已过 → expire（到期作废）
//   - 同 key 条目余额下降且未到期    → spend（对话消耗）
//   - 上次有、本次消失的 key        → expire（套餐/赠送包到期后从明细里整体移除是主因）
//   - 本次新出现的 key             → earn（新发放的套餐/赠送包）
//
// 同 key 多条目先聚合求和再差分（issue #38）：WorkBuddy 的「套餐名|到期日」伪键
// 可能对应多条独立套餐，逐条差分会每条都对比同一份旧快照 → 一次刷新重复记
// spend、快照只留末条下次继续错。
//
// key 来源：ResourceItem.Key（渠道设置的稳定标识），缺失时退回 Name。
// 快照持久化在 stat 目录的 credit-snapshot.json（可随时删除——丢了只是下次差分
// 把全部条目误记一次 earn，无功能影响）。
package stat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"wild-work3/main/protocol/meta"
)

// itemSnap 快照里的单条目（只存差分所需字段）。
type itemSnap struct {
	Key    string  `json:"k"`
	Remain float64 `json:"r"`
	Expire string  `json:"e,omitempty"` // YYYY-MM-DD（UTC+8 墙钟口径）
	// Used 已消耗量（issue #67）：delta=0 时用于识别「发放即消耗」形态——
	// WorkBuddy 每日签到包提前一天以 r=0 建档，发放与消耗落在同一刷新窗内时
	// remain 与快照持平，earn 只能靠 used 差分救回。旧快照无此字段 → 零值，安全。
	Used float64 `json:"u,omitempty"`
	// Total 池子总量（R101）：remain 下降时用来区分「池子缩水（整包到期/回收）」
	// 与「用掉更多」——remain = total - used 是三者的恒等式，故
	// Δremain = ΔTotal - ΔUsed。旧快照无此字段 → 零值，配合 Total>0 门槛
	// 保证老数据一律退回旧口径。
	Total float64 `json:"t,omitempty"`
}

// snapChannels 快照文件里保存「渠道能力」的保留键。真实账号键恒为
// "ch/uid"（含斜杠），故 "@ch" 不会与之冲突。
//
// 为什么能力记在渠道级而不是账号/key 级（R101）：「这个渠道的 total 是真面额还是
// remain 的拷贝」是**渠道属性**。按单 key 判会把「从不花钱、只签到」的账号
// （QoderCN 实录：total 正好等于累计发放，used 恒 0）误当成「不区分 total/used」
// 的渠道，它的整包到期就仍会记成消耗。反过来只要该渠道**任一账号任一 key**
// 花过一次（used>0），就证明它会给 total 与 used 分别赋值，全渠道永久受益。
const snapChannels = "@ch"

// snapshots credit-snapshot.json 结构：key "ch/uid" → 条目列表。
type snapshots map[string][]itemSnap

// expireLoc 与旧版 provider.ExpiringWithin 同口径：固定 UTC+8 墙钟。
var expireLoc = time.FixedZone("UTC+8", 8*60*60)

// snapshotPath 快照文件路径。
func (l *Book) snapshotPath() string { return filepath.Join(l.dir, "credit-snapshot.json") }

// loadSnapshots 读快照文件（不存在/损坏返回空表，不阻塞）。
func (l *Book) loadSnapshots() snapshots {
	out := snapshots{}
	if raw, err := os.ReadFile(l.snapshotPath()); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// saveSnapshots 原子写快照文件（tmp+rename，0600）。
func (l *Book) saveSnapshots(s snapshots) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	tmp := l.snapshotPath() + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, l.snapshotPath())
	}
}

// itemKey 条目差分键：优先渠道提供的稳定 Key（如 entitlement_id），退回 Name。
func itemKey(it meta.ResourceItem) string {
	if it.Key != "" {
		return it.Key
	}
	return it.Name
}

// ExpireInstant 把 ExpireAt 归一化成**失效时刻**（UTC+8 墙钟）。
//
//   - "YYYY-MM-DD"：该日全天可用，**次日 00:00** 才失效 ⇒ 当天不算过期；
//   - RFC3339：精确时刻，到点即失效。
//
// 历史坑（旧版 2026-10-18 生产实录，两个方向都错）：把「到期日 = 今天」判成已过期
// （当日有效池的消耗被记成 expire，loomy 每日积分 / qwenwork 当日钱包都中招）；
// 对 RFC3339 解析失败后恒 false（被当成「永不过期」）。两处都在这里收口。
// 空串或不可解析 → ok=false（渠道未下发，如 Qoder）。
func ExpireInstant(v string) (time.Time, bool) {
	v = trimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if len(v) >= 10 && v[4] == '-' && v[7] == '-' {
		if d, err := time.ParseInLocation("2006-01-02", v[:10], expireLoc); err == nil {
			return d.AddDate(0, 0, 1), true // 到期日当天仍可用
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ExpireDay 把 ExpireAt 归一化成 YYYY-MM-DD 墙钟日（池内路由键格式）。
// 空串或不可解析返回空串（视为未下发，不参与排序）。
func ExpireDay(v string) string {
	t, ok := ExpireInstant(v)
	if !ok {
		return ""
	}
	return t.In(expireLoc).AddDate(0, 0, -1).Format("2006-01-02")
}

// EarliestExpireDay 返回「可消耗 + 有剩余 + 尚未失效」条目中最早的到期日（YYYY-MM-DD）。
//
// 这是选号的**唯一**路由判据（旧版 R112）：快过期的先烧，减少到期作废。
// 返回空串 = 该账号没有任何可用的到期信息（渠道不下发 expire_at，如 Qoder），
// 调用方应把它当作「到期未知」，排在已知到期日的账号之后。
func EarliestExpireDay(items []meta.ResourceItem, now time.Time) string {
	best := ""
	for _, it := range items {
		if !it.Usable || it.InfoOnly || it.ExpireAt == "" {
			continue
		}
		if it.Remain == nil || *it.Remain <= 0 {
			continue
		}
		t, ok := ExpireInstant(it.ExpireAt)
		if !ok || !t.After(now) {
			continue // 已失效的包不参与「最快过期」判定
		}
		if d := ExpireDay(it.ExpireAt); d != "" && (best == "" || d < best) {
			best = d
		}
	}
	return best
}

// expired 判定条目是否**已失效**（UTC+8 墙钟）。ExpireAt 为空/不可解析时恒 false
// → 余额下降走 spend 口径。
func expired(expireAt string, now time.Time) bool {
	t, ok := ExpireInstant(expireAt)
	if !ok {
		return false
	}
	return !now.In(expireLoc).Before(t)
}

// poolShrankExpiry 报告「余额下降」是否由**池子缩水**而非消耗引起（R101）。
//
// 背景（旧版 2026-10-19 生产实录）：某渠道一个 100 分的赠送包整包到期消失，而它此前
// 已被花掉 1 分。上游回包 total 2500→2400、used 1→0、remaining 2499→2400 ⇒ 账本按
// 「余额下降 + 无到期日」记成 spend(-99)，面板显示成「今日消耗」，实际当天真消耗 0。
//
// 判据用恒等式 remain = total - used 拆项后的 Δremain = Δtotal - Δused：
// 「花了钱」是 used 变大（total 不动），「池子被回收/到期」是 total 变小。
// 三门槛**缺一不可**：
//
//  1. 该条目**未上报到期日**：有到期日的渠道走 expired() 既有判据，本条只接管
//     「上游不给包级到期时间」的盲区，对既有渠道零影响；
//  2. 该**渠道**被证实会给 total / used 分别赋值（channelBooksUsage）：把 Total 填成
//     Remain、Used 恒 0 的渠道，每次真实消耗都会让 Total 一起下降——无此门槛就会把
//     它们的消耗全记成过期；
//  3. **total 确实下降了**：且上次快照必须有 total（老快照无 t 字段 = 0，自动退回
//     旧口径，保证向后兼容——升级后首轮不改写历史归因）。
func poolShrankExpiry(prev itemSnap, curTotal float64, booksUsage bool) bool {
	if prev.Expire != "" { // 门槛 1
		return false
	}
	if !booksUsage { // 门槛 2
		return false
	}
	return prev.Total > 0 && curTotal < prev.Total // 门槛 3
}

// channelBooksUsage 报告该渠道是否**分别上报** total 与 used（即 total 是真面额，
// 而非 remain 的拷贝）。一旦为真永久有效（持久化在快照的 "@ch" 保留键里）。
func channelBooksUsage(snapAll snapshots, ch string) bool {
	return len(snapAll[snapChannels+"/"+ch]) > 0
}

// DiffCredits 余额刷新后差分记账：cur 为本次 accounts.resource_detail 拿到的条目，
// after 为本次可消耗余额（事件后的余额，nil = 未知）。产生 0..n 条流水并更新快照，
// 返回事件数。
//
// 只统计 Usable=true 的条目——不可消耗池（如 TraeWork ep=1）本工具动不到，
// 其变动与路由无关，混入会污染 earn/spend 口径。
func (l *Book) DiffCredits(ch, uid string, cur []meta.ResourceItem, after *float64) int {
	now := time.Now()
	sk := ch + "/" + uid
	l.mu.Lock()
	snapAll := l.loadSnapshots()
	prev := snapAll[sk]
	prevMap := make(map[string]itemSnap, len(prev))
	// 门槛 2 的两个来源取并：① 快照里的 "@ch" 保留键；② 本账号旧快照里留下的
	// used>0（无 "@ch" 标记时代的铁证，向后兼容）。
	booksUsage := channelBooksUsage(snapAll, ch)
	for _, p := range prev {
		prevMap[p.Key] = p
		if p.Used > 0 {
			booksUsage = true
		}
	}

	var events []CreditRecord
	firstTime := len(prev) == 0 // 首次见此账号：只记一条汇总，避免存量条目刷屏

	// 按 key 聚合本次条目（issue #38）：伪键不唯一时多条独立套餐共享一个 key，
	// 必须先合并成一条再与快照差分，保证每 key 每次刷新最多一条事件。
	type aggEnt struct {
		name, expire string
		remain, used float64
		total        float64
	}
	agg := map[string]*aggEnt{} // key → 聚合条目
	var order []string          // 首现顺序遍历，事件输出稳定可测
	for _, it := range cur {
		if !it.Usable {
			continue
		}
		k := itemKey(it)
		a := agg[k]
		if a == nil {
			a = &aggEnt{name: it.Name, expire: it.ExpireAt}
			agg[k], order = a, append(order, k)
		}
		a.remain += deref(it.Remain) // 同 key 多条余额求和，视作一个整体
		a.used += deref(it.Used)
		a.total += deref(it.Total)
		if deref(it.Used) > 0 {
			booksUsage = true // 本次就发现「分别赋值」的铁证，立即信道内生效
		}
		if a.expire == "" {
			a.expire = it.ExpireAt // 到期日取首个非空（同 key 通常相同）
		}
	}

	curMap := map[string]itemSnap{}
	for _, k := range order {
		a := agg[k]
		p, ok := prevMap[k]
		curMap[k] = itemSnap{Key: k, Remain: a.remain, Expire: a.expire, Used: a.used, Total: a.total}
		if !ok {
			if firstTime {
				// 首次见此账号：存量条目由末尾的「存量额度」baseline 统一汇总。
				continue
			}
			// 非首次出现的新 key = 真实新增发放 → 逐条记 earn。修复少记
			// （旧版 2026-10-02 生产账本实证）：WorkBuddy 的伪键是「套餐名|到期日」，
			// 月周期切换时旧键消失（已记 expire）、同余额挂在新键上——静默跳过会导致
			// 每账号每月约 2500 的周期发放从不进入收入流水，账本只降不升。
			// 条目抖动（上游瞬时缺失后回归）会产出 expire+earn 一对，净额不变，
			// 优于「回归被吞、收入永久少记」。
			if a.remain > 0 {
				events = append(events, creditEvent(ch, uid, "earn", a.remain, after, a.name))
			}
			continue
		}
		delta := a.remain - p.Remain
		switch {
		case delta > 0:
			events = append(events, creditEvent(ch, uid, "earn", delta, after, a.name))
		case delta < 0:
			// 下降：到期日已过优先归因过期（签到奖励等当天到期条目的典型形态），
			// 否则归因消耗——除非池子本身缩小了（R101，见 poolShrankExpiry）。
			kind := "spend"
			if expired(p.Expire, now) || poolShrankExpiry(p, a.total, booksUsage) {
				kind = "expire"
			}
			events = append(events, creditEvent(ch, uid, kind, delta, after, a.name))
		case a.used > p.Used:
			// remain 持平但 used 上涨（issue #67）：发放与消耗落在同一刷新窗内，
			// 差分把签到发放误判为「条目没变」。由 remain=total-used 可知发放额 =
			// usedDelta，记 earn 保住收入；对应的消耗不另记 spend（两次快照间无法
			// 拆分先后，净额守恒）。used 恒 0 的渠道自然不触发，零影响。
			events = append(events, creditEvent(ch, uid, "earn", a.used-p.Used, after, a.name))
		}
	}
	// 上次有、本次消失：套餐到期移除是主因，归因过期。
	for k, p := range prevMap {
		if _, ok := curMap[k]; !ok && p.Remain > 0 {
			events = append(events, creditEvent(ch, uid, "expire", -p.Remain, after, k))
		}
	}

	// 首次快照：存量余额整体记一条 earn（baseline），不逐条展开。
	if firstTime {
		sum := 0.0
		for _, v := range curMap {
			sum += v.Remain
		}
		if sum > 0 {
			events = append([]CreditRecord{creditEvent(ch, uid, "earn", sum, after, "存量额度")}, events...)
		}
	}

	// 更新快照（curMap 仅含 Usable 条目；空表删除该账号条目）。
	if len(curMap) == 0 {
		delete(snapAll, sk)
	} else {
		list := make([]itemSnap, 0, len(curMap))
		for _, v := range curMap {
			list = append(list, v)
		}
		snapAll[sk] = list
	}
	// 渠道级「分别上报 total/used」标记（门槛 2）：一旦证实就永久保留，
	// 供同渠道其他账号（如只签到不花钱、used 恒 0 的号）共享。
	if booksUsage {
		snapAll[snapChannels+"/"+ch] = []itemSnap{{Key: ch}}
	}
	l.saveSnapshots(snapAll)
	l.mu.Unlock()

	for _, e := range events {
		l.AppendCredit(e)
	}
	return len(events)
}

// creditEvent 构造一条积分流水（Delta 为带符号变动量，After 为事件后余额）。
func creditEvent(ch, uid, kind string, delta float64, after *float64, note string) CreditRecord {
	d := delta
	return CreditRecord{ChannelID: ch, AccountUID: uid, Kind: kind, Delta: &d, After: after, Note: note}
}

// RecordCheckinEarn 记一条签到发放（amount<=0 时忽略）。旧版同名函数语义。
func (l *Book) RecordCheckinEarn(ch, uid string, amount float64, after *float64, note string) bool {
	if amount <= 0 || l == nil {
		return false
	}
	l.AppendCredit(creditEvent(ch, uid, "earn", amount, after, note))
	return true
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// trimSpace 局部实现，避免为一次 TrimSpace 引入 strings（本文件已足够短）。
func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

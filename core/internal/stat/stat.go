// Package stat core 侧记账：用量/积分双流水（JSONL 追加，按日 + 按渠道分文件）。
//
// 术语（W3-40）：全项目统一叫 stat。旧版 dev 里叫 ledger，仅为历史叫法；
// 本包与之语义等价（记账唯一事实源，`/core/usage`、`/core/stats` 的数据源）。
//
// 文件布局（W3-46）：
//
//	<data>/core/stat/usage/20261102/<channel_id>.jsonl    每行省略 channel_id，含 account_uid
//	<data>/core/stat/credit/20261102/<channel_id>.jsonl
//	<data>/core/stat/fail/20261102.jsonl                  全部失败（含无法归渠道的），行内带 channel_id
//	<data>/core/stat/credit-snapshot.json                 积分差分快照
//
// 为什么按日 + 按渠道分文件（替代旧的按月单文件 usage-YYYYMM.jsonl）：
//   - 看单日流水不必加载整月冗余（用户 m02121 的直接诉求）；
//   - 渠道改名 = 每个日目录里一次 rename(2)，不必逐行重写内容
//     （旧实现重写命中行会重新序列化、字段顺序变了，与「事实源字节稳定」相悖）；
//   - 没有流量的渠道不产生文件，不做叠加方案（不做「共用文件 + 渠道目录」双写）。
//
// 代价是读 7 天要开 N×7 个小文件（用户 m02121 明确认为可接受），故查询侧不做缓存。
package stat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UsageRecord 一条用量流水（写盘时 ChannelID 落进文件名，故带 omitempty）。
type UsageRecord struct {
	TS                  string `json:"ts"`
	RequestID           string `json:"request_id,omitempty"`
	ConversationID      string `json:"conversation_id,omitempty"`
	ChannelID           string `json:"channel_id,omitempty"`
	AccountUID          string `json:"account_uid,omitempty"`
	Model               string `json:"model"`
	PromptTokens        int    `json:"prompt_tokens"`
	CompletionTokens    int    `json:"completion_tokens"`
	TotalTokens         int    `json:"total_tokens"`
	PromptReported      bool   `json:"prompt_reported,omitempty"`
	CompletionReported  bool   `json:"completion_reported,omitempty"`
	TotalReported       bool   `json:"total_reported,omitempty"`
	PromptEstimated     bool   `json:"prompt_estimated,omitempty"`
	CompletionEstimated bool   `json:"completion_estimated,omitempty"`
	TotalEstimated      bool   `json:"total_estimated,omitempty"`
	CachedTokens        int    `json:"cached_tokens,omitempty"`
	CachedReported      bool   `json:"cached_reported,omitempty"`
	CacheWriteTokens    int    `json:"cache_write_tokens,omitempty"`
	CacheWriteReported  bool   `json:"cache_write_reported,omitempty"`
	TimeMs              int64  `json:"time_ms,omitempty"`
	FirstTokenMs        int64  `json:"first_token_ms,omitempty"`
	OK                  bool   `json:"ok"`
	ErrKind             string `json:"err_kind,omitempty"`
}

// CreditRecord 一条积分变动流水。
//
// Kind 是差分记账的归因（earn/spend/expire，旧版 CreditEntry.Kind），Note 是条目名等
// 可读备注（旧版 CreditEntry.Note）。Delta 正=入项、负=消耗/过期，与旧版 Amount 同号。
type CreditRecord struct {
	TS         string   `json:"ts"`
	AccountUID string   `json:"account_uid"`
	ChannelID  string   `json:"channel_id,omitempty"`
	Item       string   `json:"item,omitempty"`
	Unit       string   `json:"unit,omitempty"`
	Kind       string   `json:"kind,omitempty"` // earn | spend | expire
	Before     *float64 `json:"before,omitempty"`
	After      *float64 `json:"after,omitempty"`
	Delta      *float64 `json:"delta,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// FailRecord 一条失败记录（ok=false 的那些，落盘保留，重启后可查）。
//
// 与 usage 流水的区别：usage 的渠道归属在文件名上，无法归渠道的失败（路由未命中）
// 只能落进 `_none.jsonl`，面板看起来是一个假渠道；失败日志把 channel_id 放在行内，
// 允许为空，便于单表排查「哪个渠道在报错、哪些请求根本没路由出去」。
type FailRecord struct {
	TS             string `json:"ts"`
	RequestID      string `json:"request_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	ChannelID      string `json:"channel_id,omitempty"`
	AccountUID     string `json:"account_uid,omitempty"`
	Model          string `json:"model"`
	ErrKind        string `json:"err_kind,omitempty"`
	Message        string `json:"message,omitempty"` // 上游错误摘要（截断；不含凭据，见 §9 红线）
	TimeMs         int64  `json:"time_ms,omitempty"`
	FirstTokenMs   int64  `json:"first_token_ms,omitempty"`
}

// keepDays 流水保留天数（启动时删更早的日目录 / 失败日志）。
const keepDays = 7

// 目录/文件名约定（都是契约的一部分：改名迁移与查询都按这里走）。
const (
	subUsage        = "usage"
	subCredit       = "credit"
	subFail         = "fail"
	dayLayout       = "20060102"
	unknownChannel  = "_none" // 无法归渠道的记录独占一个文件名，避免与真实渠道 ID 冲突
	failMessageCap  = 500     // 错误摘要截断长度
	lineScannerSize = 4 << 20 // 单行上限（宽请求的 usage 行也就几百字节）
)

// Book 双流水（按日 + 按渠道分文件）。
//
// 不持有任何写句柄：每条记录 open/write/close 一次。这样
//   - 崩溃不丢账（写完即落盘，无缓冲区）；
//   - Windows 下改名不会被已打开句柄挡住（陷阱 24/52 的根因）；
//   - 跨日/跨渠道自然切换，不需要轮转状态机。
//
// 记账频率是「每轮对话/每次余额刷新」，几十次 open 的代价可忽略。
type Book struct {
	mu  sync.Mutex
	dir string
}

// Open 打开（或创建）流水目录。
func Open(dir string) (*Book, error) {
	migrateLegacyDir(dir) // 旧目录名 ledger/ → stat/（W3-40 术语统一，账不能丢）
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Book{dir: dir}
	l.cleanupDays()
	return l, nil
}

// nowTS 记账时间戳：本地时区 + 偏移（RFC3339）。
//
// 用本地时间而不是 UTC，是因为面板按 `TS[:10]` 与本地「今天」比对
// （views.go 的 statsView/usageView），UTC 在 UTC+8 的 00:00~08:00 会被算成昨天。
func nowTS() string { return time.Now().Format(time.RFC3339) }

// dayOfTS 从记录时间戳取本地日期（写目录用）；解析失败退回今天。
func dayOfTS(ts string) string {
	if t, ok := parseTS(ts); ok {
		return t.Local().Format(dayLayout)
	}
	return time.Now().Format(dayLayout)
}

// AppendUsage 记一条用量。
func (l *Book) AppendUsage(r UsageRecord) {
	if r.TS == "" {
		r.TS = nowTS()
	}
	day, ch := dayOfTS(r.TS), r.ChannelID
	if ch == "" {
		ch = unknownChannel
	}
	r.ChannelID = "" // 归属落在文件名上，行内不重复（省 8~13% 行宽）
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLine(filepath.Join(l.dir, subUsage, day, ch+".jsonl"), b)
}

// AppendCredit 记一条积分变动。
func (l *Book) AppendCredit(r CreditRecord) {
	if r.TS == "" {
		r.TS = nowTS()
	}
	day, ch := dayOfTS(r.TS), r.ChannelID
	if ch == "" {
		ch = unknownChannel
	}
	r.ChannelID = ""
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLine(filepath.Join(l.dir, subCredit, day, ch+".jsonl"), b)
}

// AppendFail 记一条失败（每天一个文件，渠道放在行内，允许为空）。
func (l *Book) AppendFail(r FailRecord) {
	if r.TS == "" {
		r.TS = nowTS()
	}
	if len(r.Message) > failMessageCap {
		r.Message = r.Message[:failMessageCap]
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLine(filepath.Join(l.dir, subFail, dayOfTS(r.TS)+".jsonl"), b)
}

// appendLine 追加一行并立即关闭（不传 bufio：单行写没必要）。调用方须持锁。
func (l *Book) appendLine(path string, raw []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(raw, '\n'))
	_ = f.Close()
}

// QueryUsage 读最近 days 天用量（按日目录逐文件扫描，行内补回 channel_id）。
func (l *Book) QueryUsage(days int) []UsageRecord {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	var out []UsageRecord
	l.scanDays(subUsage, cutoff, func(ch string, line []byte) {
		var r UsageRecord
		if json.Unmarshal(line, &r) != nil {
			return
		}
		if t, ok := parseTS(r.TS); ok && t.Before(cutoff) {
			return
		}
		if ch != unknownChannel { // "_none" 保持空渠道，沿用「无渠道记录不计入渠道维度」
			r.ChannelID = ch
		}
		out = append(out, r)
	})
	return out
}

// QueryCredits 读最近 days 天积分流水。
func (l *Book) QueryCredits(days int) []CreditRecord {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	var out []CreditRecord
	l.scanDays(subCredit, cutoff, func(ch string, line []byte) {
		var r CreditRecord
		if json.Unmarshal(line, &r) != nil {
			return
		}
		if t, ok := parseTS(r.TS); ok && t.Before(cutoff) {
			return
		}
		if ch != unknownChannel {
			r.ChannelID = ch
		}
		out = append(out, r)
	})
	return out
}

// QueryFails 读最近 days 天失败日志（最新在前，最多 limit 条；limit<=0 不限）。
func (l *Book) QueryFails(days, limit int) []FailRecord {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	fdir := filepath.Join(l.dir, subFail)
	ents, err := os.ReadDir(fdir)
	if err != nil {
		return nil
	}
	var out []FailRecord
	// ReadDir 按名升序 = 按日升序，故反向遍历日文件得「新的一天在前」
	for i := len(ents) - 1; i >= 0; i-- {
		name := ents[i].Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		var day []FailRecord // 日文件内部是追加序（旧在前），单独反转后追加
		readLines(filepath.Join(fdir, name), func(line []byte) {
			var r FailRecord
			if json.Unmarshal(line, &r) != nil {
				return
			}
			if t, ok := parseTS(r.TS); ok && t.Before(cutoff) {
				return
			}
			day = append(day, r)
		})
		for j := len(day) - 1; j >= 0; j-- {
			out = append(out, day[j])
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// scanDays 按日遍历指定前缀的流水（[cutoff 当天, 今天]），对每个渠道文件逐行回调。
//
// 目录方向是「前缀/日/渠道.jsonl」：查某天全渠道 = 1 次 readdir（面板主流用法），
// 查某渠道 N 天 = N 次 readdir，两种都只多一层。
func (l *Book) scanDays(prefix string, cutoff time.Time, fn func(ch string, line []byte)) {
	base := filepath.Join(l.dir, prefix)
	now := time.Now()
	for d := dayStart(cutoff); !d.After(now); d = d.AddDate(0, 0, 1) {
		dir := filepath.Join(base, d.Format(dayLayout))
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue // 当天没流水 = 没这个目录，正常
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue // 未识别文件一律不碰
			}
			ch := strings.TrimSuffix(name, ".jsonl")
			readLines(filepath.Join(dir, name), func(line []byte) { fn(ch, line) })
		}
	}
}

// dayStart 当天 00:00（本地时区）。
func dayStart(t time.Time) time.Time {
	t = t.Local()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// readLines 逐行读一个 JSONL 文件（不存在即跳过）。
func readLines(path string, fn func(line []byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), lineScannerSize)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		fn(line)
	}
}

// cleanupDays 删除保留窗口之外的日目录与失败日志（启动时一次性）。
//
// 只有一个保留维度（keepDays）。旧的按月分段机制连同 keepMonths/monthsInRange/
// trimCurrentMonth 一起删除：按月分段本身是为「跨月看不到数据」打的补丁，
// 按日分文件后这个问题的前提不存在了。
func (l *Book) cleanupDays() {
	cutoff := time.Now().AddDate(0, 0, -keepDays).Format(dayLayout)
	for _, prefix := range []string{subUsage, subCredit} {
		base := filepath.Join(l.dir, prefix)
		ents, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() && e.Name() < cutoff {
				_ = os.RemoveAll(filepath.Join(base, e.Name()))
			}
		}
	}
	fdir := filepath.Join(l.dir, subFail)
	if ents, err := os.ReadDir(fdir); err == nil {
		for _, e := range ents {
			if name := e.Name(); strings.HasSuffix(name, ".jsonl") && strings.TrimSuffix(name, ".jsonl") < cutoff {
				_ = os.Remove(filepath.Join(fdir, name))
			}
		}
	}
	l.noticeLegacyLayout()
}

// noticeLegacyLayout 旧布局（单文件 usage.jsonl / 按月分段 usage-YYYYMM.jsonl）不再读取。
//
// 按 W3-37 定案 4（ww3 未发布，除 auths/ 外无兼容义务）不做迁移，但也不删——
// 那是用户数据，删不删该由人决定；这里只在联调时提示一行。
func (l *Book) noticeLegacyLayout() {
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if strings.HasPrefix(e.Name(), "body-trace-") {
			continue // 请求体摘要分片走自己的生命周期
		}
		fmt.Fprintf(os.Stderr, "[stat] 发现旧布局流水 %s（已不再读取，可手工删除）\n", e.Name())
		return
	}
}

// Close 兼容旧调用点：本实现不持有句柄，无操作。
func (l *Book) Close() {}

// flushLocked 兼容旧调用点：本实现无缓冲区，无操作。
func (l *Book) flushLocked() {}

// migrateLegacyDir 把旧目录名 `<data>/core/ledger` 整体搬到 `<data>/core/stat`（W3-40 术语统一）。
// 账本是唯一事实源，改名不能丢数据；目标已存在则不合并（只记一行，靠人工），
// 与插件数据根迁移（W3-39 preparePluginData）同一条纪律：绝不拿新数据盖旧账。
func migrateLegacyDir(dir string) {
	old := filepath.Join(filepath.Dir(filepath.Clean(dir)), "ledger")
	if filepath.Base(filepath.Clean(dir)) != "stat" {
		return // 非默认布局（测试临时目录等）不做猜路径的事
	}
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(os.Stderr, "[stat] 旧目录 %s 仍存在但 %s 已存在，需人工合并（本次不改动）\n", old, dir)
		return
	}
	if err := os.Rename(old, dir); err != nil { // Windows 跨盘/占用时退化：保持旧目录，照旧读写（不静默丢账）
		fmt.Fprintf(os.Stderr, "[stat] 旧目录迁移失败 %s -> %s: %v\n", old, dir, err)
	}
}

// parseTS 宽容解析 TS（RFC3339 或 Unix 秒）。
func parseTS(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

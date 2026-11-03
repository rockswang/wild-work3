// Package stat core 侧记账：用量/积分双流水（JSONL 按月分段追加）。
//
// 术语（W3-40）：全项目统一叫 stat。旧版 dev 里叫 ledger，仅为历史叫法；
// 本包与之语义等价（记账唯一事实源，`/core/usage`、`/core/stats` 的数据源）。
package stat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UsageRecord 一条用量流水。
type UsageRecord struct {
	TS                  string `json:"ts"`
	RequestID           string `json:"request_id,omitempty"`
	ConversationID      string `json:"conversation_id,omitempty"`
	ChannelID           string `json:"channel_id"`
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

// keepDays 单文件内条目保留天数（启动时裁剪更早的，旧版 keepDays=7）。
const keepDays = 7

// keepMonths 月分段保留月数（启动时删掉更早的分段，旧版 keepMonths=1）。
// 与 keepDays 是两个维度：keepMonths 管「文件」存在多久，keepDays 管「当月文件内」的条目。
const keepMonths = 1

// Stat 双流水（usage / credit 按月分段 `usage-YYYYMM.jsonl`）。
type Book struct {
	mu     sync.Mutex
	dir    string
	uFile  *os.File
	cFile  *os.File
	uBuf   *bufio.Writer
	cBuf   *bufio.Writer
	uMonth string // 当前写句柄对应的月份 "200601"（跨月自然轮转）
	cMonth string
}

// Open 打开（或创建）流水目录。
func Open(dir string) (*Book, error) {
	migrateLegacyDir(dir) // 旧目录名 ledger/ → stat/（W3-40 术语统一，账不能丢）
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Book{dir: dir}
	// 启动维护顺序：先迁移旧单文件 → 删过期月分段 → 裁剪当月条目 → 最后才打开写句柄。
	// 必须在 OpenFile 之前做完重写：Windows 下已打开的句柄会让 rename 失败（报 Access is denied），
	// 结果是裁不掉还留下一堆 .tmp 垃圾。
	l.migrateLegacySingle("usage")
	l.migrateLegacySingle("credit")
	l.cleanupMonths()
	l.trimCurrentMonth("usage")
	l.trimCurrentMonth("credit")
	return l, nil
}

// trimJSONL 丢弃 TS 早于保留期的行（原子重写；解析失败的行也一并丢弃）。
func trimJSONL(path string, days int) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	keep := make([][]byte, 0, 256)
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var head struct {
			TS string `json:"ts"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, head.TS); err == nil && t.Before(cutoff) {
			continue
		}
		keep = append(keep, line)
	}
	out := make([]byte, 0, len(raw))
	for _, line := range keep {
		out = append(append(out, line...), '\n')
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, out, 0o644) != nil {
		return
	}
	if os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp) // 重写失败（如被占用）就放弃裁剪，不留垃圾文件
	}
}

// AppendUsage 记一条用量。
func (l *Book) AppendUsage(r UsageRecord) {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeLine("usage", b)
	if l.uBuf != nil {
		_ = l.uBuf.Flush() // 逐条 flush：崩溃不丢账
	}
}

// AppendCredit 记一条积分变动。
func (l *Book) AppendCredit(r CreditRecord) {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeLine("credit", b)
	if l.cBuf != nil {
		_ = l.cBuf.Flush()
	}
}

// QueryUsage 读最近 days 天用量（跨月分段自动合并）。
func (l *Book) QueryUsage(days int) []UsageRecord {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()
	var out []UsageRecord
	for _, m := range monthsInRange(cutoff, time.Now()) {
		l.queryMonth("usage", m, func(line []byte) {
			var r UsageRecord
			if json.Unmarshal(line, &r) != nil {
				return
			}
			if t, ok := parseTS(r.TS); ok && t.Before(cutoff) {
				return
			}
			out = append(out, r)
		})
	}
	return out
}

// QueryCredits 读最近 days 天积分流水（跨月分段自动合并）。
func (l *Book) QueryCredits(days int) []CreditRecord {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()
	var out []CreditRecord
	for _, m := range monthsInRange(cutoff, time.Now()) {
		l.queryMonth("credit", m, func(line []byte) {
			var r CreditRecord
			if json.Unmarshal(line, &r) != nil {
				return
			}
			if t, ok := parseTS(r.TS); ok && t.Before(cutoff) {
				return
			}
			out = append(out, r)
		})
	}
	return out
}

// flushLocked 落盘两个缓冲句柄（调用方须持锁）。
func (l *Book) flushLocked() {
	if l.uBuf != nil {
		_ = l.uBuf.Flush()
	}
	if l.cBuf != nil {
		_ = l.cBuf.Flush()
	}
}

// Close 关闭文件。
func (l *Book) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	if l.uFile != nil {
		_ = l.uFile.Close()
	}
	if l.cFile != nil {
		_ = l.cFile.Close()
	}
}

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

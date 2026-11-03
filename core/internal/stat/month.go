package stat

// 月分段流水（旧版 dev/internal/ledger 的 monthFile/writer/cleanup 语义）。
//
// 旧版把 usage/credit 按月分文件 `usage-202601.jsonl`：
//   - 跨月自动轮转（自然轮转，不需要定时任务）；
//   - 启动时删掉 keepMonths=1 之外的旧分段（保留上月+当月）；
//   - 当月文件内仍按 keepDays=7 裁剪条目（trimCurrentMonth）。
//
// ww3 早期是单文件 `usage.jsonl` + 7 天裁剪，等于只能看 7 天；这里补齐月分段，
// 使面板的 7 天窗口跨月时也能读到上月数据（旧版真实痛点：
// 「每月 1~6 号看不到本月数据」就是因为月分段枚举写错）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// monthFile 月份分段文件名：usage-202601.jsonl。
func monthFile(prefix string, t time.Time) string {
	return fmt.Sprintf("%s-%s.jsonl", prefix, t.Format("200601"))
}

// monthOf 从分段文件名截出月份（"usage-202601.jsonl" → "202601"）。
func monthOf(name string) string {
	stem := strings.TrimSuffix(name, ".jsonl")
	if i := strings.LastIndexByte(stem, '-'); i >= 0 && len(stem)-i-1 == 6 {
		return stem[i+1:]
	}
	return ""
}

// migrateLegacySingle 把旧单文件（ww3 早期 `<prefix>.jsonl`）迁移到当月分段
// `usage-YYYYMM.jsonl`，否则升级后旧流水凭空消失（用户看到「统计清零」）。
// 幂等：迁移成功后删原文件。
func (l *Book) migrateLegacySingle(prefix string) {
	src := filepath.Join(l.dir, prefix+".jsonl")
	raw, err := os.ReadFile(src)
	if err != nil || len(raw) == 0 {
		if err == nil {
			_ = os.Remove(src) // 空文件直接清掉
		}
		return
	}
	dst := filepath.Join(l.dir, monthFile(prefix, time.Now()))
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, werr := f.Write(raw)
	_ = f.Close()
	if werr == nil {
		_ = os.Remove(src)
	}
}

// cleanupMonths 删除保留窗口之外的月分段（keepMonths=1：保留上月与当月）。
func (l *Book) cleanupMonths() {
	cutoff := time.Now().AddDate(0, -keepMonths, 0).Format("200601")
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if m := monthOf(name); m != "" && m < cutoff {
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

// trimCurrentMonth 裁剪当月分段里超过 keepDays 的条目（旧版同款：一次性重写）。
// 注意顺序：必须在打开写句柄之前调用（Windows 下已打开句柄会让 rename 失败）。
func (l *Book) trimCurrentMonth(prefix string) {
	fp := filepath.Join(l.dir, monthFile(prefix, time.Now()))
	trimJSONL(fp, keepDays)
}

// queryMonth 扫描指定月份分段（不存在即跳过）。
func (l *Book) queryMonth(prefix, month string, fn func(line []byte)) {
	fp := filepath.Join(l.dir, fmt.Sprintf("%s-%s.jsonl", prefix, month))
	f, err := os.Open(fp)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		fn(line)
	}
}

// monthsInRange 枚举 [from, now] 覆盖的月份（含端点）。
//
// 必须逐月枚举：曾用「from 起 +15 天步进」，当 from 在上月 25 号之后
// （7 天窗口跨月的常态）时 +15 天直接越过当月，导致本月分段整月不被扫描。
func monthsInRange(from, now time.Time) []string {
	var out []string
	loc := now.Location()
	for t := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, loc); !t.After(now); t = t.AddDate(0, 1, 0) {
		out = append(out, t.Format("200601"))
	}
	return out
}

// writer 取指定月份的缓冲写句柄；跨月时关旧开新（自然轮转）。调用方须持锁。
func (l *Book) writer(prefix string, t time.Time) *bufio.Writer {
	month := t.Format("200601")
	var cur *bufio.Writer
	var curF **os.File
	var curM *string
	switch prefix {
	case "usage":
		cur, curF, curM = l.uBuf, &l.uFile, &l.uMonth
	case "credit":
		cur, curF, curM = l.cBuf, &l.cFile, &l.cMonth
	}
	if cur != nil && *curM == month {
		return cur
	}
	if cur != nil && *curF != nil { // 跨月：关旧句柄
		_ = cur.Flush()
		_ = (*curF).Close()
		switch prefix {
		case "usage":
			l.uBuf, l.uFile, l.uMonth = nil, nil, ""
		case "credit":
			l.cBuf, l.cFile, l.cMonth = nil, nil, ""
		}
	}
	fp := filepath.Join(l.dir, monthFile(prefix, t))
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	w := bufio.NewWriterSize(f, 8*1024)
	*curF, *curM = f, month
	switch prefix {
	case "usage":
		l.uBuf = w
	case "credit":
		l.cBuf = w
	}
	return w
}

// writeLine 写一行到指定前缀的当月分段（自动轮转）。调用方须持锁。
func (l *Book) writeLine(prefix string, raw []byte) {
	w := l.writer(prefix, time.Now())
	if w == nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
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

// tsOf 从一条 JSON 行取 ts 字段（兼容字符串与数字）。
func tsOf(line []byte) (time.Time, bool) {
	var head struct {
		TS json.RawMessage `json:"ts"`
	}
	if json.Unmarshal(line, &head) != nil || len(head.TS) == 0 {
		return time.Time{}, false
	}
	s := strings.Trim(string(head.TS), `"`)
	return parseTS(s)
}

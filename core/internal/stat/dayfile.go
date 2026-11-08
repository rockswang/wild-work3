package stat

// dayfile.go 按日 + 按渠道的分文件布局（W3-46）。
//
//	usage/20261102/<channel_id>.jsonl
//	credit/20261102/<channel_id>.jsonl
//	fail/20261102.jsonl
//
// 本文件只放路径/命名/日期这些「布局」层面的工具；写入与查询在 stat.go，
// 改名迁移在 rename.go。
//
// 与旧布局（按月单文件 usage-YYYYMM.jsonl）的关键差异：
//   - 留存窗口只有一个维度 keepDays，旧的 keepMonths 月分段机制整体删除
//     （monthFile/monthOf/monthsInRange/queryMonth/cleanupMonths/trimCurrentMonth/
//     trimJSONL/migrateLegacySingle 都不再需要）；
//   - 单日文件天然很小，不需要「当月文件内裁剪条目」那种重写式维护；
//   - 渠道归属从行内字段改为文件名，改名从「逐行重写内容」变成「文件改名」。

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dayDir 某天的流水目录：<stat>/<prefix>/<YYYYMMDD>。
func (l *Book) dayDir(prefix string, t time.Time) string {
	return filepath.Join(l.dir, prefix, t.Format(dayLayout))
}

// channelFile 某天某渠道的流水文件；channel 为空时用 _none 占位。
func (l *Book) channelFile(prefix string, t time.Time, channel string) string {
	if channel == "" {
		channel = unknownChannel
	}
	return filepath.Join(l.dayDir(prefix, t), channel+".jsonl")
}

// failFile 某天的失败日志文件（单文件，渠道在行内）。
func (l *Book) failFile(t time.Time) string {
	return filepath.Join(l.dir, subFail, t.Format(dayLayout)+".jsonl")
}

// daysInRange 枚举 [from 当天, now] 的每一天（本地时区，含端点）。
//
// 逐日枚举而不是「起点 +N 天步进」：后者曾因 DST/取整漏掉一天（旧版按月分段
// 的同类 bug 也是「枚举写错」）。
func daysInRange(from, now time.Time) []string {
	var out []string
	for d := dayStart(from); !d.After(now); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(dayLayout))
	}
	return out
}

// isDayDir 判断目录名是不是合法日目录（YYYYMMDD）。
func isDayDir(name string) bool {
	if len(name) != 8 {
		return false
	}
	for i := 0; i < 8; i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// listChannelFiles 列出某日某前缀下的渠道文件名（不含 .jsonl），按名升序。
func (l *Book) listChannelFiles(prefix, day string) []string {
	dir := filepath.Join(l.dir, prefix, day)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	return out
}

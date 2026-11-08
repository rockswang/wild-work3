package stat

// 渠道实例改名后的账本归属迁移（W3-45 / W3-46）。
//
// 为什么必须迁：core 的账本（usage/credit 流水 + credit-snapshot.json）以
// `channel_id` / `account_uid` 为归属键。账号主键形如 `<remoteUID>@<channelID>`，
// 无凭据渠道的虚拟账号则直接等于渠道 ID。渠道实例改名后若只改 config.json，
// 历史用量会挂在一个**已不存在**的渠道 ID 上——面板按渠道聚合时冒出幽灵渠道；
// 积分差分更糟：快照键变了，下次刷新会把整包存量重记一次 earn，账目直接翻倍。
//
// W3-46 起归属落在**文件名**上（`<prefix>/<day>/<channel>.jsonl`），所以改名
// 就是每个日目录里一次 rename(2)：不解析行内容、不重新序列化，事实源字节零改动。
// 行内仍要改的只有一处——`account_uid` 里的 `@oldID` 后缀（账号主键含渠道 ID），
// 那是行内字段，只能重写；为保持"只改必须改的字节"，这里做**字节级后缀替换**。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RenameChannel 把账本里 oldID 的归属整体迁到 newID（幂等；oldID==newID 直接返回）。
//
// 顺序：流水文件改名（快、不碰内容）→ 行内 account_uid 后缀改写 → 快照键迁移。
// 不再需要「先关句柄」：本实现不持句柄（见 Book 注释），Windows 不会挡住 rename。
func (l *Book) RenameChannel(oldID, newID string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.renameChannelFiles(oldID, newID); err != nil {
		return err
	}
	l.remapSnapshotKeys(oldID, newID)
	return nil
}

// renameChannelFiles 把 [近 keepDays 天] 内 oldID 的流水文件改名为 newID，
// 并改写这些文件里 account_uid 的 `@oldID` 后缀。
//
// 只处理保留窗口内的文件：更早的目录启动时会被 cleanupDays 删掉，改名它们
// 纯属浪费（且会让「改名耗时」随历史线性增长）。
func (l *Book) renameChannelFiles(oldID, newID string) error {
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	for _, prefix := range []string{subUsage, subCredit} {
		for _, day := range daysInRange(cutoff, time.Now()) {
			dir := filepath.Join(l.dir, prefix, day)
			src := filepath.Join(dir, oldID+".jsonl")
			if _, err := os.Stat(src); err != nil {
				continue // 该渠道这天没有流水，正常
			}
			// 先改行内 account_uid（仍需打开旧文件），再改名
			if err := rewriteJSONL(src, func(line []byte) []byte {
				return remapLedgerLine(line, oldID, newID)
			}); err != nil {
				return err
			}
			dst := filepath.Join(dir, newID+".jsonl")
			if _, err := os.Stat(dst); err == nil {
				// 目标已存在（改名成同前缀的旧实例）：合并追加，避免覆盖丢账
				if err := appendFile(dst, src); err != nil {
					return err
				}
				_ = os.Remove(src)
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	// 失败日志不按渠道拆文件，归属在行内 —— 改成逐日文件扫描重写（文件本身很小）
	fdir := filepath.Join(l.dir, subFail)
	if ents, err := os.ReadDir(fdir); err == nil {
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if err := rewriteJSONL(filepath.Join(fdir, e.Name()), func(line []byte) []byte {
				return remapLedgerLine(line, oldID, newID)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// remapSnapshotKeys 迁移 credit-snapshot.json 的两种键：
//
//	"ch/uid"  真实账号键 —— ch 段与 uid 段都要按规则改；
//	"@ch/ch"  渠道能力键 —— 只改 ch 段（见 credit.go 的 snapChannels）。
func (l *Book) remapSnapshotKeys(oldID, newID string) {
	snapAll := l.loadSnapshots()
	if len(snapAll) == 0 {
		return
	}
	next := make(snapshots, len(snapAll))
	for k, v := range snapAll {
		ch, uid, ok := strings.Cut(k, "/")
		if !ok {
			next[k] = v
			continue
		}
		if ch != snapChannels && ch == oldID {
			ch = newID // 真实账号键：渠道段迁到新 ID
		}
		next[ch+"/"+remapUID(uid, oldID, newID)] = v
	}
	l.saveSnapshots(next)
}

// remapUID 账号主键迁移：虚拟账号（UID 恰为渠道 ID）与 `<remoteUID>@<channelID>` 两种形态。
func remapUID(uid, oldID, newID string) string {
	if uid == oldID {
		return newID
	}
	if strings.HasSuffix(uid, "@"+oldID) {
		return uid[:len(uid)-len(oldID)] + newID
	}
	return uid
}

// remapLedgerLine 只重写需要改的字段；其余原样返回字节。
//
// 用 map 重新序列化会让字段顺序变化（旧实现就是「命中行整体重排」），与
// 「账本是 append-only 事实源」相悖，故这里做**字节级替换**：
//   - `account_uid`：账号主键含渠道 ID（`<remote>@<oldID>` 或虚拟账号 = oldID），必须改；
//   - `channel_id`：usage/credit 已把它移到文件名（行内不出现），但失败日志
//     （fail/YYYYMMDD.jsonl）仍写在行内，故也要改。
func remapLedgerLine(line []byte, oldID, newID string) []byte {
	var head struct {
		ChannelID  string `json:"channel_id"`
		AccountUID string `json:"account_uid"`
	}
	if json.Unmarshal(line, &head) != nil {
		return line
	}
	newCh, newUID := head.ChannelID, head.AccountUID
	if newCh == oldID {
		newCh = newID
	}
	newUID = remapUID(newUID, oldID, newID)
	if newCh == head.ChannelID && newUID == head.AccountUID {
		return line // 未命中：字节不变
	}
	out := line
	if newCh != head.ChannelID {
		out = bytes.Replace(out, []byte(`"channel_id":`+quoteJSON(head.ChannelID)), []byte(`"channel_id":`+quoteJSON(newCh)), 1)
	}
	if newUID != head.AccountUID {
		old := []byte(`"account_uid":` + quoteJSON(head.AccountUID))
		next := []byte(`"account_uid":` + quoteJSON(newUID))
		if bytes.Contains(out, old) { // 字段有值必出现（omitempty 只对有值时省略）
			out = bytes.Replace(out, old, next, 1)
		} else {
			// 兜底：源行里该字段是别的转义写法，退回整体重序列化（仅这一行）
			var m map[string]any
			if json.Unmarshal(out, &m) == nil {
				m["account_uid"] = newUID
				if b, err := json.Marshal(m); err == nil {
					out = b
				}
			}
		}
	}
	return out
}

// quoteJSON 把字符串编码成 JSON 字面量（只用于拼字节级替换的目标片段）。
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// appendFile 把 src 的内容追加到 dst（用于改名撞上已存在的同名渠道文件：
// 把旧实例的账目并入新实例而不是覆盖，用户 m02121「渠道 ID 重名可认为同一个渠道」）。
func appendFile(dst, src string) error {
	raw, err := os.ReadFile(src)
	if err != nil || len(raw) == 0 {
		return nil
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(raw)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// rewriteJSONL 逐行改写一份 JSONL（原子重写 tmp+rename；失败不留垃圾）。
func rewriteJSONL(path string, fn func(line []byte) []byte) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var out bytes.Buffer
	out.Grow(len(raw) + 64)
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		out.Write(fn(line))
		out.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // 重写失败（如被占用）不留垃圾，原文件保持原样
		return err
	}
	return nil
}

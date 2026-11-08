package stat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 渠道实例改名后，账本里的归属键必须整体迁到新 ID（W3-45，用户口径「端点改名则迁移数据」）。
//
// 不迁的后果有两种，都不会报错，只会静默错账：
//   - 历史用量挂在已不存在的渠道 ID 上 → 面板按渠道聚合冒出「幽灵渠道」；
//   - 积分快照键变了 → DiffCredits 认为「首次见到该渠道」，把整包存量额度重记一条 earn。
//
// W3-46 起渠道归属在文件名上，故「迁」= 文件 rename + 行内 account_uid 后缀改写。
func TestRenameChannelRemapsLedger(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b.AppendUsage(UsageRecord{ChannelID: "old", AccountUID: "u1@old", Model: "m1", TotalTokens: 10})
	b.AppendUsage(UsageRecord{ChannelID: "old", AccountUID: "old", Model: "m2", TotalTokens: 20}) // 虚拟账号：UID 即渠道 ID
	b.AppendUsage(UsageRecord{ChannelID: "keep", AccountUID: "u9@keep", Model: "m3", TotalTokens: 30})
	b.AppendCredit(CreditRecord{ChannelID: "old", AccountUID: "u1@old", Item: "积分", Delta: f64(5)})
	// 快照：普通账号键 + 渠道级保留键
	b.saveSnapshots(snapshots{
		"old/u1":              {{Key: "credits", Remain: 100}},
		snapChannels + "/old": {{Key: "old"}},
		"keep/u9":             {{Key: "credits", Remain: 7}},
	})
	if err := b.RenameChannel("old", "new"); err != nil {
		t.Fatal(err)
	}
	// 关闭后重新打开：确认落盘（不是只改了内存）
	b.Close()
	b2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()

	us := b2.QueryUsage(7)
	if len(us) != 3 {
		t.Fatalf("用量条数变了: %d", len(us))
	}
	got := map[string]string{} // model → "channel/uid"
	for _, u := range us {
		got[u.Model] = u.ChannelID + "/" + u.AccountUID
	}
	if got["m1"] != "new/u1@new" {
		t.Errorf("普通账号未迁移: %q", got["m1"])
	}
	if got["m2"] != "new/new" {
		t.Errorf("虚拟账号未迁移: %q", got["m2"])
	}
	if got["m3"] != "keep/u9@keep" {
		t.Errorf("无关渠道被误改: %q", got["m3"])
	}
	cs := b2.QueryCredits(7)
	if len(cs) != 1 || cs[0].ChannelID != "new" || cs[0].AccountUID != "u1@new" {
		t.Fatalf("积分流水未迁移: %+v", cs)
	}
	snap := b2.loadSnapshots()
	if _, ok := snap["new/u1"]; !ok {
		t.Errorf("快照普通键未迁移: %+v", snap)
	}
	if _, ok := snap[snapChannels+"/new"]; !ok {
		t.Errorf("快照渠道级键未迁移: %+v", snap)
	}
	if _, ok := snap["old/u1"]; ok {
		t.Errorf("旧快照键残留: %+v", snap)
	}
	if _, ok := snap["keep/u9"]; !ok {
		t.Errorf("无关快照被误改: %+v", snap)
	}
}

// 改名后必须继续可写（新实现不持句柄，此处守住「迁移不会让 Book 变成只读」）。
func TestRenameChannelKeepsWritable(t *testing.T) {
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.AppendUsage(UsageRecord{ChannelID: "old", Model: "m"})
	if err := b.RenameChannel("old", "new"); err != nil {
		t.Fatal(err)
	}
	b.AppendUsage(UsageRecord{ChannelID: "new", Model: "m2"}) // 迁移后继续记账
	us := b.QueryUsage(7)
	if len(us) != 2 {
		t.Fatalf("迁移后新记录丢失: %+v", us)
	}
}

// 改名是幂等的、空参安全：同名/空 ID 一律不动账本。
func TestRenameChannelNoop(t *testing.T) {
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.AppendUsage(UsageRecord{ChannelID: "same", Model: "m"})
	for _, c := range [][2]string{{"same", "same"}, {"", "x"}, {"x", ""}} {
		if err := b.RenameChannel(c[0], c[1]); err != nil {
			t.Fatalf("%v: %v", c, err)
		}
	}
	if us := b.QueryUsage(7); len(us) != 1 || us[0].ChannelID != "same" {
		t.Fatalf("不应被改动: %+v", us)
	}
}

// 改名撞上已存在的同名渠道文件时必须**合并追加**，不能覆盖（用户口径：重名视为同一渠道）。
func TestRenameChannelMergesIntoExisting(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.AppendUsage(UsageRecord{ChannelID: "old", AccountUID: "u@old", Model: "from-old"})
	b.AppendUsage(UsageRecord{ChannelID: "new", AccountUID: "u@new", Model: "from-new"})
	if err := b.RenameChannel("old", "new"); err != nil {
		t.Fatal(err)
	}
	us := b.QueryUsage(7)
	seen := map[string]string{}
	for _, u := range us {
		seen[u.Model] = u.ChannelID + "/" + u.AccountUID
	}
	if seen["from-old"] != "new/u@new" {
		t.Errorf("旧实例条目应并入新实例并改写 uid: %q", seen["from-old"])
	}
	if seen["from-new"] != "new/u@new" {
		t.Errorf("新实例自有条目被破坏: %q", seen["from-new"])
	}
}

// 改名只处理保留窗口内的日目录（更早的启动时会被清掉，处理它们纯属浪费）。
func TestRenameChannelOnlyRecentDays(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	oldDay := time.Now().AddDate(0, 0, -30)
	p := b.channelFile(subUsage, oldDay, "old")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"ts":"x","model":"m"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.RenameChannel("old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("窗口外的日目录不应被改名（留给 cleanupDays）: %v", err)
	}
}

// 失败日志的渠道归属在行内（不按渠道拆文件），改名时必须一并改，
// 否则「今日失败」表格会把历史失败挂在已消失的渠道 ID 上。
func TestRenameChannelRemapsFailLog(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.AppendFail(FailRecord{ChannelID: "old", AccountUID: "u@old", Model: "m", ErrKind: "server"})
	b.AppendFail(FailRecord{ChannelID: "keep", Model: "m2", ErrKind: "server"})
	if err := b.RenameChannel("old", "new"); err != nil {
		t.Fatal(err)
	}
	got := b.QueryFails(7, 0)
	seen := map[string]string{}
	for _, f := range got {
		seen[f.Model] = f.ChannelID + "/" + f.AccountUID
	}
	if seen["m"] != "new/u@new" {
		t.Errorf("失败日志未迁移: %q", seen["m"])
	}
	if seen["m2"] != "keep/" {
		t.Errorf("无关失败记录被误改: %q", seen["m2"])
	}
}

// 坏行/空行不得让整份账本改写失败（容忍脏数据，账不能因此丢），且未命中行字节不变。
func TestRewriteJSONLToleratesBadLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.jsonl")
	keep := `{"ts":"t","account_uid":"other@x","model":"m"}`
	if err := os.WriteFile(p, []byte("not json\n\n"+keep+"\n"+`{"account_uid":"a@old","model":"m2"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rewriteJSONL(p, func(l []byte) []byte { return remapLedgerLine(l, "old", "new") }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, `"account_uid":"a@new"`) {
		t.Fatalf("命中行未改写: %s", out)
	}
	if !strings.Contains(out, "not json") {
		t.Fatalf("坏行被丢弃: %s", out)
	}
	if !strings.Contains(out, keep) {
		t.Fatalf("未命中行的字节不应变化: %s", out)
	}
}

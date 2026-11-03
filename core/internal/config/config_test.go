package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// HardCooldownFor：旧版 dev 用例——19:07（UTC+8）撞 402，发奖窗口 00:00 + 缓冲 30m，
// 硬冷却从 12h 截到次日 00:30（即 5h23m）。
func TestHardCooldownForTruncates(t *testing.T) {
	c := &Config{DailyGrantAt: "00:00", DailyGrantBufferSec: 1800}
	now := time.Date(2026, 1, 1, 19, 7, 0, 0, grantLoc)
	got := c.HardCooldownFor(now, 12*time.Hour)
	want := 5*time.Hour + 23*time.Minute
	if got != want {
		t.Fatalf("HardCooldownFor = %v, want %v", got, want)
	}
}

// 未启用/配置坏/截断不短于 base 时都退回完整硬冷却（旧版 d<=0 || d>base 退回）。
func TestHardCooldownForFallbacks(t *testing.T) {
	base := 12 * time.Hour
	now := time.Date(2026, 1, 1, 19, 7, 0, 0, grantLoc)
	cases := map[string]struct {
		c *Config
	}{
		"未启用":         {&Config{}},
		"格式坏":         {&Config{DailyGrantAt: "25:00"}},
		"窗口已过且超过base": {&Config{DailyGrantAt: "23:00"}}, // 23:00-19:07=3h53m < 12h，会截断
	}
	// 上表第 3 项实际应截断，单列断言；先测真正回退的两种
	for _, name := range []string{"未启用", "格式坏"} {
		if got := cases[name].c.HardCooldownFor(now, base); got != base {
			t.Fatalf("%s: HardCooldownFor = %v, want base", name, got)
		}
	}
	if got := cases["窗口已过且超过base"].c.HardCooldownFor(now, base); got != 3*time.Hour+53*time.Minute {
		t.Fatalf("23:00 窗口应截断为 3h53m，得 %v", got)
	}
	// now 已在窗口后很久：截断时长会超过 base → 退回 base（不延长）
	c := &Config{DailyGrantAt: "23:50"}
	late := time.Date(2026, 1, 1, 0, 10, 0, 0, grantLoc) // 距 23:50 还有 23h40m > 12h
	if got := c.HardCooldownFor(late, base); got != base {
		t.Fatalf("截断不应延长冷却：got %v", got)
	}
	// 跳天：23:50 时撞 402，窗口 00:00+30m → 锁到 40 分钟后
	dayRoll := &Config{DailyGrantAt: "00:00", DailyGrantBufferSec: 1800}
	cross := time.Date(2026, 1, 1, 23, 50, 0, 0, grantLoc)
	if got := dayRoll.HardCooldownFor(cross, base); got != 40*time.Minute {
		t.Fatalf("跳天截断 = %v, want 40m", got)
	}
}

// normalize 的 raw-JSON 探针：键缺失补默认（err_cooldown 600 对齐旧版 10m）。
func TestNormalizeCooldownDefaults(t *testing.T) {
	dir, root := coreDataDirs(t)
	writeCoreSection(t, root, `{"listen":"127.0.0.1:1","channels":[]}`)
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := st.Get()
	if c.ErrCooldownSeconds != 600 || c.ErrThreshold != 3 || c.CoolSecondsSoft != 60 || c.CoolSecondsHard != 43200 {
		t.Fatalf("冷却默认值不对：%+v", c)
	}
	soft, hard, errCool, threshold := c.Cooling()
	if soft != time.Minute || hard != 12*time.Hour || errCool != 10*time.Minute || threshold != 3 {
		t.Fatalf("Cooling() = %v %v %v %v", soft, hard, errCool, threshold)
	}
	// 显式给值不覆盖
	writeCoreSection(t, root, `{"err_cooldown_seconds":60}`)
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open2: %v", err)
	}
	if st2.Get().ErrCooldownSeconds != 60 {
		t.Fatalf("显式配置被覆盖：%d", st2.Get().ErrCooldownSeconds)
	}
}

// checkin_retry_until：键缺失=720（旧版 CheckinRetryUntil），显式 0=关闭，负值回退默认。
func TestNormalizeCheckinRetryUntil(t *testing.T) {
	dir, root := coreDataDirs(t)
	// 键缺失
	writeCoreSection(t, root, `{}`)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Get().CheckinRetryUntil != 720 {
		t.Fatalf("缺省应为 720，得 %d", st.Get().CheckinRetryUntil)
	}
	// 显式 0 = 关闭
	writeCoreSection(t, root, `{"checkin_retry_until":0}`)
	st2, _ := Open(dir)
	if st2.Get().CheckinRetryUntil != 0 {
		t.Fatalf("显式 0 应保留，得 %d", st2.Get().CheckinRetryUntil)
	}
	// 负值回退默认
	writeCoreSection(t, root, `{"checkin_retry_until":-5}`)
	st3, _ := Open(dir)
	if st3.Get().CheckinRetryUntil != 720 {
		t.Fatalf("负值应回退 720，得 %d", st3.Get().CheckinRetryUntil)
	}
}

// W3-37 定案 4：ww3 未发布过任何版本，除 auths/ 凭据格式外取消向前兼容义务，
// 所以旧的 <data>/core/core.json 不再被读取也不被迁移（只有 config.json 的 core 段是权威）。
func TestOpenIgnoresLegacyCoreJSON(t *testing.T) {
	dir, root := coreDataDirs(t)
	legacy := filepath.Join(dir, "core.json")
	if err := os.WriteFile(legacy, []byte(`{"listen":"127.0.0.1:9999","checkin_retry_until":30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := st.Get().Listen; got != "127.0.0.1:7864" {
		t.Fatalf("listen = %q，旧 core.json 不应被读（期望默认 127.0.0.1:7864）", got)
	}
	if got := st.Get().CheckinRetryUntil; got != 720 {
		t.Fatalf("checkin_retry_until = %d，旧 core.json 不应被读（期望默认 720）", got)
	}
	// Open 在 core 段缺失时会写出默认值（既有行为）；关键是写出的内容不得混进旧 core.json 的值。
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("9999")) || bytes.Contains(b, []byte("\"checkin_retry_until\":30")) {
		t.Fatalf("config.json 里出现了旧 core.json 的值：%s", b)
	}
}

// W3-37 定案 2：core 管理面永远只听回环，非 loopback host 一律被改写（保留端口）。
func TestListenForcedLoopback(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:7864", "127.0.0.1:7864"},
		{"[::1]:7864", "[::1]:7864"},
		{"0.0.0.0:7864", "127.0.0.1:7864"},
		{"10.0.0.5:8888", "127.0.0.1:8888"},
		{"localhost:7864", "127.0.0.1:7864"},
		{"7864", "127.0.0.1:7864"},
	} {
		if got, _ := forceLoopback(tc.in); got != tc.want {
			t.Fatalf("forceLoopback(%q) = %q，应为 %q", tc.in, got, tc.want)
		}
	}
	// 走完整 Open 路径：配了 0.0.0.0 也要被拉回回环
	dir, root := coreDataDirs(t)
	writeCoreSection(t, root, `{"listen":"0.0.0.0:17864"}`)
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := st.Get().Listen; got != "127.0.0.1:17864" {
		t.Fatalf("listen = %q，应被改写为 127.0.0.1:17864", got)
	}
}

// coreDataDirs 返回 (core 运行数据目录 <root>/core, 数据根 <root>)。
func coreDataDirs(t *testing.T) (dir, root string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "core")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, root
}

// writeCoreSection 直接写统一配置文件里的 core 段（不经旧 core.json）。
func writeCoreSection(t *testing.T, root, coreJSON string) {
	t.Helper()
	doc := `{"core":` + coreJSON + `}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// SetRules：按 ID 覆盖/删除/追加三条语义，并且坏规则不落盘（W3-35）。
func TestSetRulesOverrides(t *testing.T) {
	dir, root := coreDataDirs(t)
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	base, err := st.Rules()
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	// 1) 覆盖 + 删除 + 追加
	body := `{"rules":{
		"role.developer":{"order":99},
		"sanitize.cc-kv":{},
		"user.custom":{"kind":"regex","regex":"foo","replacement":"bar","order":500}
	}}`
	if err := st.SetRules([]byte(body)); err != nil {
		t.Fatalf("SetRules: %v", err)
	}
	got, err := st.Rules()
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	byID := map[string]int{}
	for i, r := range got {
		byID[r.ID] = i
	}
	if _, ok := byID["sanitize.cc-kv"]; ok {
		t.Fatal("sanitize.cc-kv 应被 {} 删除")
	}
	if _, ok := byID["user.custom"]; !ok {
		t.Fatal("新规则 user.custom 应被追加")
	}
	if len(got) != len(base) {
		t.Fatalf("规则数量 = %d，应为 %d（删一条加一条）", len(got), len(base))
	}
	// 覆盖生效：role.developer 的 order 变成 99 → 排在 sanitize 之后
	if got[len(got)-1].ID != "user.custom" {
		t.Fatalf("最后一条应为 user.custom（order 500），得 %s", got[len(got)-1].ID)
	}

	// 2) 坏规则（正则不合法）不落盘
	before, _ := st.RulesJSON()
	if err := st.SetRules([]byte(`{"rules":{"bad":{"kind":"regex","regex":"("}}`)); err == nil {
		t.Fatal("非法正则应报错")
	}
	after, _ := st.RulesJSON()
	if string(before) != string(after) {
		t.Fatal("坏规则不应落盘")
	}

	// 3) null 回到全默认
	if err := st.SetRules([]byte(`null`)); err != nil {
		t.Fatalf("SetRules(null): %v", err)
	}
	def, err := st.Rules()
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	if len(def) != len(base) {
		t.Fatalf("回默认后规则数量 = %d，应为 %d", len(def), len(base))
	}
	// 统一文件里不应再有 rules 段（删段语义）
	b, _ := os.ReadFile(filepath.Join(root, "config.json"))
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc["rules"]) != 0 {
		t.Fatalf("null 应删除 rules 段，实得 %s", doc["rules"])
	}
}

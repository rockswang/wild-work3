package appconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"wild-work3/main/rules"
)

// TestRuleOverridesShape 本层只出「用户覆盖段」，不合并默认（W3-37 定案 3：默认表在 core）。
func TestRuleOverridesShape(t *testing.T) {
	dir := t.TempDir()
	user := `{"rules":{
	  "sanitize.feedback": {"replacement": "自定义反馈"},
	  "sanitize.dsh-gui": {},
	  "custom.only-cn": {"kind":"regex","regex":"secret","replacement":"x","scope":{"channels":["glm-cn"]},"order":500}
	}}`
	if err := os.WriteFile(Path(dir), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	over, err := OpenData(dir).RuleOverrides()
	if err != nil {
		t.Fatalf("RuleOverrides: %v", err)
	}
	if len(over) != 3 {
		t.Fatalf("应原样给出 3 条覆盖项，实际 %d", len(over))
	}
	if string(over["sanitize.dsh-gui"]) != "{}" {
		t.Errorf("删除语义（空对象）必须原样保留: %s", over["sanitize.dsh-gui"])
	}
	// 无文件/无段 → nil（调用方据此「全默认」）
	if got, err := OpenData(t.TempDir()).RuleOverrides(); err != nil || got != nil {
		t.Errorf("空目录应返回 nil: %v %v", got, err)
	}
}

// TestWriteRulesRoundTrip 生效集渲染成 ID→对象后必须能读回（gateway 侧靠这个形状拉取）。
func TestWriteRulesRoundTrip(t *testing.T) {
	raw, err := WriteRules([]Rule{{ID: "a", Kind: rules.KindRegex, Regex: "x", Order: 10}, {ID: "b", Kind: rules.KindJSONPath, JSONPath: "$.m"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rules map[string]struct {
			Kind string `json:"kind"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if doc.Rules["a"].Kind != "regex" || doc.Rules["b"].Kind != "jsonpath" {
		t.Errorf("渲染结果缺字段: %s", raw)
	}
}

// TestUpdateKeepsOtherSections 验证「只替换自己那一段」：core 写不覆盖 rules/gateway。
func TestUpdateKeepsOtherSections(t *testing.T) {
	dir := t.TempDir()
	f := OpenData(dir)
	seed := `{"gateway":{"api_key":"k1"},"rules":{"sanitize.feedback":{}},"core":{"listen":"127.0.0.1:7864"}}`
	if err := os.WriteFile(Path(dir), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(SectionCore, func(json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"listen":"127.0.0.1:9999","channels":[]}`), nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	doc := map[string]json.RawMessage{}
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if got := compactJSON(doc[SectionGateway]); got != `{"api_key":"k1"}` {
		t.Errorf("gateway 段被误改: %s", got)
	}
	if got := compactJSON(doc[SectionRules]); got != `{"sanitize.feedback":{}}` {
		t.Errorf("rules 段被误改: %s", got)
	}
	if got, want := compactJSON(doc[SectionCore]), `{"channels":[],"listen":"127.0.0.1:9999"}`; got != want {
		t.Errorf("core 段: want %s got %s", want, got)
	}
}

// TestUpdateDeleteSection 返回空 JSON 表示删除该段。
func TestUpdateDeleteSection(t *testing.T) {
	dir := t.TempDir()
	f := OpenData(dir)
	if err := os.WriteFile(Path(dir), []byte(`{"rules":{"a":{"kind":"regex","regex":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(SectionRules, func(json.RawMessage) (json.RawMessage, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if secs, err := f.Sections(); err != nil || len(secs) != 0 {
		t.Errorf("段应被删除，实际 %v %v", secs, err)
	}
}

// TestNoLegacyMigration 旧布局文件不再被搬运（W3-37 定案 4：未发布过版本，无兼容义务）。
func TestNoLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gateway.json"), []byte(`{"api_key":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if sec, err := OpenData(dir).Section(SectionGateway); err != nil || sec != nil {
		t.Errorf("不应出现迁移出来的 gateway 段: %s %v", sec, err)
	}
}

// TestBadJSONFailsClosed 坏 JSON 必须报错而不是退回默认。
func TestBadJSONFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenData(dir).RuleOverrides(); err == nil {
		t.Error("坏 JSON 应返回错误")
	}
}

// TestUpdateLockReleased 更新后锁文件必须被释放（否则下次 Update 会等 5s 超时）。
func TestUpdateLockReleased(t *testing.T) {
	dir := t.TempDir()
	f := OpenData(dir)
	for i := 0; i < 3; i++ {
		if err := f.Update(SectionCore, func(json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		}); err != nil {
			t.Fatalf("第 %d 次 Update: %v", i+1, err)
		}
	}
	if _, err := os.Stat(f.Path() + ".lock"); !os.IsNotExist(err) {
		t.Errorf("锁文件应已删除: %v", err)
	}
}

// TestLockBusyCoversWindowsVariant P1 回归：锁竞争在 Windows 上是 EACCES 而不是 EEXIST，
// 只认 os.IsExist 会让 core 启动直接致命（实测 open data\config.json.lock: Access is denied.）。
func TestLockBusyCoversWindowsVariant(t *testing.T) {
	for _, err := range []error{
		os.ErrExist, os.ErrPermission,
		syscall.Errno(syscall.EAGAIN), syscall.Errno(syscall.EACCES),
		fmt.Errorf("写配置失败: %w", os.ErrPermission),
		&fs.PathError{Op: "open", Path: "config.json.lock", Err: syscall.Errno(syscall.EACCES)},
	} {
		if !lockBusy(err) {
			t.Errorf("%v 应判为「锁被占、可重试」", err)
		}
	}
	if lockBusy(errors.New("open x.lock: " + os.ErrPermission.Error())) {
		t.Error("手抄的字符串不是包装错误，不能当锁竞争")
	}
	if lockBusy(errors.New("disk full")) {
		t.Error("真 I/O 错误不得当成锁竞争（否则坏错误会被吞成 5s 超时）")
	}
}

// compactJSON 把任意 JSON 归一成紧凑形式，避免比对受缩进影响。
func compactJSON(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "<invalid:" + err.Error() + ">"
	}
	out, _ := json.Marshal(v)
	return string(out)
}

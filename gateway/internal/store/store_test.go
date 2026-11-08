package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 卸载默认必须保留 auths/logs/stat 三个契约数据目录（W3-21/W3-27），
// 否则重装后要重新登录所有账号、历史统计也丢。
func TestUninstallKeepsDataDirs(t *testing.T) {
	root := t.TempDir()
	id := "custom-api"
	dir := filepath.Join(root, id)
	mk := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("current")
	mk("1.0.0/custom-api-ext.exe")
	mk("auths/u1.json")
	mk("logs/custom-api.log")
	mk("stat/stats.jsonl")

	m := New(root)
	if err := m.Uninstall(id, false); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	// 版本目录与指针必须删掉
	for _, rel := range []string{"current", "1.0.0"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("%s 应已删除，err=%v", rel, err)
		}
	}
	// 数据目录必须保留
	for _, rel := range []string{"auths/u1.json", "logs/custom-api.log", "stat/stats.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("%s 应保留: %v", rel, err)
		}
	}
}

// 即使旧调用传了 purge=true，卸载仍不得删除旧布局中的凭据。
func TestUninstallPurgeKeepsData(t *testing.T) {
	root := t.TempDir()
	id := "glm"
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auths", "a.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New(root).Uninstall(id, true); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "auths", "a.json")); err != nil {
		t.Errorf("旧布局凭据应保留: %v", err)
	}
}

// 未安装的插件卸载不应报错（幂等）。
func TestUninstallMissing(t *testing.T) {
	if err := New(t.TempDir()).Uninstall("nope", false); err != nil {
		t.Errorf("幂等卸载应无错: %v", err)
	}
}

// 新布局（数据不住安装树）下卸载要把目录本身删掉，否则 List 仍会列出这个空壳。
func TestUninstallRemovesDirNewLayout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "loopguard", "1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"current":                  "1.0.0",
		"1.0.0/loopguard-hook.exe": "x",
		"1.0.0/manifest.json":      `{"id":"loopguard","kind":"hook","version":"1.0.0"}`,
	} {
		p := filepath.Join(root, "loopguard", rel)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := New(root)
	if l, _ := m.List(); len(l) != 1 {
		t.Fatalf("装完应列出 1 个，实际 %d", len(l))
	}
	if err := m.Uninstall("loopguard", false); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "loopguard")); !os.IsNotExist(err) {
		t.Errorf("新布局下安装目录应整个删除，err=%v", err)
	}
	if l, _ := m.List(); len(l) != 0 {
		t.Errorf("卸载后不应再列出，实际 %+v", l)
	}
}

// 没有 current 的目录不是已装插件（卸载残留空壳 / 半途失败的临时目录）。
func TestListSkipsDirWithoutCurrent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "broken", "1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if l, err := New(root).List(); err != nil || len(l) != 0 {
		t.Fatalf("无 current 的目录不应列出: %+v err=%v", l, err)
	}
}

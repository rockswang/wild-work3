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

// purge=true 才连数据一起删（用户在面板显式确认「连凭据一起删」）。
func TestUninstallPurge(t *testing.T) {
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
		t.Fatalf("purge 卸载失败: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("purge 应删整个目录，err=%v", err)
	}
}

// 未安装的插件卸载不应报错（幂等）。
func TestUninstallMissing(t *testing.T) {
	if err := New(t.TempDir()).Uninstall("nope", false); err != nil {
		t.Errorf("幂等卸载应无错: %v", err)
	}
}

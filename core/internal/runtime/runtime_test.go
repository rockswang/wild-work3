package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPluginDataDir 钉死 W3-39 的数据根形状：插件可变量在 core 数据根之内、以插件 ID 为界，
// 不再落在安装树里（安装树只放 exe+manifest，卸载/回滚不得威胁凭据）。
func TestPluginDataDir(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "plugins"), filepath.Join(t.TempDir(), "data", "core"))
	got := m.PluginDataDir("workbuddy")
	want := filepath.Join(m.dataDir, "plugins", "workbuddy")
	if got != want {
		t.Fatalf("PluginDataDir = %s，期望 %s", got, want)
	}
	// 与安装根无交集：卸载 RemoveAll(安装根) 不可能连带删掉数据。
	if strings.HasPrefix(filepath.Clean(got), filepath.Clean(m.workDir)+string(filepath.Separator)) {
		t.Fatalf("数据根在安装树内: workDir=%s data=%s", m.workDir, got)
	}
	// 两个 core 实例各用一份凭据（数据根随 --data 走，不按父目录反推）。
	other := New(m.workDir, filepath.Join(filepath.Dir(m.dataDir), "core2"))
	if other.PluginDataDir("workbuddy") == got {
		t.Fatal("不同 core 数据目录竟共用同一插件数据根")
	}
}

// TestPreparePluginData 覆盖旧布局迁移的三条硬约束：搬得动、不覆盖、目标根必须被建出来。
func TestPreparePluginData(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "plugins") // 安装根（旧布局里同时混着 auths）
	data := filepath.Join(root, "data", "core")
	m := New(install, data)

	// 旧布局：<plugins>/workbuddy/{auths,logs,stat}，<plugins>/workbuddy/1.0.0/ 是版本目录
	write := func(rel, name, body string) string {
		p := filepath.Join(install, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(p, name)
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	cred := write("workbuddy/auths", "workbuddy-u1.json", `{"auth":{"token":"x"}}`)
	write("workbuddy/stat", "stats.jsonl", `{"kind":"chat"}`)
	if err := os.MkdirAll(filepath.Join(install, "workbuddy", "1.0.0"), 0o700); err != nil {
		t.Fatal(err)
	}

	m.preparePluginData("workbuddy", "workbuddy")

	dst := filepath.Join(m.PluginDataDir("workbuddy"), "auths", filepath.Base(cred))
	if b, err := os.ReadFile(dst); err != nil || string(b) == "" {
		t.Fatalf("凭据未迁到数据根 %s: %v", dst, err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Fatal("旧凭据应被移走（rename 成功时不留源）")
	}
	if _, err := os.Stat(filepath.Join(m.PluginDataDir("workbuddy"), "stat", "stats.jsonl")); err != nil {
		t.Fatalf("stat 未迁移: %v", err)
	}
	// 版本目录不属契约数据目录，迁移不该碰它
	if _, err := os.Stat(filepath.Join(install, "workbuddy", "1.0.0")); err != nil {
		t.Fatalf("版本目录被误删: %v", err)
	}

	// 目标已存在时不得覆盖：新数据优先，旧位置留着等人工合并。
	keep := filepath.Join(m.PluginDataDir("workbuddy"), "auths", "keep.json")
	if err := os.WriteFile(keep, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 旧位置重新出现一份凭据（源目录已随 rename 搬走，要先建回来）
	if err := os.MkdirAll(filepath.Dir(cred), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cred, []byte("old"), 0o600); err != nil {
		t.Fatalf("重建旧凭据失败: %v", err)
	}
	m.preparePluginData("workbuddy", "workbuddy")
	if b, err := os.ReadFile(keep); err != nil || string(b) != "new" {
		t.Fatalf("已有数据被覆盖: %q %v", b, err)
	}
	if b, err := os.ReadFile(cred); err != nil || string(b) != "old" {
		t.Fatalf("目标已存在时源应保留: %q %v", b, err)
	}

	// 未注册/未安装过的插件也要能算出数据根，且幂等不报错。
	m.preparePluginData("ghost", "ghost")
	if st, err := os.Stat(m.PluginDataDir("ghost")); err != nil || !st.IsDir() {
		t.Fatalf("数据根未建出: %v", err)
	}
	m.preparePluginData("ghost", "ghost")
}

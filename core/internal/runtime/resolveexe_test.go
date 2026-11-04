package runtime

// resolveExe / isExeName 的行为回归：可执行文件名是建议性约定（用户 m00634），manifest.id 才是身份。
// 覆盖：约定名优先、-hook/kind 优先级、裸 id、目录扫描兜底、多候选 id 词干优先、空目录显式失败。

import (
	"os"
	"path/filepath"
	"testing"
)

func mkExe(t *testing.T, dir string, names ...string) string {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestResolveExeConventional(t *testing.T) {
	d := mkExe(t, t.TempDir(), "wb-ext.exe", "manifest.json")
	if got := resolveExe(d, "wb", "channel"); filepath.Base(got) != "wb-ext.exe" {
		t.Fatalf("channel 应命中 <id>-ext.exe，got %q", got)
	}
	d = mkExe(t, t.TempDir(), "lg-hook.exe")
	if got := resolveExe(d, "lg", "hook"); filepath.Base(got) != "lg-hook.exe" {
		t.Fatalf("kind=hook 应优先 <id>-hook.exe，got %q", got)
	}
	// 早期把 hook 打包成 -ext：约定名兜底
	d = mkExe(t, t.TempDir(), "lg-ext.exe")
	if got := resolveExe(d, "lg", "hook"); filepath.Base(got) != "lg-ext.exe" {
		t.Fatalf("kind=hook 无 -hook 时应兜底 -ext，got %q", got)
	}
}

func TestResolveExeNonConformingName(t *testing.T) { // chatgpt-cli 形态：exe 名与 id 词干同但不带 -ext
	d := mkExe(t, t.TempDir(), "chatgpt.exe", "manifest.json")
	if got := resolveExe(d, "chatgpt", "channel"); filepath.Base(got) != "chatgpt.exe" {
		t.Fatalf("裸 id 名应命中（chatgpt-cli 合法形态），got %q", got)
	}
}

func TestResolveExeScanFallback(t *testing.T) { // exe 名完全不像任何约定：目录扫描兜底
	d := mkExe(t, t.TempDir(), "weird-binary-name.exe", "manifest.json")
	if got := resolveExe(d, "weird", "channel"); filepath.Base(got) != "weird-binary-name.exe" {
		t.Fatalf("任意 exe 名应被目录扫描兜底，got %q", got)
	}
	// 多候选：id 词干优先于字典序
	d = mkExe(t, t.TempDir(), "aaa-tool.exe", "zzz-main.exe", "zzz-main.exe.bak")
	d2 := mkExe(t, t.TempDir(), "aaa-tool.exe", "zzz-main.exe")
	if got := resolveExe(d2, "zzz", "channel"); filepath.Base(got) != "zzz-main.exe" {
		t.Fatalf("多候选应优先 id 词干命中，got %q", got)
	}
	_ = d
	// 非可执行文件不参选
	d3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(d3, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveExe(d3, "x", "channel"); got != "" {
		t.Fatalf("目录里只有 manifest.json 应返回空串，got %q", got)
	}
}

func TestResolveExeEmpty(t *testing.T) {
	if got := resolveExe(t.TempDir(), "x", "channel"); got != "" {
		t.Fatalf("空目录应返回空串（不再编造路径假加载），got %q", got)
	}
}

func TestIsExeName(t *testing.T) {
	for _, ok := range []string{"a.exe", "A.EXE", "run.bat", "chatgpt-cli", "tool.sh"} {
		if !isExeName(ok) {
			t.Errorf("%q 应视为可执行", ok)
		}
	}
	for _, no := range []string{"manifest.json", "README.md", "notes.txt", "data.zip", "a.json"} {
		if isExeName(no) {
			t.Errorf("%q 不应视为可执行", no)
		}
	}
}

package stat

import (
	"os"
	"path/filepath"
	"testing"
)

// 旧目录名 ledger/ 必须整体搬到 stat/（W3-40 术语统一，账不能丢）。
func TestMigrateLegacyDir(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "ledger")
	cur := filepath.Join(root, "stat")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "usage-202601.jsonl"), []byte(`{"ts":"2026-01-01T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateLegacyDir(cur)
	if _, err := os.Stat(filepath.Join(cur, "usage-202601.jsonl")); err != nil {
		t.Fatalf("迁移后账本应在新目录: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("旧目录应已消失: %v", err)
	}
}

// 目标已存在时不合并、不动旧目录（绝不拿新数据盖旧账）。
func TestMigrateLegacyDirNoOverwrite(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "ledger")
	cur := filepath.Join(root, "stat")
	for _, d := range []string{old, cur} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	migrateLegacyDir(cur)
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("目标已存在时旧目录应原样保留: %v", err)
	}
}

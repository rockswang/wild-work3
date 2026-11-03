package stat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countBodyTrace 统计目录下的 body-trace 分片数（Open 会建 usage/credit 流水，不能整目录计数）。
func countBodyTrace(dir string) int {
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "body-trace-") {
			n++
		}
	}
	return n
}

// TestSummarizeBodyShape 摘要形状：条数/角色分布/字符数/工具定义/Top10 降序。
func TestSummarizeBodyShape(t *testing.T) {
	body := `{"model":"gpt-x","messages":[` +
		`{"role":"system","content":"你是助手"},` +
		`{"role":"user","content":"开场问题：读这个文件"},` +
		`{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("长", 300) + `"}]},` +
		`{"role":"tool","content":"结果"}` +
		`],"tools":[{"type":"function","function":{"name":"read"}}]}`
	e, key := summarizeBody("ch1", "gpt-x", []byte(body))
	if e.Msgs != 4 {
		t.Fatalf("msgs=%d want 4", e.Msgs)
	}
	if e.Roles["system"] != 1 || e.Roles["user"] != 1 || e.Roles["assistant"] != 1 || e.Roles["tool"] != 1 {
		t.Errorf("角色分布不符: %v", e.Roles)
	}
	if e.SysChar != len([]rune("你是助手")) {
		t.Errorf("sys_char=%d want %d", e.SysChar, len([]rune("你是助手")))
	}
	if e.ToolsLen == 0 {
		t.Error("tools_len 应大于 0（请求带了 tools）")
	}
	if len(e.Top) != 4 || e.Top[0].Role != "assistant" {
		t.Errorf("Top 应按体量降序且首条是最大的 assistant: %v", e.Top)
	}
	// 预览截断到 200 字 + 省略号
	if n := len([]rune(e.Top[0].Preview)); n != previewChars+1 {
		t.Errorf("preview 长度=%d want %d", n, previewChars+1)
	}
	// 会话指纹取首条 user 文本，不是 system
	if e.Head != preview("开场问题：读这个文件") {
		t.Errorf("head=%q 应取首条 user 文本", e.Head)
	}
	if key == bodyTraceNoSess || len(key) != 10 {
		t.Errorf("会话指纹=%q 应为 10 位十六进制", key)
	}
}

// TestSummarizeBodyMalformed 畸形请求体只保留字节数，不得报错（诊断工具不能阻塞主链路）。
func TestSummarizeBodyMalformed(t *testing.T) {
	e, key := summarizeBody("ch", "m", []byte(`not json`))
	if e.Bytes != 8 || e.Msgs != 0 || key != bodyTraceNoSess {
		t.Fatalf("畸形请求应只记字节数: %+v key=%q", e, key)
	}
}

// TestBodyTraceGatedBySwitch 关闭时不落任何文件（默认关，近乎零开销）。
func TestBodyTraceGatedBySwitch(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	SetBodyTrace(false)
	l.TraceBody("ch", "m", []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if n := countBodyTrace(dir); n != 0 {
		t.Fatalf("关闭时不应产生 body-trace 文件，实际 %d 个", n)
	}
	SetBodyTrace(true)
	defer SetBodyTrace(false)
	l.TraceBody("ch", "m", []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	files := []string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "body-trace-") {
			files = append(files, e.Name())
		}
	}
	if len(files) != 1 {
		t.Fatalf("开启后应有一个 body-trace 分片，实际 %v", files)
	}
	// 同一会话（同一开场问题）必须写同一个文件
	l.TraceBody("ch", "m", []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"user","content":"2"}]}`))
	if n := countBodyTrace(dir); n != 1 {
		t.Fatalf("同一会话不应分片，实际 %d 个", n)
	}
	// 落盘内容必须是合法 JSONL 且带预览
	b, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行，实际 %d", len(lines))
	}
	var e BodyTraceEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if e.Ch != "ch" || e.Model != "m" || len(e.Top) != 1 || e.Top[0].Preview != "hi" {
		t.Errorf("落盘字段不符: %+v", e)
	}
}

// TestCleanupBodyTrace 过期分片按修改时间清理，未过期的保留。
func TestCleanupBodyTrace(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "body-trace-aaaaaaaaaa.jsonl")
	fresh := filepath.Join(dir, "body-trace-bbbbbbbbbb.jsonl")
	other := filepath.Join(dir, "usage.jsonl")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 把 old 的修改时间推到 8 天前
	past := time.Now().AddDate(0, 0, -8)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	cleanupBodyTrace(dir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("过期分片应被删除")
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s 不应被删除: %v", filepath.Base(p), err)
		}
	}
}

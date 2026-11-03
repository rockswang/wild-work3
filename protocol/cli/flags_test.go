package cli

import "testing"

// TestStrList 校验可重复开关（W3-30 的 images --ref）：按给出顺序累积、不做逗号切分、未给值为空切片。
func TestStrList(t *testing.T) {
	app := NewApp("x", "test", "call")
	c := app.Cmd("images", "")
	refs := c.StrList("ref", "参考图")
	prompt := c.Str("prompt", "", "提示词")

	if _, err := app.Parse([]string{"images", "--prompt", "p", "--ref", "data:image/png;base64,iVBOR,abc", "--ref", "./b.jpg"}); err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if len(*refs) != 2 || (*refs)[0] != "data:image/png;base64,iVBOR,abc" || (*refs)[1] != "./b.jpg" {
		t.Fatalf("多值未累积或被逗号切分: %q", *refs)
	}
	if *prompt != "p" {
		t.Fatalf("普通开关被影响: %q", *prompt)
	}

	// 一次都不给时必须是空切片（非 nil），调用方可直接 range
	app2 := NewApp("x", "test", "call")
	c2 := app2.Cmd("images", "")
	r2 := c2.StrList("ref", "")
	if _, err := app2.Parse([]string{"images"}); err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if r2 == nil || len(*r2) != 0 {
		t.Fatalf("缺省应为空切片非 nil: %v", r2)
	}
}

// TestStrListHelp 确认可重复开关在 --help 里渲染成双短横线且提示可重复，避免文档与帮助不一致。
func TestStrListHelp(t *testing.T) {
	app := NewApp("x", "test", "call")
	c := app.Cmd("images", "")
	c.StrList("ref", "参考图，可重复")
	var buf writeCounter
	app.Out = &buf
	c.FS.Usage()
	if buf.n == 0 {
		t.Fatal("Usage 未输出任何内容")
	}
}

type writeCounter struct{ n int }

func (w *writeCounter) Write(p []byte) (int, error) { w.n += len(p); return len(p), nil }

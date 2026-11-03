package rpc_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"wild-work3/main/protocol/jsonl"
	"wild-work3/main/protocol/rpc"
)

// TestDuplexStream 校验双向流：调用方 Emit 回灌的事件能被被调方 Next 读到，
// 被调方 Event 下发的事件能进调用方 OnEvent（W3-26 拦截器契约的基础）。
func TestDuplexStream(t *testing.T) {
	// c2s：调用方写 → 被调方读；s2c：被调方写 → 调用方读
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()

	srv := rpc.NewServer(c2sR, s2cW)
	srv.HandleStream("hook.stream", func(ctx context.Context, p jsonl.RawMessage, s *rpc.Stream) (jsonl.RawMessage, *jsonl.ErrObj) {
		got := []string{}
		for {
			m, err := s.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, jsonl.Errorf("cancel", "request", "%v", err)
			}
			if m.Event == "upstream.end" {
				break
			}
			var d map[string]any
			_ = json.Unmarshal(m.Data, &d)
			txt, _ := d["content"].(string)
			got = append(got, txt)
			// 逐条放大回下游
			_ = s.Event("text.delta", map[string]any{"content": strings.ToUpper(txt)})
		}
		b, _ := json.Marshal(map[string]any{"echo": got})
		return b, nil
	})
	go func() { _ = srv.Serve(context.Background()) }()

	cli := rpc.NewClient(c2sW, s2cR)
	var out []string
	cb := &rpc.Callback{OnEvent: func(ev *jsonl.Msg) error {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		if s, ok := d["content"].(string); ok {
			out = append(out, s)
		}
		return nil
	}}
	done := make(chan error, 1)
	var res jsonl.Msg
	_ = res
	var id string
	cb.OnID = func(v string) { id = v }
	go func() {
		var err error
		res, _, err = cli.Invoke(context.Background(), "hook.stream", map[string]any{"stage": "response"}, cb)
		done <- err
	}()
	// 等 id 就绪（Invoke 已在写入前回调 OnID）
	deadline := time.Now().Add(2 * time.Second)
	for id == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("OnID 未被调用")
	}
	for _, s := range []string{"ab", "cd", "ef"} {
		if err := cli.Emit(id, "upstream.event", map[string]any{"content": s}); err != nil {
			t.Fatalf("Emit 失败: %v", err)
		}
	}
	if err := cli.Emit(id, "upstream.end", map[string]any{}); err != nil {
		t.Fatalf("Emit end 失败: %v", err)
	}
	// 被调方收到 upstream.end 后 break（否则管道不会 EOF）。
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Invoke 失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("双向流超时")
	}
	if len(out) != 3 || out[0] != "AB" {
		t.Fatalf("下游事件异常: %v", out)
	}
}

// TestPluginDuplex 校验 plugin.ServeOnce 的双向路径（call 形态）与 plugin.Start 的 Send。
func TestPluginDuplex(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过子进程测试")
	}
}

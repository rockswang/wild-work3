// 弃源判据的回归：读体必须与定时器在同一个 select 里争抢。
//
// 曾经的实现是「主循环 Read → select 定时器」，select 会阻塞到下一个定时器到期，
// 于是**一个吞吐窗口里只可能读到一坨数据**。上游若按小块写（net/http 的 ServeContent
// 就是 512B 一片），健康源会被误判成 1KB/s 而弃掉——表现是「本地/正常镜像下载必失败，
// 只有整块返回的源能过」。本测试用 512B 分片 + 微间隔的源做断言。
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchFileChunkedSourceIsNotAborted(t *testing.T) {
	body := bytes.Repeat([]byte("CHUNK-"), 6000) // 36KB，正好被 512 整除
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")
		for i := 0; i < len(body); i += 512 { // 逐片写并 flush，模拟真实分片传输
			_, _ = w.Write(body[i:min(i+512, len(body))])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(200 * time.Microsecond)
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.bin")
	o := Options{FirstByte: 2 * time.Second, Idle: 2 * time.Second, StallWin: 400 * time.Millisecond, MinRate: 200 * 1024}
	got, err := FetchFile(context.Background(), []string{srv.URL + "/pkg.bin"}, dest, hex.EncodeToString(sum[:]), o, nil)
	if err != nil {
		t.Fatalf("健康分片源被误弃: %v", err)
	}
	if !bytes.Contains([]byte(got), []byte("pkg.bin")) {
		t.Errorf("应返回命中源: %s", got)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, body) {
		t.Errorf("落盘内容不一致: %d vs %d", len(b), len(body))
	}
}

// TestFetchFileStallsSlowSource 反证：真的慢（<20KB/s）仍须被弃，别把修复做成「不判吞吐」。
func TestFetchFileStallsSlowSource(t *testing.T) {
	body := bytes.Repeat([]byte("SLOW-SND"), 2000) // 16KB
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < len(body); i += 512 {
			select {
			case <-r.Context().Done(): // 客户端已弃源，早退（否则 srv.Close() 要等整个 4.8s 写完）
				return
			default:
			}
			_, _ = w.Write(body[i:min(i+512, len(body))])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(150 * time.Millisecond) // ≈3.4KB/s
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.bin")
	o := Options{FirstByte: time.Second, Idle: 3 * time.Second, StallWin: 500 * time.Millisecond, MinRate: 20 * 1024}
	start := time.Now()
	_, err := FetchFile(context.Background(), []string{srv.URL + "/pkg.bin"}, dest, hex.EncodeToString(sum[:]), o, nil)
	if err == nil {
		t.Fatalf("慢源本该被弃，却成功了")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("吞吐过低")) {
		t.Errorf("失败原因应是吞吐判据: %v", err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("弃源太慢（软失败会永久卡 downloading）: %s", d)
	}
}

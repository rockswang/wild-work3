package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourcesGeneration(t *testing.T) {
	g := GitHubRelease{OwnerRepo: "wild-work3/ww3-registry", Asset: "index.json"}
	if got := g.Direct(); got != "https://github.com/wild-work3/ww3-registry/releases/latest/download/index.json" {
		t.Fatalf("直连 URL 错: %s", got)
	}
	g.Tag = "v1.2.3"
	if got := g.Direct(); !strings.Contains(got, "/releases/download/v1.2.3/") {
		t.Fatalf("带 tag 的 URL 错: %s", got)
	}
	g.Tag = ""
	srcs := g.Sources([]string{"https://gh-proxy.com", "https://ghfast.top/", " ", "not-a-url"})
	if len(srcs) != 3 {
		t.Fatalf("应展开成 1 直连 + 2 代理，实得 %d: %v", len(srcs), srcs)
	}
	if !strings.HasPrefix(srcs[1], "https://gh-proxy.com/https://github.com/") {
		t.Fatalf("代理前缀拼接错: %s", srcs[1])
	}
	hosts := Hosts([]GitHubRelease{g}, []string{"https://gh-proxy.com/"})
	if !hosts["github.com"] || !hosts["gh-proxy.com"] {
		t.Fatalf("allowlist 缺项: %v", hosts)
	}
	if hosts["evil.com"] {
		t.Fatal("allowlist 不应包含未声明主机")
	}
}

func TestParseIndexHardChecks(t *testing.T) {
	base := `{"version":1,"plugins":[{"id":"a","kind":"channel","repo":"o/r","asset":"a.zip","versions":[%s]}]}`
	good := `{"version":"1.0.0","sha256":"` + strings.Repeat("ab", 32) + `"}`
	if _, err := ParseIndex([]byte(fmt.Sprintf(base, good))); err != nil {
		t.Fatalf("合法清单被拒: %v", err)
	}
	// sha256 缺失（W3-38 起必填）
	if _, err := ParseIndex([]byte(fmt.Sprintf(base, `{"version":"1.0.0"}`))); err == nil {
		t.Fatal("sha256 缺失必须报错")
	}
	// kind 非法
	if _, err := ParseIndex([]byte(`{"plugins":[{"id":"a","kind":"channel-x","repo":"o/r","asset":"a.zip","versions":[{"version":"1","sha256":"` + strings.Repeat("0", 64) + `"}]}]}`)); err == nil {
		t.Fatal("kind 非法必须报错")
	}
	// 有 repo 无 asset
	if _, err := ParseIndex([]byte(`{"plugins":[{"id":"a","kind":"hook","repo":"o/r","versions":[{"version":"1","sha256":"` + strings.Repeat("0", 64) + `"}]}]}`)); err == nil {
		t.Fatal("缺 asset 名必须报错")
	}
	// id 重复
	dup := `{"version":1,"plugins":[{"id":"a","kind":"channel","repo":"o/r","asset":"a.zip","versions":[]},{"id":"a","kind":"channel","repo":"o/r","asset":"b.zip","versions":[]}]}`
	if _, err := ParseIndex([]byte(dup)); err == nil {
		t.Fatal("id 重复必须报错")
	}
}

func TestVerifyAndGate(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"version":1,"plugins":[]}`)
	sig := hex.EncodeToString(ed25519.Sign(priv, payload))
	pubHex := hex.EncodeToString(pub)
	if err := Verify([]string{pubHex}, payload, sig); err != nil {
		t.Fatalf("验签应通过: %v", err)
	}
	if err := Verify([]string{pubHex}, append(payload, '!'), sig); err == nil {
		t.Fatal("内容被改必须验签失败")
	}
	if err := Verify(nil, payload, sig); err == nil {
		t.Fatal("无公钥时必须拒绝采信")
	}
	if err := Verify([]string{pubHex}, payload, "deadbeef"); err == nil {
		t.Fatal("签名格式错必须报错")
	}

	p := PluginEntry{ID: "a", Versions: []AssetEntry{{Version: "1", Protocol: ProtocolRng{Min: 1, Max: 2},
		Gate: VersionGate{Min: "1.0.0", Max: "1.9.0"}}}}
	v := p.Versions[0]
	if err := CheckGate(p, v, 1, "1.2.0"); err != nil {
		t.Fatalf("区间内应通过: %v", err)
	}
	if err := CheckGate(p, v, 3, "1.2.0"); err == nil || !strings.Contains(err.Error(), "≤2") {
		t.Fatalf("协议超上限应拒: %v", err)
	}
	if err := CheckGate(p, v, 1, "0.9.0"); err == nil || !strings.Contains(err.Error(), "gateway ≥1.0.0") {
		t.Fatalf("gateway 低于下限应拒: %v", err)
	}
	if err := CheckGate(p, v, 1, "2.0.0"); err == nil {
		t.Fatal("gateway 超上限应拒")
	}
	for _, c := range [][3]any{{"1.10.0", "1.9.0", 1}, {"1.0.0", "v1.0", 0}, {"1.0.1", "1.0.2", -1}} {
		if got := CmpSemver(c[0].(string), c[1].(string)); got != c[2].(int) {
			t.Fatalf("cmpSemver(%s,%s)=%d 期望 %d", c[0], c[1], got, c[2].(int))
		}
	}
}

// 竞速：一个源秒回、一个源慢到触发吞吐判据、一个源回 HTML 错误页 → 必须取秒回的那个。
func TestFetchRacingPicksFastSource(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":1}`))
	}))
	defer fast.Close()
	body := strings.Repeat("a", 400_000) // 低于 MaxBody，但配合 tiny 判据会被弃
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		for i := 0; i < 200; i++ { // 每次 2KB，间隔 200ms → 约 10KB/s
			if _, err := w.Write([]byte(body[i*2000 : i*2000+2000])); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(200 * time.Millisecond)
		}
	}))
	defer slow.Close()
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(strings.Repeat("<html>big page</html>", 100000)))
	}))
	defer html.Close()

	ctx := context.Background()
	o := Options{FirstByte: 2 * time.Second, Idle: time.Second, StallWin: 500 * time.Millisecond,
		MinRate: 100 * 1024, MaxBody: 1 << 20, Total: 20 * time.Second}
	start := time.Now()
	got, src, err := FetchURLs(ctx, []string{slow.URL, html.URL, fast.URL}, o)
	if err != nil {
		t.Fatalf("竞速应有一个源成功: %v", err)
	}
	if string(got) != `{"version":1}` || src != fast.URL {
		t.Fatalf("赢家应是快源: src=%s body=%q", src, got)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("并发竞速不该等慢源，耗时 %v", d)
	}
}

func TestFetchAllSourcesFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer bad.Close()
	o := Options{Total: 3 * time.Second}
	_, _, err := FetchURLs(context.Background(), []string{bad.URL, bad.URL + "/2"}, o)
	if err == nil {
		t.Fatal("全失败必须报错")
	}
	var e *ErrAllSources
	if !asErrAll(err, &e) || len(e.Reasons) != 2 {
		t.Fatalf("应带逐源原因: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("原因里要能看到状态码: %v", err)
	}
}

func TestFetchFileSHAAndResume(t *testing.T) {
	payload := []byte("wild-work3 插件包内容，含中文与二进制\x00\xff")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	// 支持 Range 的源
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=5-" {
			w.WriteHeader(http.StatusPartialContent)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(payload)-1, len(payload)))
			w.Write(payload[5:])
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "pkg.zip")
	if _, err := FetchFile(context.Background(), []string{srv.URL}, dest, "0123", Options{Total: 5 * time.Second}, nil); err == nil {
		t.Fatal("sha256 格式非法必须直接拒")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("非法 sha 不该落盘")
	}
	// 续传：留一个 5 字节半包，服务端对 Range 回 206，结果必须仍是完整包
	half := filepath.Join(dir, "resume.zip")
	if err := os.WriteFile(half, payload[:5], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchFile(context.Background(), []string{srv.URL}, half, want, Options{Total: 10 * time.Second}, nil); err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	if b, _ := os.ReadFile(half); string(b) != string(payload) {
		t.Fatalf("续传结果不完整: %d 字节", len(b))
	}
	// 校验失败必须删掉半包（避免永远卡在坏包上）
	if _, err := FetchFile(context.Background(), []string{srv.URL + "/other"}, filepath.Join(dir, "x.zip"), strings.Repeat("ff", 32), Options{Total: 5 * time.Second}, nil); err == nil {
		t.Fatal("摘要不符必须报错")
	} else if !strings.Contains(err.Error(), "SHA256 mismatch from") {
		t.Fatalf("失败原因需归因到源: %v", err)
	}
}

func TestProbe(t *testing.T) {
	var rangeHdr []string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr = append(rangeHdr, r.Header.Get("Range"))
		w.Write([]byte(strings.Repeat("z", ProbeBytes)))
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", http.StatusNotFound)
	}))
	defer bad.Close()

	n, src, err := ProbeURLs(context.Background(), []string{bad.URL, ok.URL}, Options{Total: 5 * time.Second})
	if err != nil {
		t.Fatalf("可达源应成功: %v", err)
	}
	if n != ProbeBytes || src != ok.URL {
		t.Fatalf("探测结果异常: n=%d src=%s", n, src)
	}
	for _, h := range rangeHdr {
		if h == "" {
			t.Fatal("Probe 必须带 Range，不得整包拉")
		}
	}
	if _, _, err := ProbeURLs(context.Background(), []string{bad.URL}, Options{Total: 5 * time.Second}); err == nil {
		t.Fatal("404 必须报错")
	}
}

func asErrAll(err error, target **ErrAllSources) bool {
	for err != nil {
		if e, ok := err.(*ErrAllSources); ok {
			*target = e
			return true
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
			continue
		}
		return false
	}
	return false
}

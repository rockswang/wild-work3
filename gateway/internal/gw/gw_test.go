// W3-38 分发链的本地端到端验证：清单 → 商店 → 安装（版本门 / sha256 / host 白名单）
// → core 更新（检查 / 下载 / 应用 / 回滚）。全部走 httptest，零真实外网请求。
package gw

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work3/main/gateway/internal/registry"
	"wild-work3/main/gateway/internal/store"
)

// gh 假装自己是 GitHub release 资产目录：按路径发文件，且支持 Range（等价于真实 release 下载）。
type gh struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
}

func newGH(t *testing.T) *gh {
	g := &gh{t: t, files: map[string][]byte{}, hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		g.mu.Lock()
		g.hits[key]++
		body, ok := g.files[key]
		g.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, key, time.Unix(1700000000, 0), bytes.NewReader(body))
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gh) put(name string, b []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[name] = b
}

func (g *gh) hit(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[name]
}

func shaHex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func js(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pluginZip 打一个合法插件包（manifest.json + exe 壳）。
func pluginZip(t *testing.T, id, kind, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mj, _ := json.Marshal(map[string]any{
		"id": id, "kind": kind, "version": version, "name": id,
		"capabilities": map[string][]string{"control": {"models.list"}, "conversation": {"chat.generate"}},
	})
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write(mj)
	e, _ := zw.Create(id + "-ext.exe")
	_, _ = e.Write([]byte("FAKE-EXE-" + version))
	_ = zw.Close()
	return buf.Bytes()
}

// fastOpts 测试用竞速判据（生产的 8s/15s 在本地毫无意义）。
func fastOpts() registry.Options {
	return registry.Options{FirstByte: 2 * time.Second, Idle: 1 * time.Second, StallWin: 500 * time.Millisecond, MinRate: 1024}
}

// env 一次测试的目录与 Server。
type env struct {
	s       *Server
	mux     *http.ServeMux
	root    string
	coreExe string
	halted  int
	started int
}

func newEnv(t *testing.T, g *gh) *env {
	t.Helper()
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	coreExe := filepath.Join(root, "ww3-core.exe")
	if err := os.WriteFile(coreExe, []byte("OLD-CORE"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &env{root: root, coreExe: coreExe}
	e.s = &Server{
		Store:   store.New(plugins),
		Version: map[string]string{"gateway": "1.0.0", "protocol": "1"},
		Dist: Dist{
			Registry:    registry.Source{URL: g.srv.URL + "/index.json", Name: "official"},
			Main:        registry.Source{URL: g.srv.URL + "/update.json", Name: "main"},
			ExtraHosts:  []string{"127.0.0.1"}, // httptest 本机必须在 allowlist 里
			ProtocolVer: 1,
			Fetch:       fastOpts(),
			TTL:         time.Minute,
		},
		CoreExe:    coreExe,
		GatewayExe: filepath.Join(root, "wild-work3.exe"),
		UpdateDir:  filepath.Join(root, "updates"),
		HaltCore:   func() error { e.halted++; return nil },
		StartCore:  func() { e.started++ },
	}
	e.mux = http.NewServeMux()
	e.s.Routes(e.mux)
	return e
}

func (e *env) get(path string) *httptest.ResponseRecorder {
	return e.req(http.MethodGet, path, "")
}

func (e *env) post(path string) *httptest.ResponseRecorder {
	return e.req(http.MethodPost, path, "")
}

func (e *env) body(path, body string) *httptest.ResponseRecorder {
	return e.req(http.MethodPost, path, body)
}

func (e *env) req(method, path, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	e.mux.ServeHTTP(rr, httptest.NewRequest(method, path, rd))
	return rr
}

func decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("非法 JSON (%d): %s", rr.Code, rr.Body.String())
	}
	return v
}

func decodeList(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var v []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("期望裸数组 (%d): %s", rr.Code, rr.Body.String())
	}
	return v
}

// ---- 商店与安装 ----

func TestStoreListAndInstall(t *testing.T) {
	g := newGH(t)
	det := js(t, map[string]any{
		"id": "demo-ext", "description": "只用于测试",
		"versions": []map[string]any{{
			"version": "1.2.0", "url": g.srv.URL + "/assets/demo.zip",
			"sha256": "占位", "size": 0, "protocol": map[string]int{"min": 1, "max": 3},
		}},
	})
	g.put("plugins/demo-ext.json", det)
	idx := js(t, map[string]any{
		"version": 1, "updated_at": "2026-11-02T00:00:00Z",
		"plugins": []map[string]any{{
			"id": "demo-ext", "name": "演示渠道", "kind": "channel",
			"file": "plugins/demo-ext.json", "digest": shaHex(det),
		}},
	})
	g.put("index.json", idx)

	e := newEnv(t, g)
	// 版本包内容（先不知道摘要，故在装之前写盘并回填索引两次）
	pkg := pluginZip(t, "demo-ext", "channel", "1.2.0")
	g.put("assets/demo.zip", pkg)
	// 把真实 sha/size 回填进详情文件与索引摘要
	det = js(t, map[string]any{
		"id": "demo-ext", "description": "只用于测试",
		"versions": []map[string]any{{
			"version": "1.2.0", "url": g.srv.URL + "/assets/demo.zip",
			"sha256": shaHex(pkg), "size": len(pkg), "protocol": map[string]int{"min": 1, "max": 3},
		}},
	})
	g.put("plugins/demo-ext.json", det)
	g.put("index.json", js(t, map[string]any{
		"version": 1, "updated_at": "2026-11-02T00:00:00Z",
		"plugins": []map[string]any{{
			"id": "demo-ext", "name": "演示渠道", "kind": "channel",
			"file": "plugins/demo-ext.json", "digest": shaHex(det), "latest": "1.2.0",
		}},
	}))
	e.s.cache.Invalidate("") // 清掉刚才那份占位索引

	list := decodeList(t, e.get("/gw/plugins/store"))
	if len(list) != 1 {
		t.Fatalf("商店应有 1 项: %v", list)
	}
	it := list[0]
	if it["plugin_id"] != "demo-ext" || it["latest"] != "1.2.0" {
		t.Errorf("条目形状不对: %v", it)
	}
	if it["catalog_signed"] != false {
		t.Errorf("未内置公钥时必须标未验签: %v", it["catalog_signed"])
	}
	// 指针条目：首屏不展开详情（只靠索引声明的 latest），versions 为空、detail=false
	if v, ok := it["versions"].([]any); !ok || len(v) != 0 {
		t.Errorf("指针条目的首屏不该带 versions: %v", it["versions"])
	}
	if it["detail"] != false {
		t.Errorf("detail 应为 false: %v", it["detail"])
	}
	// 显式展开后才有版本列表（前端点开下拉时用）
	exp := decodeList(t, e.get("/gw/plugins/store?expand=1"))
	if v, ok := exp[0]["versions"].([]any); !ok || len(v) != 1 {
		t.Errorf("expand=1 应给出 versions: %v", exp[0])
	}
	if exp[0]["detail"] != true {
		t.Errorf("展开后 detail 应为 true: %v", exp[0]["detail"])
	}

	// 安装
	rr := e.body("/gw/plugins/install", `{"plugin_id":"demo-ext"}`)
	if rr.Code != 200 {
		t.Fatalf("安装失败 %d: %s", rr.Code, rr.Body.String())
	}
	got := decode(t, rr)
	if got["ok"] != true || got["version"] != "1.2.0" {
		t.Fatalf("安装响应不对: %v", got)
	}
	if !strings.Contains(got["source"].(string), "demo.zip") {
		t.Errorf("应回命中源: %v", got["source"])
	}
	ver := filepath.Join(e.root, "plugins", "demo-ext", "1.2.0")
	if b, err := os.ReadFile(filepath.Join(ver, "manifest.json")); err != nil || !bytes.Contains(b, []byte("demo-ext")) {
		t.Fatalf("包未落地: %v %s", err, b)
	}
	if cur, err := os.ReadFile(filepath.Join(e.root, "plugins", "demo-ext", "current")); err != nil || strings.TrimSpace(string(cur)) != "1.2.0" {
		t.Fatalf("current 指针没切: %q %v", cur, err)
	}

	// /gw/plugins 的 latest 只读缓存，不得再打外网
	before := g.hit("index.json")
	rr = e.get("/gw/plugins")
	if !strings.Contains(rr.Body.String(), `"latest":"1.2.0"`) {
		t.Errorf("已安装列表应带清单最新号: %s", rr.Body.String())
	}
	if g.hit("index.json") != before {
		t.Errorf("/gw/plugins 不该重拉清单（应只用缓存）")
	}

	// 手动刷新清单：必须真的再打一次
	if rr := e.post("/gw/registry/refresh"); rr.Code != 200 {
		t.Fatalf("refresh 失败: %d %s", rr.Code, rr.Body.String())
	}
	if g.hit("index.json") <= before {
		t.Errorf("refresh 后应重拉清单")
	}
}

// 三道硬门：sha 不符 / 版本门不过 / 主机不在白名单。
func TestInstallHardGates(t *testing.T) {
	cases := []struct {
		name  string
		url   string // 空 = 用本地 gh
		sha   string // 合法/非法摘要
		proto map[string]int
		want  string
	}{
		{name: "sha 不符", sha: strings.Repeat("ab", 32), want: "SHA256 mismatch"},
		{name: "协议版本门", sha: "", proto: map[string]int{"min": 9}, want: "协议"},
		{name: "主机白名单", url: "https://evil.example.com/pkg.zip", want: "允许列表"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGH(t)
			pkg := pluginZip(t, "gate-ext", "channel", "1.0.0")
			g.put("assets/gate.zip", pkg)
			u := c.url
			if u == "" {
				u = g.srv.URL + "/assets/gate.zip"
			}
			sha := c.sha
			if sha == "" {
				sha = shaHex(pkg)
			}
			ver := map[string]any{"version": "1.0.0", "url": u, "sha256": sha, "size": len(pkg)}
			if c.proto != nil {
				ver["protocol"] = c.proto
			}
			det := js(t, map[string]any{"id": "gate-ext", "versions": []map[string]any{ver}})
			g.put("plugins/gate-ext.json", det)
			g.put("index.json", js(t, map[string]any{
				"version": 1,
				"plugins": []map[string]any{{"id": "gate-ext", "kind": "channel", "file": "plugins/gate-ext.json", "digest": shaHex(det)}},
			}))

			e := newEnv(t, g)
			rr := e.body("/gw/plugins/install", `{"plugin_id":"gate-ext"}`)
			if rr.Code == 200 {
				t.Fatalf("本该拒装却成功了: %s", rr.Body.String())
			}
			msg := decode(t, rr)["error"]
			if !strings.Contains(msg.(string), c.want) {
				t.Errorf("错误文案缺 %q: %v", c.want, msg)
			}
			// 拒装不得留下 current 指针（装了再说 = 半成品砖）
			if _, err := os.Stat(filepath.Join(e.root, "gate-ext", "current")); err == nil {
				t.Errorf("拒装后不该有 current")
			}
		})
	}
}

// ---- core 更新 ----

func TestUpdatesDownloadApplyRollback(t *testing.T) {
	g := newGH(t)
	newCore := bytes.Repeat([]byte("NEW-CORE-"), 4096) // ~36KB，够跑进度回调
	g.put("core/ww3-core.exe", newCore)
	g.put("update.json", js(t, map[string]any{
		"version": "1.1.0", "notes": "修了几个 bug",
		"gateway": map[string]string{"min": "1.0.0", "max": "2.0.0"},
		"assets": []map[string]any{{
			"version": "1.1.0", "asset": "ww3-core.exe", "sha256": shaHex(newCore),
			"size": len(newCore), "os": runtime.GOOS, "arch": runtime.GOARCH,
			"url": g.srv.URL + "/core/ww3-core.exe", "changelog": "本轮改动",
		}},
	}))

	e := newEnv(t, g)

	rr := e.get("/gw/updates")
	out := decode(t, rr)
	gwv := out["gateway"].(map[string]any)
	if gwv["update"] != false || gwv["manual"] != true {
		t.Errorf("gateway 分支必须恒报手动升级: %v", gwv)
	}
	note, _ := gwv["note"].(string)
	if !strings.Contains(note, "wild-work3.exe") || !strings.Contains(note, "改名") {
		t.Errorf("note 要写清换哪个文件、怎么换: %q", note)
	}
	if rel, _ := gwv["release"].(string); rel == "" {
		t.Errorf("必须给出可点开的 release 位置（手动升级要用）: %q", rel)
	}
	core := out["core"].(map[string]any)
	if core["update"] != true || core["latest"] != "1.1.0" || core["current"] != "" {
		t.Fatalf("core 应报可升级: %v", core)
	}
	if core["changelog"] != "本轮改动" {
		t.Errorf("changelog 没透出: %v", core["changelog"])
	}
	if v, ok := core["version_gate"].(map[string]any); !ok || v["min"] != "1.0.0" {
		t.Errorf("版本门没透出: %v", core["version_gate"])
	}
	if out["catalog"] == nil {
		t.Errorf("应带 catalog 状态（清单从哪拿的、多旧）")
	}

	// 下载：立即返回 + 轮询到 ready
	if rr := e.post("/gw/updates/download"); rr.Code != 200 {
		t.Fatalf("download 失败 %d: %s", rr.Code, rr.Body.String())
	}
	st := waitJob(t, e, "ready", 10*time.Second)
	if p, _ := st["progress"].(float64); int(p) != 100 {
		t.Errorf("进度应到 100: %v", st)
	}
	if b, _ := st["downloaded_bytes"].(float64); int64(b) != int64(len(newCore)) {
		t.Errorf("字节数应等于包长: %v", st["downloaded_bytes"])
	}
	if !strings.Contains(st["source"].(string), "ww3-core.exe") {
		t.Errorf("应记命中源: %v", st["source"])
	}
	// 幂等：已在 ready 再点下载，重新起一轮而不是报错
	if rr := e.post("/gw/updates/download"); rr.Code != 200 {
		t.Errorf("重复 download 应幂等: %d %s", rr.Code, rr.Body.String())
	}
	waitJob(t, e, "ready", 10*time.Second)

	// 应用
	if rr := e.post("/gw/updates/apply"); rr.Code != 200 {
		t.Fatalf("apply 失败 %d: %s", rr.Code, rr.Body.String())
	}
	if b, err := os.ReadFile(e.coreExe); err != nil || !bytes.Equal(b, newCore) {
		t.Fatalf("新包没到位: err=%v len=%d", err, len(b))
	}
	if b, err := os.ReadFile(e.coreExe + ".old"); err != nil || string(b) != "OLD-CORE" {
		t.Fatalf("旧版没留作回滚: %q %v", b, err)
	}
	if e.halted != 1 || e.started != 1 {
		t.Errorf("应各调一次 Halt/Start，实得 halted=%d started=%d", e.halted, e.started)
	}
	if got := decode(t, e.get("/gw/updates/status"))["state"]; got != "applied" {
		t.Errorf("job 状态应为 applied，实得 %v", got)
	}

	// 回滚（.old 是唯一回滚路径，故刻意不删）
	if rr := e.post("/gw/updates/rollback"); rr.Code != 200 {
		t.Fatalf("rollback 失败 %d: %s", rr.Code, rr.Body.String())
	}
	if b, _ := os.ReadFile(e.coreExe); string(b) != "OLD-CORE" {
		t.Fatalf("回滚没恢复旧版: %q", b)
	}
	if got := decode(t, e.get("/gw/updates/status"))["state"]; got != "rolled_back" {
		t.Errorf("job 状态应为 rolled_back，实得 %v", got)
	}
	// 没有 .old 之后再回滚 → 可读拒绝
	_ = os.Remove(e.coreExe + ".old")
	if rr := e.post("/gw/updates/rollback"); rr.Code != http.StatusBadRequest {
		t.Errorf("无 .old 回滚应 400，实得 %d %s", rr.Code, rr.Body.String())
	}
	// 没下载就应用 → 可读拒绝
	e.s.setJob(func(j *updateJob) { *j = updateJob{State: "idle", Message: "空"} })
	if rr := e.post("/gw/updates/apply"); rr.Code != http.StatusBadRequest {
		t.Errorf("空任务应用应 400，实得 %d %s", rr.Code, rr.Body.String())
	}
}

// 清单不可达时必须透出原因，并且有负缓存（面板 3s 轮询不能次次打外网）。
// pkgRoot 向上找包含 go.mod 的目录（仅测试用于定位 dist/）。
func pkgRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		p := filepath.Dir(dir)
		if p == dir {
			break
		}
		dir = p
	}
	return "."
}

// TestUpdateE2EWithMockRegistry 端到端验核心自动更新：本地假装 GitHub release 资产目录 +
// 真实 dist/ww3-core.exe 当作替换目标，走完 download -> apply（rename trick）-> rollback。
// 需要 `bash build.sh` 的产物（dist/wild-work3.exe、dist/ww3-core.exe），故默认 t.Skip。
func TestUpdateE2EWithMockRegistry(t *testing.T) {
	dist := filepath.Join(pkgRoot(), "..", "dist")
	gwExe, coreExePath := filepath.Join(dist, gwExeFile()), filepath.Join(dist, coreExeFile())
	if _, err := os.Stat(gwExe); err != nil {
		t.Skipf("未找到 %s（先跑 bash build.sh）", gwExe)
	}
	if _, err := os.Stat(coreExePath); err != nil {
		t.Skipf("未找到 %s", coreExePath)
	}

	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	// 运行副本：升级会覆盖 core exe，绝不能动 dist/
	runCore := filepath.Join(root, coreExeFile())
	origCore, err := os.ReadFile(coreExePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runCore, origCore, 0o755); err != nil {
		t.Fatal(err)
	}

	// 「新版本」core = 原 exe 尾部追加标记字节：PE 允许尾部 overlay，仍能起来，
	// 而 sha256/大小与旧版不同，够断言「真的换了文件」。
	newCore := append(append([]byte{}, origCore...), []byte("WW3-E2E-NEW-CORE-MARKER")...)

	// 版本由 gateway 经 core /api/health 现取（dist 里构建版本号会变，不能写死）

	g := newGH(t)
	g.put(coreExeFile(), newCore)
	g.put("index.json", []byte(`{"version":1,"plugins":[]}`))
	g.put("update.json", js(t, map[string]any{
		"version": "9.9.9", "notes": "端到端夹具",
		"assets": []map[string]any{{
			"version": "9.9.9", "asset": coreExeFile(), "sha256": shaHex(newCore),
			"size": len(newCore), "os": runtime.GOOS, "arch": runtime.GOARCH,
			"changelog": "- 端到端夹具改动",
		}},
	}))

	port := freePort(t)
	cfg := map[string]any{
		"gateway": map[string]any{
			"listen": fmt.Sprintf("127.0.0.1:%d", port),
			// 清单与包都用绝对 URL：夹具完全本地，不出网
			"registry_repo":    g.srv.URL + "/index.json",
			"main_repo":        g.srv.URL + "/update.json",
			"download_proxies": []string{},
			"dist_hosts":       []string{"127.0.0.1"}, // 否则 host allowlist 会拒掉本地包
		},
	}
	buf, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "config.json"), buf, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, gwExe, "serve", "--data", data, "--no-tray", "--core", runCore)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !waitHTTP(t, base+"/gw/version", 30*time.Second) {
		t.Fatalf("gateway 未就绪：%s", tail(out.String(), 2000))
	}

	// 1) 先等 core 真起来（/core/health 反代通），再谈清单
	if !waitCoreAlive(t, base, 40*time.Second) {
		t.Fatalf("core 未就绪：%s", tail(out.String(), 2500))
	}
	v := getJSON(t, base+"/gw/updates")
	core, _ := v["core"].(map[string]any)
	if core == nil {
		t.Fatalf("响应无 core 段：%s", tail(out.String(), 1500))
	}
	if core["error"] != nil {
		t.Fatalf("读清单失败：%v", core["error"])
	}
	if core["latest"] != "9.9.9" || core["update"] != true {
		t.Fatalf("core 段不对：%#v", core)
	}
	if cur, _ := core["current"].(string); cur == "" {
		t.Fatalf("未取到 core 当前版本：%#v", core)
	}

	// 2) 下载（异步 job）
	if code := postJSON(t, base+"/gw/updates/download", nil); code != 200 {
		t.Fatalf("download 回 %d：%s", code, tail(out.String(), 1500))
	}
	job := waitJobState(t, base, "ready", 60*time.Second)
	if got := int(job["progress"].(float64)); got != 100 {
		t.Fatalf("进度=%v", job["progress"])
	}
	if job["source"] == nil || !strings.Contains(fmt.Sprint(job["source"]), coreExeFile()) {
		t.Fatalf("未记录命中源：%#v", job)
	}

	// 3) 应用：新字节到位、旧字节进 .old、core 真的被重新拉起
	if code := postJSON(t, base+"/gw/updates/apply", nil); code != 200 {
		t.Fatalf("apply 回 %d：%s", code, tail(out.String(), 2000))
	}
	if got, err := os.ReadFile(runCore); err != nil || !bytes.Equal(got, newCore) {
		t.Fatalf("新 core 未落位（err=%v 长度 %d/%d）", err, len(got), len(newCore))
	}
	old, err := os.ReadFile(runCore + ".old")
	if err != nil || !bytes.Equal(old, origCore) {
		t.Fatalf(".old 不是原 core：%v", err)
	}
	if !waitCoreAlive(t, base, 30*time.Second) {
		t.Fatalf("应用后 core 未恢复（gateway.log 尾部：%s）", tail(out.String(), 2000))
	}

	// 4) 回滚
	if code := postJSON(t, base+"/gw/updates/rollback", nil); code != 200 {
		t.Fatalf("rollback 回 %d", code)
	}
	if got, err := os.ReadFile(runCore); err != nil || !bytes.Equal(got, origCore) {
		t.Fatalf("回滚未恢复原 core（err=%v len=%d）", err, len(got))
	}
	if !waitCoreAlive(t, base, 30*time.Second) {
		t.Fatal("回滚后 core 未恢复")
	}
}

// waitCoreAlive 等 core 管理面经 gateway 反代真的可达（升级/回滚后都要重验）。
func waitCoreAlive(t *testing.T, base string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/core/health")
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			var v struct {
				OK      bool              `json:"ok"`
				Version map[string]string `json:"version"`
			}
			if resp.StatusCode == 200 && json.Unmarshal(b, &v) == nil && v.OK && v.Version["core"] != "" {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// ---- HTTP/进程小工具 ----

func gwExeFile() string {
	if runtime.GOOS == "windows" {
		return "wild-work3.exe"
	}
	return "wild-work3"
}

func coreExeFile() string {
	if runtime.GOOS == "windows" {
		return "ww3-core.exe"
	}
	return "ww3-core"
}

// tail 取输出尾部（失败时把 gateway 日志尾部贴进断言，免去翻文件）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// freePort 占一个随机端口后立刻释放（Windows 下 bind 检查必须串行，见上）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitHTTP(t *testing.T, url string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s 回 %d: %s", url, resp.StatusCode, string(b))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("解析 %s: %v (%s)", url, err, string(b))
	}
	return m
}

func postJSON(t *testing.T, url string, body any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	resp, err := http.Post(url, "application/json", rd)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func waitJobState(t *testing.T, base, want string, d time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = getJSON(t, base+"/gw/updates/status")
		if last["state"] == want {
			return last
		}
		if last["state"] == "failed" {
			t.Fatalf("job 失败：%#v", last)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等 job=%s 超时，最后状态 %#v", want, last)
	return nil
}

func TestUpdatesCatalogFailureIsCached(t *testing.T) {
	g := newGH(t) // 不放 update.json → 404
	e := newEnv(t, g)

	out := decode(t, e.get("/gw/updates"))
	core := out["core"].(map[string]any)
	if core["update"] == true {
		t.Errorf("清单取不到却报可更新: %v", core)
	}
	if msg, _ := core["error"].(string); msg == "" {
		t.Errorf("必须把失败原因透出给面板: %v", core)
	}
	n := g.hit("update.json")
	for i := 0; i < 3; i++ {
		decode(t, e.get("/gw/updates"))
	}
	if g.hit("update.json") != n {
		t.Errorf("失败后应有负缓存（否则每次轮询都被拖成一次外网请求）: %d→%d", n, g.hit("update.json"))
	}
	// force 时必须真的重打
	decode(t, e.get("/gw/updates?refresh=1"))
	if g.hit("update.json") <= n {
		t.Errorf("?refresh=1 应绕过缓存")
	}
}

// 版本门不过时不得下载（gateway 太旧要先手动升级）。
func TestMainGateBlocksDownload(t *testing.T) {
	g := newGH(t)
	b := bytes.Repeat([]byte("X"), 512)
	g.put("core/ww3-core.exe", b)
	g.put("update.json", js(t, map[string]any{
		"version": "9.9.9",
		"gateway": map[string]string{"min": "2.0.0"}, // 当前 gateway 1.0.0
		"assets": []map[string]any{{
			"version": "9.9.9", "asset": "ww3-core.exe", "sha256": shaHex(b),
			"size": len(b), "os": runtime.GOOS, "arch": runtime.GOARCH,
			"url": g.srv.URL + "/core/ww3-core.exe",
		}},
	}))
	e := newEnv(t, g)
	rr := e.post("/gw/updates/download")
	if rr.Code == 200 {
		t.Fatalf("版本门没过却允许下载: %s", rr.Body.String())
	}
	if msg, _ := decode(t, rr)["error"].(string); !strings.Contains(msg, "gateway") {
		t.Errorf("错误要指明是 gateway 版本不够: %v", msg)
	}
	if e.halted != 0 {
		t.Errorf("拒下载不该动 core")
	}
}

// waitJob 轮询到指定状态，超时即失败（顺带返回最后一次快照）。
func waitJob(t *testing.T, e *env, want string, to time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(to)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = decode(t, e.get("/gw/updates/status"))
		if last["state"] == want {
			return last
		}
		if last["state"] == "failed" && want != "failed" {
			t.Fatalf("任务失败: %v", last["message"])
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("等不到状态 %s，最后为 %v", want, last)
	return last
}

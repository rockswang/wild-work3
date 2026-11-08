// W3-48 v2 分发链的本地端到端验证：注册表 tarball（平铺注册文件）→ 商店 → 安装
// （版本门 / sha256 / host 白名单 / 发布页定位）→ main.zip 更新（检查 / 下载 / 应用 / 回滚）。
// 全部走 httptest，零真实外网请求。
package gw

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work3/main/gateway/internal/registry"
	"wild-work3/main/gateway/internal/store"
)

// repoSlug 夹具注册表仓（注册文件里的 repo 必须与它一致，否则包地址拼不到夹具上）。
const repoSlug = "wild-work3/ww3-ext-registry"

// tarballKey 单元测试里注册表 tarball 的路径（e2e 走真 codeload 形态，见 proxied）。
const tarballKey = "tarball"

// fixture 假装 GitHub：既发注册表仓 tarball，也发发布页（tags.atom）与 release 资产（支持 Range）。
// 同一份文件同时以「裸路径」和「代理形态 URL」两种键存在，
// 于是单元测试（GitHubBase=夹具）与 e2e（走 download_proxies 路径前缀）都能命中。
//
// **刻意不提供 expanded_assets**：W3-49 起定位只需 repo + version + file，
// 一旦实现里还残留「枚举资产再猜」的代码，这里会 404 立刻暴露。
type fixture struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	files    map[string][]byte
	hits     map[string]int
	reg      map[string]string // 注册文件名 → 内容
	tags     []string          // 最新在前（atom 是时间倒序）
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, files: map[string][]byte{}, hits: map[string]int{},
		reg: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	f.hits[key]++
	body, ok := f.files[key]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, key, time.Unix(1700000000, 0), bytes.NewReader(body))
}

func (f *fixture) hit(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func (f *fixture) put(key string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[key] = b
}

// flushLocked 重建 tags.atom 与注册表 tarball（调用方持锁）。
func (f *fixture) flushLocked() {
	var ta bytes.Buffer
	ta.WriteString(`<?xml version="1.0" encoding="UTF-8"?><feed>`)
	for _, tg := range f.tags {
		fmt.Fprintf(&ta, `<entry><link rel="alternate" type="text/html" href="https://github.com/%s/releases/tag/%s"/></entry>`, repoSlug, tg)
	}
	ta.WriteString(`</feed>`)
	// 两种键：裸路径（单元测试）与代理形态（e2e 的 download_proxies 前缀）
	f.files[repoSlug+"/tags.atom"] = ta.Bytes()
	f.files["https://github.com/"+repoSlug+"/tags.atom"] = ta.Bytes()

	body := tarballOf(f.t, f.reg)
	f.files[tarballKey] = body
	f.files["https://codeload.github.com/"+repoSlug+"/tar.gz/refs/heads/main"] = body
}

// addPlugin 登记一个插件：注册文件 + tag + 资产 + 资产字节（一次性重建发布页与 tarball）。
func (f *fixture) addPlugin(id, typ, version string, pkg []byte, mutate func(reg map[string]any)) {
	tag := "v" + version
	asset := id + "-" + version + ".zip"
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags = append([]string{tag}, f.tags...)
	f.files[repoSlug+"/releases/download/"+tag+"/"+asset] = pkg
	f.files["https://github.com/"+repoSlug+"/releases/download/"+tag+"/"+asset] = pkg
	reg := map[string]any{
		"schema": 2, "id": id, "type": typ, "name": id + " 名称", "repo": repoSlug,
		"versions": []map[string]any{{"version": version, "file": asset, "sha256": shaHex(pkg), "size": len(pkg),
			"protocol": map[string]int{"min": 1, "max": 3}}},
	}
	if mutate != nil {
		mutate(reg)
	}
	b, err := json.Marshal(reg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.reg[id+".json"] = string(b)
	f.flushLocked()
}

// addMain 登记 main.json（主程序条目：core + 官方样板整包）。
func (f *fixture) addMain(version string, pkg []byte, mutate func(reg map[string]any)) {
	f.addPlugin("main", "main", version, pkg, mutate)
}

// tarballOf 打一个注册表仓形状的 tarball（顶层目录 + 平铺 .json）。
func tarballOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "repo-main", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		add("repo-main/"+n, files[n])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
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
	exe := id + "-ext.exe"
	if kind == "hook" {
		exe = id + "-hook.exe"
	}
	e, _ := zw.Create(exe)
	_, _ = e.Write([]byte("FAKE-EXE-" + version))
	_ = zw.Close()
	return buf.Bytes()
}

// mainZip 打 main.zip（core exe + 两个官方样板的 exe/manifest 对）。
func mainZip(t *testing.T, core []byte, bundled bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ww3-core.exe")
	_, _ = w.Write(core)
	if bundled {
		for _, id := range []string{"custom-api-ext", "loopguard-hook"} {
			kind := "channel"
			if strings.HasSuffix(id, "-hook") {
				kind = "hook"
			}
			mj, _ := json.Marshal(map[string]any{"id": strings.TrimSuffix(strings.TrimSuffix(id, "-ext"), "-hook"),
				"kind": kind, "version": "1.0.0", "name": id})
			m, _ := zw.Create(id + ".manifest.json")
			_, _ = m.Write(mj)
			e, _ := zw.Create(id + ".exe")
			_, _ = e.Write([]byte("BUNDLED-" + id))
		}
	}
	_ = zw.Close()
	return buf.Bytes()
}

// fastOpts 测试用竞速判据（生产的 8s/15s 在本地毫无意义）。
func fastOpts(f *fixture) registry.Options {
	return registry.Options{FirstByte: 2 * time.Second, Idle: time.Second, StallWin: 500 * time.Millisecond,
		MinRate: 1024, GitHubBase: f.srv.URL}
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

func newEnv(t *testing.T, f *fixture) *env {
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
			// URL 形态只为离线夹具留口子：生产是 Source{Repo, Ref}
			Registry:    registry.Source{URL: f.srv.URL + "/" + tarballKey, Name: "official"},
			ExtraHosts:  []string{"127.0.0.1"}, // httptest 本机必须在 allowlist 里
			ProtocolVer: 1,
			Fetch:       fastOpts(f),
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
	f := newFixture(t)
	pkg := pluginZip(t, "demo", "channel", "1.2.0")
	f.addPlugin("demo", "channel", "1.2.0", pkg, nil)
	// 再放一个未安装的（商店要能列出没装的）
	f.addPlugin("hooked", "hook", "0.3.0", pluginZip(t, "hooked", "hook", "0.3.0"), nil)

	e := newEnv(t, f)

	list := decodeList(t, e.get("/gw/plugins/store"))
	if len(list) != 2 {
		t.Fatalf("商店应有 2 项: %v", list)
	}
	byID := map[string]map[string]any{}
	for _, it := range list {
		byID[it["plugin_id"].(string)] = it
	}
	it := byID["demo"]
	if it == nil || it["latest"] != "1.2.0" || it["kind"] != "channel" {
		t.Fatalf("条目形状不对: %v", it)
	}
	if it["installed"] != false || it["installable"] != true {
		t.Errorf("未安装/可安装标记不对: %v", it)
	}
	if _, has := it["catalog_signed"]; has {
		t.Errorf("v2 不验签，不该再有 catalog_signed: %v", it)
	}
	if src, _ := it["catalog_source"].(string); src == "" {
		t.Errorf("必须透出注册表来源（面板要显示「从哪拿的」）: %v", it)
	}
	if it["catalog_updated_at"] == "" {
		t.Errorf("必须透出快照时间: %v", it)
	}
	if hk := byID["hooked"]; hk == nil || hk["kind"] != "hook" {
		t.Errorf("hook 类型应从注册文件 type 透出: %v", hk)
	}

	// 安装
	rr := e.body("/gw/plugins/install", `{"plugin_id":"demo"}`)
	if rr.Code != 200 {
		t.Fatalf("安装失败 %d: %s", rr.Code, rr.Body.String())
	}
	got := decode(t, rr)
	if got["ok"] != true || got["version"] != "1.2.0" {
		t.Fatalf("安装响应不对: %v", got)
	}
	if !strings.Contains(got["source"].(string), "demo-1.2.0.zip") {
		t.Errorf("应回命中源（发布页定位出的资产）: %v", got["source"])
	}
	ver := filepath.Join(e.root, "plugins", "demo", "1.2.0")
	if b, err := os.ReadFile(filepath.Join(ver, "manifest.json")); err != nil || !bytes.Contains(b, []byte("demo")) {
		t.Fatalf("包未落地: %v %s", err, b)
	}
	if cur, err := os.ReadFile(filepath.Join(e.root, "plugins", "demo", "current")); err != nil || strings.TrimSpace(string(cur)) != "1.2.0" {
		t.Fatalf("current 指针没切: %q %v", cur, err)
	}

	// /gw/plugins 的 latest 只读缓存，不得再打外网
	before := f.hit(tarballKey)
	rr = e.get("/gw/plugins")
	if !strings.Contains(rr.Body.String(), `"latest":"1.2.0"`) {
		t.Errorf("已安装列表应带注册表最新号: %s", rr.Body.String())
	}
	if f.hit(tarballKey) != before {
		t.Errorf("/gw/plugins 不该重拉注册表（应只用缓存）")
	}

	// 手动刷新注册表：必须真的再打一次
	if rr := e.post("/gw/registry/refresh"); rr.Code != 200 {
		t.Fatalf("refresh 失败: %d %s", rr.Code, rr.Body.String())
	}
	if f.hit(tarballKey) <= before {
		t.Errorf("refresh 后应重拉注册表")
	}

	// 装一个不存在的插件 → 404 且文案可读
	if rr := e.body("/gw/plugins/install", `{"plugin_id":"nope"}`); rr.Code != http.StatusNotFound {
		t.Errorf("不存在的插件应 404，实得 %d %s", rr.Code, rr.Body.String())
	}
}

// 三道硬门：sha 不符 / 版本门不过 / 主机不在白名单；外加「版本不在最近 tag 内」的定位失败。
func TestInstallHardGates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(reg map[string]any)
		tweak  func(e *env)
		prep   func(f *fixture, pkg []byte)
		want   string
	}{
		{name: "sha 不符", mutate: func(reg map[string]any) {
			reg["versions"] = []map[string]any{{"version": "1.0.0", "file": "gate-1.0.0.zip", "sha256": strings.Repeat("ab", 32)}}
		}, want: "SHA256 mismatch"},
		{name: "协议版本门", mutate: func(reg map[string]any) {
			reg["versions"] = []map[string]any{{"version": "1.0.0", "file": "gate-1.0.0.zip",
				"sha256": strings.Repeat("cd", 32), "protocol": map[string]int{"min": 9}}}
		}, want: "协议"},
		{name: "主机白名单", tweak: func(e *env) { e.s.Dist.ExtraHosts = nil }, want: "允许列表"},
		{name: "版本不在最近 tag 内", prep: func(f *fixture, pkg []byte) {
			// 注册文件说 2.0.0，但发布页只有 v1.0.0
			f.mu.Lock()
			f.reg["gate.json"] = fmt.Sprintf(`{"schema":2,"id":"gate","type":"channel","name":"g","repo":"%s",
				"versions":[{"version":"2.0.0","file":"gate-2.0.0.zip","sha256":"%s"}]}`, repoSlug, shaHex(pkg))
			f.flushLocked()
			f.mu.Unlock()
		}, want: "无法自动定位发布包"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			pkg := pluginZip(t, "gate", "channel", "1.0.0")
			if c.prep != nil {
				f.addPlugin("gate", "channel", "1.0.0", pkg, nil)
				c.prep(f, pkg)
			} else {
				f.addPlugin("gate", "channel", "1.0.0", pkg, c.mutate)
			}
			e := newEnv(t, f)
			if c.tweak != nil {
				c.tweak(e)
			}
			rr := e.body("/gw/plugins/install", `{"plugin_id":"gate"}`)
			if rr.Code == 200 {
				t.Fatalf("本该拒装却成功了: %s", rr.Body.String())
			}
			msg, _ := decode(t, rr)["error"].(string)
			if !strings.Contains(msg, c.want) {
				t.Errorf("错误文案缺 %q: %v", c.want, msg)
			}
			// 拒装不得留下 current 指针（装了再说 = 半成品砖）
			if _, err := os.Stat(filepath.Join(e.root, "plugins", "gate", "current")); err == nil {
				t.Errorf("拒装后不该有 current")
			}
		})
	}
}

// 注册表里的坏文件必须经 /gw/plugins/store/errors 透出（§1.2 不许静默吞掉）。
// DrainBody 必须把 handler 没读完的请求体丢掉：否则 net/http 会在回包后直接关连接，
// 客户端表现为 ConnectionResetError 10054（实测 /gw/registry/refresh 连发 8 次挂 5~6 次）。
func TestDrainBodyConsumesUnread(t *testing.T) {
	h := DrainBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 故意不读 body，与 /gw/registry/refresh 同形
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/gw/registry/refresh",
		strings.NewReader(`{"pad":"`+strings.Repeat("x", 4096)+`"}`))
	h.ServeHTTP(rr, r)
	b, _ := io.ReadAll(r.Body)
	if len(b) != 0 {
		t.Errorf("请求体应已被排空，实际还剩 %d 字节", len(b))
	}
	if rr.Code != http.StatusOK {
		t.Errorf("状态码应为 200，实际 %d", rr.Code)
	}
}

func TestStoreErrorsExposed(t *testing.T) {
	f := newFixture(t)
	f.addPlugin("demo", "channel", "1.0.0", pluginZip(t, "demo", "channel", "1.0.0"), nil)
	f.mu.Lock()
	f.reg["broken.json"] = `{`
	f.flushLocked()
	f.mu.Unlock()

	e := newEnv(t, f)
	if list := decodeList(t, e.get("/gw/plugins/store")); len(list) != 1 {
		t.Fatalf("坏文件不该拖垮整份清单: %v", list)
	}
	errs := decodeList(t, e.get("/gw/plugins/store/errors"))
	if len(errs) != 1 || errs[0]["file"] != "broken.json" {
		t.Fatalf("坏文件必须透出: %v", errs)
	}
	if msg, _ := errs[0]["reason"].(string); msg == "" {
		t.Errorf("要带可读原因: %v", errs[0])
	}
}

// ---- main.zip 更新 ----

func TestUpdatesDownloadApplyRollback(t *testing.T) {
	f := newFixture(t)
	newCore := bytes.Repeat([]byte("NEW-CORE-"), 4096) // ~36KB，够跑进度回调
	pkg := mainZip(t, newCore, true)
	f.addMain("1.1.0", pkg, func(reg map[string]any) {
		reg["gateway"] = map[string]string{"version": "1.1.0", "sha256": strings.Repeat("ee", 32), "note": "整包替换"}
		reg["versions"] = []map[string]any{{"version": "1.1.0", "file": "main-1.1.0.zip", "sha256": shaHex(pkg), "size": len(pkg),
			"os": runtime.GOOS, "arch": runtime.GOARCH, "changelog": "本轮改动",
			"gateway": map[string]string{"min": "1.0.0", "max": "2.0.0"}}}
	})

	e := newEnv(t, f)

	out := decode(t, e.get("/gw/updates"))
	gwv := out["gateway"].(map[string]any)
	if gwv["update"] != false || gwv["manual"] != true {
		t.Errorf("gateway 分支必须恒报手动升级: %v", gwv)
	}
	if gwv["latest"] != "1.1.0" {
		t.Errorf("gateway 该展示注册表声明的最新版（只展示不替换）: %v", gwv)
	}
	note, _ := gwv["note"].(string)
	if !strings.Contains(note, "wild-work3.exe") {
		t.Errorf("note 要写清换哪个文件: %q", note)
	}
	if rel, _ := gwv["release"].(string); rel == "" {
		t.Errorf("必须给出可点开的 release 位置（手动升级要用）: %q", rel)
	}
	core := out["core"].(map[string]any)
	if core["update"] != true || core["latest"] != "1.1.0" || core["current"] != "" {
		t.Fatalf("main 应报可升级: %v", core)
	}
	if core["changelog"] != "本轮改动" || core["asset"] != "main.zip" {
		t.Errorf("changelog/asset 没透出: %v", core)
	}
	if v, ok := core["version_gate"].(map[string]any); !ok || v["min"] != "1.0.0" {
		t.Errorf("版本门没透出: %v", core["version_gate"])
	}
	if out["catalog"] == nil {
		t.Errorf("应带 catalog 状态（注册表从哪拿的、多旧）")
	}

	// 下载：立即返回 + 轮询到 ready
	if rr := e.post("/gw/updates/download"); rr.Code != 200 {
		t.Fatalf("download 失败 %d: %s", rr.Code, rr.Body.String())
	}
	st := waitJob(t, e, "ready", 10*time.Second)
	if p, _ := st["progress"].(float64); int(p) != 100 {
		t.Errorf("进度应到 100: %v", st)
	}
	if b, _ := st["downloaded_bytes"].(float64); int64(b) != int64(len(pkg)) {
		t.Errorf("字节数应等于包长: %v", st["downloaded_bytes"])
	}
	if !strings.Contains(st["source"].(string), "main-1.1.0.zip") {
		t.Errorf("应记命中源: %v", st["source"])
	}
	// 幂等：已在 ready 再点下载，重新起一轮而不是报错
	if rr := e.post("/gw/updates/download"); rr.Code != 200 {
		t.Errorf("重复 download 应幂等: %d %s", rr.Code, rr.Body.String())
	}
	waitJob(t, e, "ready", 10*time.Second)

	// 应用：core 替换 + 包内官方样板按 manifest 落版本目录
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
	for _, id := range []string{"custom-api", "loopguard"} {
		cur, err := os.ReadFile(filepath.Join(e.root, "plugins", id, "current"))
		if err != nil || strings.TrimSpace(string(cur)) != "1.0.0" {
			t.Fatalf("包内插件 %s 未安装: %q %v", id, cur, err)
		}
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

// 注册表不可达时必须透出原因，并且有负缓存（面板 3s 轮询不能次次打外网）。
func TestUpdatesCatalogFailureIsCached(t *testing.T) {
	f := newFixture(t) // 不放 tarball → 404
	e := newEnv(t, f)

	out := decode(t, e.get("/gw/updates"))
	core := out["core"].(map[string]any)
	if core["update"] == true {
		t.Errorf("注册表取不到却报可更新: %v", core)
	}
	if msg, _ := core["error"].(string); msg == "" {
		t.Errorf("必须把失败原因透出给面板: %v", core)
	}
	n := f.hit(tarballKey)
	for i := 0; i < 3; i++ {
		decode(t, e.get("/gw/updates"))
	}
	if f.hit(tarballKey) != n {
		t.Errorf("失败后应有负缓存（否则每次轮询都被拖成一次外网请求）: %d→%d", n, f.hit(tarballKey))
	}
	// force 时必须真的重打
	decode(t, e.get("/gw/updates?refresh=1"))
	if f.hit(tarballKey) <= n {
		t.Errorf("?refresh=1 应绕过缓存")
	}
}

// 版本门不过时不得下载（gateway 太旧要先手动升级）。
func TestMainGateBlocksDownload(t *testing.T) {
	f := newFixture(t)
	core := bytes.Repeat([]byte("X"), 512)
	pkg := mainZip(t, core, false)
	f.addMain("9.9.9", pkg, func(reg map[string]any) {
		reg["versions"] = []map[string]any{{"version": "9.9.9", "file": "main-9.9.9.zip", "sha256": shaHex(pkg), "size": len(pkg),
			"os": runtime.GOOS, "arch": runtime.GOARCH, "gateway": map[string]string{"min": "2.0.0"}}}
	})
	e := newEnv(t, f)
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

// main.json 里没有当前平台的包时必须可读拒绝（不能拿别的平台的包去覆盖 core）。
func TestMainMissingPlatform(t *testing.T) {
	f := newFixture(t)
	core := bytes.Repeat([]byte("X"), 64)
	pkg := mainZip(t, core, false)
	f.addMain("1.1.0", pkg, func(reg map[string]any) {
		reg["versions"] = []map[string]any{{"version": "1.1.0", "file": "main-1.1.0.zip", "sha256": shaHex(pkg), "os": "plan9"}}
	})
	e := newEnv(t, f)
	out := decode(t, e.get("/gw/updates"))
	if msg, _ := out["core"].(map[string]any)["error"].(string); !strings.Contains(msg, "当前平台") {
		t.Fatalf("应说明没有当前平台的包: %v", out["main"])
	}
	if rr := e.post("/gw/updates/download"); rr.Code != http.StatusBadRequest {
		t.Errorf("无当前平台包时下载应 400，实得 %d", rr.Code)
	}
}

// ---- 真二进制端到端（需要 bash build.sh 产物，故默认跳过） ----

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

// TestUpdateE2EWithMockRegistry 端到端验核心自动更新：本地假 GitHub（注册表 tarball +
// 发布页 + 资产）+ 真实 dist/ww3-core.exe 当作替换目标，走完 download → apply（rename trick）→ rollback。
//
// 配置里 `registry_repo` 用 `owner/repo` 形态，`download_proxies` 指向夹具——
// 夹具按 gh-proxy 的路径前缀形态（`/<原 URL>`）发文件，于是整条链（codeload 枚举 →
// 发布页定位 → 资产下载）都命中夹具，且**顺带验证了代理前缀拼接**。
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

	f := newFixture(t)
	pkg := mainZip(t, newCore, false)
	f.addMain("9.9.9", pkg, func(reg map[string]any) {
		reg["versions"] = []map[string]any{{"version": "9.9.9", "file": "main-9.9.9.zip", "sha256": shaHex(pkg), "size": len(pkg),
			"os": runtime.GOOS, "arch": runtime.GOARCH, "changelog": "- 端到端夹具改动"}}
	})

	port := freePort(t)
	cfg := map[string]any{
		"gateway": map[string]any{
			"listen": fmt.Sprintf("127.0.0.1:%d", port),
			// repo 形态 + 夹具当代理：完全不碰真实 github.com
			"registry_repo":    repoSlug,
			"download_proxies": []string{f.srv.URL + "/"},
		},
	}
	if err := os.WriteFile(filepath.Join(data, "config.json"), js(t, cfg), 0o600); err != nil {
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

	// 1) 先等 core 真起来（/core/health 反代通），再谈注册表
	if !waitCoreAlive(t, base, 40*time.Second) {
		t.Fatalf("core 未就绪：%s", tail(out.String(), 2500))
	}
	v := getJSON(t, base+"/gw/updates")
	core, _ := v["core"].(map[string]any)
	if core == nil {
		t.Fatalf("响应无 core 段：%s", tail(out.String(), 1500))
	}
	if core["error"] != nil {
		t.Fatalf("读注册表失败：%v", core["error"])
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
	if job["source"] == nil || !strings.Contains(fmt.Sprint(job["source"]), "main-9.9.9.zip") {
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
		t.Fatalf("应用后 core 未恢复（日志尾部：%s）", tail(out.String(), 2000))
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

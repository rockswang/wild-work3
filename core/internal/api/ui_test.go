package api

// ui_test.go 守住 W3-37 定案 1 的搬迁结果：Web UI 资产必须真的嵌进 core 二进制并能取到。
//
// 这条测试的价值在于「embed 静默为空」是编译器不会报的错——目录改名/漏提交时，
// 面板会变成一个永远 404 的空白页，而管理 API 全部正常，很难一眼看出。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIEmbeddedAndServe(t *testing.T) {
	h := uiHandler()
	for _, tc := range []struct{ path, wantContains string }{
		{"/", "wild-work3"},
		{"/app.js", "async function"},
		{"/style.css", ":"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s = %d，应为 200（embed 是否漏了 web 目录？）", tc.path, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, tc.wantContains) {
			t.Fatalf("%s 内容不含 %q（前 80 字节：%q）", tc.path, tc.wantContains, body[:min(80, len(body))])
		}
	}
}

// TestUIRoutesDontShadowAPI 确认 "/" 没把 /api/* 吃掉（ServeMux 的具体模式优先，但改挂载顺序时易翻车）。
func TestUIRoutesDontShadowAPI(t *testing.T) {
	s := &Server{Ver: map[string]string{"core": "test"}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("/api/health = %d %s，应命中 health 而不是 UI", rec.Code, rec.Body.String())
	}
}

// TestUIWrongPortHint 守住「浏览器直连 core 端口」的防呆（真踩过：面板能显示但所有 API 404）。
//
// 判据：带 gateway 反代头 → 正常返回 index.html；不带且已知 gateway 地址 → 返回提示页（不含 app.js 引用）；
// 不带但 gateway 地址未知（core 单跑）→ 仍正常返回，不阻断调试。
func TestUIWrongPortHint(t *testing.T) {
	h := uiHandler()
	get := func(path string, hdr map[string]string) (int, string) {
		r := httptest.NewRequest("GET", path, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}

	// 未注入 gateway 地址：退化放行（否则 `ww3-core serve` 单独调试时连面板都看不到）。
	if code, body := get("/", nil); code != 200 || !strings.Contains(body, "wild-work3") {
		t.Fatalf("未知 gateway 地址时应正常返回面板，实得 %d", code)
	}

	SetGatewayListen("127.0.0.1:7863")
	defer SetGatewayListen("")

	code, body := get("/", nil)
	if code != 200 || !strings.Contains(body, "你打开的是 core 的内部端口") {
		t.Fatalf("直连 core 端口应返回错端口提示，实得 %d %q", code, body[:min(120, len(body))])
	}
	if !strings.Contains(body, "http://127.0.0.1:7863/") {
		t.Fatalf("提示页未给出 gateway 地址：%q", body)
	}

	// 经 gateway 反代进来：必须照旧给面板（提示页不引用静态资产，误拦会直接白屏）。
	if code, body := get("/", map[string]string{upstreamHdr: "core-ui"}); code != 200 || !strings.Contains(body, "wild-work3") {
		t.Fatalf("带 X-WW3-Upstream 时应正常返回面板，实得 %d", code)
	}
	// 静态资产不受防呆影响（否则连 core 单跑时的 /app.js 都取不到）。
	if code, body := get("/app.js", nil); code != 200 || !strings.Contains(body, "async function") {
		t.Fatalf("/app.js 应照旧返回，实得 %d", code)
	}
}

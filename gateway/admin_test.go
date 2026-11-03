package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestGWConfig(pass string) *gwConfig {
	g := &gwConfig{c: gatewayConfig{Listen: "127.0.0.1:7863", AdminPass: pass}}
	return g
}

// 5 分钟内连续失败 5 次即锁；成功后清零（旧版 loginMaxFail=5 / loginFailWindow=5min）。
func TestLoginThrottle(t *testing.T) {
	loginLimiter = &loginGuard{recs: map[string]*failRec{}}
	h := gwLogin(newTestGWConfig("s3cret"))

	call := func(pass string) int {
		r := httptest.NewRequest(http.MethodPost, "/gw/login", strings.NewReader(`{"password":"`+pass+`"}`))
		r.RemoteAddr = "127.0.0.1:5555"
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	for i := 0; i < loginMaxFail; i++ {
		if c := call("wrong"); c != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错口令应 401，得到 %d", i+1, c)
		}
	}
	if c := call("wrong"); c != http.StatusTooManyRequests {
		t.Fatalf("第 6 次应被锁定 429，得到 %d", c)
	}
	// 锁定期内即使口令正确也拒绝
	if c := call("s3cret"); c != http.StatusTooManyRequests {
		t.Fatalf("锁定期间正确口令也应 429，得到 %d", c)
	}
}

// 成功登录必须清零失败计数，否则偶发输错会累积成锁定。
func TestLoginResetOnSuccess(t *testing.T) {
	loginLimiter = &loginGuard{recs: map[string]*failRec{}}
	h := gwLogin(newTestGWConfig("s3cret"))
	call := func(pass string) int {
		r := httptest.NewRequest(http.MethodPost, "/gw/login", strings.NewReader(`{"password":"`+pass+`"}`))
		r.RemoteAddr = "127.0.0.1:5556"
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	for i := 0; i < loginMaxFail-1; i++ {
		call("wrong")
	}
	if c := call("s3cret"); c != http.StatusOK {
		t.Fatalf("正确口令应 200，得到 %d", c)
	}
	// 清零后又能错 loginMaxFail 次
	for i := 0; i < loginMaxFail; i++ {
		if c := call("wrong"); c != http.StatusUnauthorized {
			t.Fatalf("清零后第 %d 次应 401，得到 %d", i+1, c)
		}
	}
}

// 窗口过期后自动解锁（不能永久锁死）。
func TestLoginWindowExpiry(t *testing.T) {
	g := &loginGuard{recs: map[string]*failRec{}}
	ip := "127.0.0.1"
	for i := 0; i < loginMaxFail; i++ {
		g.fail(ip)
	}
	if _, ok := g.blocked(ip); !ok {
		t.Fatal("应处于锁定状态")
	}
	// 手工把首次失败时间推到窗口之外
	g.mu.Lock()
	g.recs[ip].first = time.Now().Add(-loginFailWindow - time.Second)
	g.mu.Unlock()
	if _, ok := g.blocked(ip); ok {
		t.Fatal("窗口已过应自动解锁")
	}
}

// 无口令时登录直接放行（本机默认无口令）。
func TestLoginNoPassword(t *testing.T) {
	loginLimiter = &loginGuard{recs: map[string]*failRec{}}
	h := gwLogin(newTestGWConfig(""))
	r := httptest.NewRequest(http.MethodPost, "/gw/login", strings.NewReader(`{"password":"anything"}`))
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("无口令应放行，得到 %d", w.Code)
	}
}

package main

// gateway 管理面「安全与生命周期」相关端点（W3-29 §1/§8）：
//
//	GET  /gw/session  会话状态（是否必须登录）
//	POST /gw/login    管理口令登录 → 置 HttpOnly cookie
//	POST /gw/logout   退出
//	GET  /gw/state    面板状态（监听地址/模型 Key 脱敏/自启）
//	PUT  /gw/config   保存监听/API Key/管理口令/开机自启
//
// 归属依据：管理鉴权统一收口在 gateway（W3-15），core 不持有面板口令；
// API-Key 仅用于 /v1/* 鉴权（W3-02/§8）。业务策略配置在 core 的 core.json。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"wild-work3/main/appconfig"
)

// gwConfig gateway 配置（可热更新 + 原子落盘）。
//
// 不再单独落 gateway.json，而是写进全项目唯一配置文件 <data>/config.json 的 gateway 段
// （见 main/appconfig）；gateway 与 core 各写自己那一段，跨进程文件锁内读-改-写。
type gwConfig struct {
	mu   sync.RWMutex
	file *appconfig.File
	c    gatewayConfig
}

// defaultGWListen 定义在 main.go（coreAddrFor 旁）。

// newGWConfig 打开 gateway 段配置存储。
//
// W3-37 定案 4：ww3 未发布过任何版本，除 auths/ 凭据格式外取消向前兼容义务，
// 所以不再有旧 gateway.json / core/core.json 的迁移逻辑（appconfig.Migrate 已删）。
func newGWConfig(dataDir string) *gwConfig {
	f := appconfig.OpenData(dataDir)
	return &gwConfig{file: f, c: loadGatewaySection(f)}
}

// loadGatewaySection 读 gateway 段（缺省段返回默认值 + 环境变量覆盖）。
func loadGatewaySection(f *appconfig.File) gatewayConfig {
	var c gatewayConfig
	if b, err := f.Section(appconfig.SectionGateway); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &c)
	}
	// 环境变量优先（容器/服务场景无法手改配置）：
	//   WILDWORK_ADMIN_PASSWORD  面板管理口令
	//   WILDWORK_API_KEY         /v1/* 鉴权 Key
	// 旧版 dev 同名变量，便于既有部署直接搬过来；显式置空则不覆盖配置文件值。
	if v := os.Getenv("WILDWORK_ADMIN_PASSWORD"); v != "" {
		c.AdminPass = v
	}
	if v := os.Getenv("WILDWORK_API_KEY"); v != "" {
		c.APIKey = v
	}
	return c
}

func (g *gwConfig) Get() gatewayConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.c
}

// File 暴露底层统一配置文件（规则段/其它段由调用方各自读写）。
func (g *gwConfig) File() *appconfig.File { return g.file }

func (g *gwConfig) Update(fn func(*gatewayConfig)) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn(&g.c)
	b, err := json.Marshal(g.c)
	if err != nil {
		return err
	}
	return g.file.Update(appconfig.SectionGateway, func(json.RawMessage) (json.RawMessage, error) {
		return b, nil
	})
}

// adminToken 由管理口令派生的会话令牌：cookie 不携带明文口令。
func adminToken(pass string) string {
	sum := sha256.Sum256([]byte("ww3-admin:" + pass))
	return hex.EncodeToString(sum[:16])
}

// adminExempt 免鉴权端点：登录/退出/会话查询必须可达，否则无法登录。
var adminExempt = map[string]bool{
	"/gw/login": true, "/gw/logout": true, "/gw/session": true,
}

// adminAuth 管理鉴权（只作用于 /core/*、/gw/*、/ext/*；/v1/* 走模型鉴权）。
// 口令为空则放行（本机默认无口令）。
func adminAuth(passFn func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pass := passFn()
		if pass == "" {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		need := strings.HasPrefix(p, "/core/") || strings.HasPrefix(p, "/gw/") || strings.HasPrefix(p, "/ext/")
		if !need || adminExempt[p] {
			next.ServeHTTP(w, r)
			return
		}
		if authed(r, pass) {
			next.ServeHTTP(w, r)
			return
		}
		jsonErr(w, http.StatusUnauthorized, "需要管理口令")
	})
}

// authed 判定请求是否已通过管理鉴权（cookie 令牌或 CLI 头）。
func authed(r *http.Request, pass string) bool {
	tok := adminToken(pass)
	if c, err := r.Cookie("ww3_admin"); err == nil && (c.Value == tok || c.Value == pass) {
		return true
	}
	if v := r.Header.Get("X-WW3-Admin"); v == pass || v == tok {
		return true
	}
	return false
}

func gwSession(cfg *gwConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pass := cfg.Get().AdminPass
		jsonOut(w, map[string]any{
			"ok":            true,
			"required":      pass != "",
			"authenticated": pass == "" || authed(r, pass),
		})
	}
}

func gwLogin(cfg *gwConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "方法不允许")
			return
		}
		// 登录失败节流（旧版 dev/internal/app/session.go:31 loginMaxFail=5 / loginFailWindow=5min）：
		// 面板监听 127.0.0.1，但本机其他进程/浏览器扩展仍可暴力猜口令，故保留锁。
		ip := clientIP(r)
		if wait, ok := loginLimiter.blocked(ip); ok {
			jsonErr(w, http.StatusTooManyRequests,
				fmt.Sprintf("登录失败次数过多，请 %d 秒后再试", int(wait.Seconds())+1))
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pass := cfg.Get().AdminPass
		if pass != "" && body.Password != pass {
			loginLimiter.fail(ip)
			jsonErr(w, http.StatusUnauthorized, "管理口令错误")
			return
		}
		loginLimiter.reset(ip)
		http.SetCookie(w, &http.Cookie{
			Name: "ww3_admin", Value: adminToken(pass), Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
		})
		jsonOut(w, map[string]any{"ok": true})
	}
}

// ---- 登录失败节流（按客户端 IP）----
// 口径对齐旧版：5 分钟内连续失败 5 次即锁到窗口结束（滑动窗口，成功登录清零）。
const (
	loginMaxFail    = 5
	loginFailWindow = 5 * time.Minute
)

type failRec struct {
	count int
	first time.Time
}

type loginGuard struct {
	mu   sync.Mutex
	recs map[string]*failRec
}

var loginLimiter = &loginGuard{recs: map[string]*failRec{}}

// blocked 返回是否仍处于锁定状态与剩余等待时长。
func (g *loginGuard) blocked(ip string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.recs[ip]
	if r == nil || r.count < loginMaxFail {
		return 0, false
	}
	left := loginFailWindow - time.Since(r.first)
	if left <= 0 { // 窗口已过，重新计数
		delete(g.recs, ip)
		return 0, false
	}
	return left, true
}

func (g *loginGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.recs[ip]
	if r == nil || time.Since(r.first) > loginFailWindow {
		g.recs[ip] = &failRec{count: 1, first: time.Now()}
		return
	}
	r.count++
}

func (g *loginGuard) reset(ip string) {
	g.mu.Lock()
	delete(g.recs, ip)
	g.mu.Unlock()
}

// clientIP 取客户端 IP（gateway 只监听回环，不做 X-Forwarded-For 信任）。
func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func gwLogout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ww3_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		jsonOut(w, map[string]any{"ok": true})
	}
}

// gwStateView gateway 侧面板状态（脱敏）。listen = 当前实际监听（生效值）；
// listen_config = 已保存值（改了监听地址后需重启才生效，故两者可能不同）。
func gwStateView(cfg *gwConfig, listen string) map[string]any {
	c := cfg.Get()
	lc := c.Listen
	if lc == "" {
		lc = listen
	}
	return map[string]any{
		"ok":                 true,
		"listen":             listen,
		"listen_config":      lc,
		"api_key":            maskSecret(c.APIKey),
		"admin_password":     maskSecret(c.AdminPass),
		"admin_password_set": c.AdminPass != "",
		"autostart":          autostartEnabled(),
	}
}

func gwStateHandler(cfg *gwConfig, listen string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { jsonOut(w, gwStateView(cfg, listen)) }
}

// gwConfigHandler 保存 gateway 配置；脱敏值（以 *** 开头）视作「未改动」。
//
// 保存成功后调 onSaved（若设置）以热重载派生状态（如规则引擎）。
func gwConfigHandler(cfg *gwConfig, onSaved func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			jsonOut(w, gwStateView(cfg, cfg.Get().Listen))
			return
		case http.MethodPut:
			var body struct {
				Listen        *string `json:"listen"`
				APIKey        *string `json:"api_key"`
				AdminPassword *string `json:"admin_password"`
				Autostart     *bool   `json:"autostart"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			msgParts := []string{}
			if err := cfg.Update(func(c *gatewayConfig) {
				if body.Listen != nil && *body.Listen != "" {
					c.Listen = *body.Listen
					msgParts = append(msgParts, "监听地址")
				}
				if body.APIKey != nil && !strings.HasPrefix(*body.APIKey, "***") {
					c.APIKey = *body.APIKey
					msgParts = append(msgParts, "API Key")
				}
				if body.AdminPassword != nil && !strings.HasPrefix(*body.AdminPassword, "***") {
					c.AdminPass = *body.AdminPassword
					msgParts = append(msgParts, "管理口令")
				}
			}); err != nil {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if body.Autostart != nil {
				if err := autostartSet(*body.Autostart); err != nil {
					jsonErr(w, http.StatusInternalServerError, "开机自启设置失败: "+err.Error())
					return
				}
				msgParts = append(msgParts, map[bool]string{true: "已开启自启", false: "已关闭自启"}[*body.Autostart])
			}
			msg := "已保存"
			if len(msgParts) > 0 {
				msg = "已保存：" + strings.Join(msgParts, "、")
			}
			if onSaved != nil {
				onSaved()
			}
			jsonOut(w, map[string]any{"ok": true, "message": msg})
			return
		default:
			jsonErr(w, http.StatusMethodNotAllowed, "方法不允许")
		}
	}
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "***"
	}
	return "***" + s[len(s)-4:]
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

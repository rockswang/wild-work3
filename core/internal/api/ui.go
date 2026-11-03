// Web UI 静态资产（W3-37 定案 1：页面归 core，gateway 只把未命中的路径透传过来）。
//
// 为什么在 core 而不是 gateway：UI 是全项目改动最频繁的交付物（渠道卡片/向导/规则编辑器/主题/
// 统计图表），而 gateway 发布后按「全包手动升级」收敛、不应因前端改动发版；core 本来就走自动更新。
// 浏览器的 origin 仍是 gateway，所以 app.js 里的 fetch 前缀（/core/*、/gw/*、/ext/*）不变。
package api

import (
	"embed"
	"fmt"
	"html"
	"io/fs"
	"net"
	"net/http"
	"sync"
)

//go:embed all:web
var webFS embed.FS

// upstreamHdr 是 gateway 反代时注入的来源标记（见 main/gateway/internal/proxy/proxy.go）。
// core 只依赖这一个约定：带该头 = 经 gateway 进来；浏览器跨域发不出该头，所以直接访问
// core 端口必然不带它（见下面的错端口防呆）。
const upstreamHdr = "X-WW3-Upstream"

// gateway 对外监听地址（gateway 通过 --gw-listen 注入）。
// 空串 = 未知（core 单跑 / CLI 自驱动），此时不做错端口提示。
var (
	gwMu     sync.RWMutex
	gwListen string
)

// SetGatewayListen 记录 gateway 的对外监听地址，仅用于「打开错端口」提示。
func SetGatewayListen(addr string) {
	gwMu.Lock()
	gwListen = addr
	gwMu.Unlock()
}

func gatewayListen() string {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return gwListen
}

// gatewayURL 把监听地址算成浏览器可点的 URL（0.0.0.0/::/空 host → 127.0.0.1）。
func gatewayURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// uiHandler 提供 Web UI：/ 返回 index.html，其余按路径取静态文件。
//
// 路径由 http.FileServer 在 embed.FS 内自己解析（不可能越到磁盘目录），未命中就是 404；
// /api/* 在 ServeMux 上是更具体的模式，不会被这里的 "/" 吞掉。
func uiHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		// embed 缺失属于构建错误，宁可 500 也不静默返回空面板。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Web UI 资产未嵌入: "+err.Error(), http.StatusInternalServerError)
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 错端口防呆（真踩过）：浏览器直接打开 core 端口时，静态资产照样取得到、面板完整渲染，
		// 但 /gw/* 与 /core/* 在 core 上全是 404 → 前端每条请求都「请求失败」，看着像后端全挂。
		// 这里在入口就把「走错端口」讲清楚，而不是让人去猜。
		// 只拦入口页：/app.js、/style.css 等照旧放行（提示页不引用它们）。
		if p := r.URL.Path; (p == "/" || p == "/index.html") && r.Header.Get(upstreamHdr) == "" {
			if gw := gatewayURL(gatewayListen()); gw != "" {
				writeWrongPort(w, r, gw)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

// wrongPortHTML 错端口提示页（自包含，不引用任何静态资产——此时面板本来就用不了）。
const wrongPortHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>端口不对 — wild-work3</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
background:#16181d;color:#e6e8ec;font:14px/1.7 "Segoe UI","Microsoft YaHei",sans-serif}
.box{max-width:620px;padding:28px 32px;background:#1e2128;border:1px solid #2c313b;border-radius:10px}
h1{margin:0 0 12px;font-size:19px;color:#faad14}
code{background:#11131a;padding:1px 6px;border-radius:4px;color:#7ec8ff}
a{color:#7ec8ff}
p{margin:10px 0}
.tip{color:#8b93a1;font-size:13px}
</style></head>
<body><div class="box">
<h1>⚠ 你打开的是 core 的内部端口</h1>
<p>当前地址 <code>%s</code> 是 <b>ww3-core</b> 的管理面端口，它只提供 <code>/api/*</code>；
面板需要的 <code>/gw/*</code> 与 <code>/core/*</code> 由 <b>gateway</b> 统一对外。</p>
<p>请改用：<a href="%s">%s</a></p>
<p class="tip">core 的 HTTP 只监听本机回环、只供 gateway 反代；正常使用请从托盘「打开主界面」或上面的地址进入。
如果你是单独跑 <code>ww3-core serve</code> 调试，需要面板请同时启动 <code>wild-work3</code>。</p>
</div></body></html>`

func writeWrongPort(w http.ResponseWriter, r *http.Request, gw string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, wrongPortHTML, html.EscapeString(r.Host), html.EscapeString(gw), html.EscapeString(gw))
}

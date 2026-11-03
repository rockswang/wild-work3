// Package proxy 实现单一起源反代（W3-16）：/core/* → core、/ext/<id>/* → 插件 UI。
package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Proxy 反代器。
type Proxy struct {
	// CoreURL core 管理面地址（如 http://127.0.0.1:7864）
	CoreURL *url.URL
	// ExtURL 插件 UI 基址解析（按 plugin_id 返回地址；无则 nil）
	ExtURL func(pluginID string) *url.URL
}

// Handler 返回反代 http.Handler。
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/core/"):
			// /core/* → core 的 /api/*
			target := *p.CoreURL
			rp := &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.Out.URL.Scheme = target.Scheme
					pr.Out.URL.Host = target.Host
					pr.Out.URL.Path = "/api" + strings.TrimPrefix(pr.In.URL.Path, "/core")
					pr.Out.Host = target.Host
					pr.Out.Header.Set("X-WW3-Upstream", "core")
				},
				FlushInterval: -1, // SSE：立即刷新
				ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
					http.Error(w, "core 不可用: "+err.Error(), http.StatusBadGateway)
				},
			}
			rp.ServeHTTP(w, r)
		case strings.HasPrefix(path, "/ext/"):
			// /ext/<plugin_id>/... → 插件 UI
			rest := strings.TrimPrefix(path, "/ext/")
			id := rest
			if i := strings.Index(rest, "/"); i >= 0 {
				id = rest[:i]
			}
			if p.ExtURL == nil {
				http.Error(w, "插件 UI 未启用", http.StatusNotFound)
				return
			}
			target := p.ExtURL(id)
			if target == nil {
				http.Error(w, "插件未声明自定义 UI: "+id, http.StatusNotFound)
				return
			}
			rp := &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.Out.URL.Scheme = target.Scheme
					pr.Out.URL.Host = target.Host
					pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/ext/"+id)
					pr.Out.Host = target.Host
					pr.Out.Header.Set("X-WW3-Upstream", "ext:"+id)
				},
				FlushInterval: -1,
				ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
					http.Error(w, "插件 UI 不可用: "+err.Error(), http.StatusBadGateway)
				},
			}
			rp.ServeHTTP(w, r)
		default:
			// 其余路径原样透传给 core（Web UI 静态资产在 core，W3-37 定案 1）。
			// 不剥前缀、不改路径；/v1 /gw /healthz /status 在 gateway 的 mux 上是更具体模式，不会走到这里。
			if p.CoreURL == nil {
				http.NotFound(w, r)
				return
			}
			rp := &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.SetURL(p.CoreURL)
					pr.Out.Host = p.CoreURL.Host
					pr.Out.Header.Set("X-WW3-Upstream", "core-ui")
				},
				FlushInterval: -1,
				ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
					http.Error(w, "core 不可用: "+err.Error(), http.StatusBadGateway)
				},
			}
			rp.ServeHTTP(w, r)
		}
	})
}

// ParseURL 宽松解析（失败返回 nil）。
func ParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	return u
}

// HostOnly 从监听地址提取 host（用于 Host 头校验）。
func HostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

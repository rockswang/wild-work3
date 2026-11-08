// Package gw 实现 gateway 管理面 HTTP API（/gw/*）：版本、插件商店/安装/升级/回滚/卸载、更新、退出。
//
// 返回结构以 core 的 Web UI（app.js）为权威契约（W3-17）：
//
//	GET /gw/version        → {gateway, core, protocol, ok}
//	GET /gw/plugins        → 裸数组 [{plugin_id,name,version,latest,kind,...}]
//	GET /gw/plugins/store  → 裸数组（注册表条目 + installed/version/latest/versions）
//	GET /gw/plugins/store/errors → 裸数组 [{file,reason}]
//	GET /gw/updates        → {gateway:{current,latest,update:false,manual:true}, main:{...}, job, catalog}
//
// W3-48 v2：注册表 = 平铺注册文件（不签名、不托管二进制、不约定 tag），
// 包由发布页定位（tags.atom → expanded_assets），下载走多源竞速 + sha256 强制校验。
package gw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work3/main/gateway/internal/registry"
	"wild-work3/main/gateway/internal/store"
)

// Server gateway 管理面。
type Server struct {
	Store *store.Manager
	// Dist 分发配置（注册表仓 + 代理 + 竞速判据）。
	Dist    Dist
	Version map[string]string
	// CoreVersionFn 拉取 core 版本（可为 nil）
	CoreVersionFn func() map[string]string
	// ReloadFn 安装/卸载/切换后通知 core 重扫插件
	ReloadFn func()
	OnQuit   func()
	// CoreExe 待替换的 ww3-core 可执行文件路径（更新用，空则只能下载不能应用）
	CoreExe string
	// HaltCore 杀 core 并等它真退出（Windows 下句柄释放后才能 rename），可为 nil
	HaltCore func() error
	// StartCore 替换后重新拉起 core，可为 nil
	StartCore func()
	// UpdateDir 更新包目录（downloaded 的 main.zip）
	UpdateDir string
	// GatewayExe 自身可执行路径（仅用于「手动升级」提示里写清要换哪个文件，W3-38）
	GatewayExe string

	cache *registry.Cache // 注册表快照按来源缓存

	mu      sync.Mutex
	update2 *updateJob // main.zip 更新任务状态（idle/downloading/ready/failed/applied/rolled_back）
}

// Routes 注册 /gw/* 路由。
func (s *Server) Routes(mux *http.ServeMux) {
	if s.cache == nil {
		s.cache = &registry.Cache{}
	}
	mux.HandleFunc("/gw/version", s.version)
	mux.HandleFunc("/gw/plugins", s.plugins)
	mux.HandleFunc("/gw/plugins/store", s.storeList)
	mux.HandleFunc("/gw/plugins/store/errors", s.storeErrors)
	mux.HandleFunc("/gw/plugins/", s.pluginOne)
	mux.HandleFunc("/gw/updates", s.updates)
	mux.HandleFunc("/gw/updates/status", s.updateStatus)
	mux.HandleFunc("/gw/updates/download", s.updateDownload)
	mux.HandleFunc("/gw/updates/apply", s.updateApply)
	mux.HandleFunc("/gw/updates/rollback", s.updateRollback)
	// 注册表刷新：面板「重新拉清单」按钮（TTL 未到也能强制重拉）
	mux.HandleFunc("/gw/registry/refresh", s.registryRefresh)
	mux.HandleFunc("/gw/shutdown", s.shutdown)
}

// DrainBody 保证请求体被读完（包在最外层，覆盖 /gw/*、/core/* 与 gateway 自己的登录/配置端点）。
//
// 起因：handler 不读 body 时（`/gw/registry/refresh` 完全不看参数，但前端仍会 `POST {}`），
// net/http 发现请求体未消费完，就在回完响应后直接关连接；客户端表现为
// `ConnectionResetError: [WinError 10054] 远程主机强迫关闭了一个现有的连接`——
// 实测连发 8 次挂 5~6 次。`curl` 不复用连接所以看不出来，容易误判成前端 bug。
//
// 用「handler 返回后再丢弃剩余」而不是「事先读进内存」：后者会把大请求体（/v1 可到 30MiB）
// 整个缓冲一遍，且 1MiB 上限会截断正常请求。
func DrainBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		}
	})
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true, "gateway": s.Version["gateway"], "protocol": 1}
	if s.CoreVersionFn != nil {
		if cv := s.CoreVersionFn(); cv != nil {
			out["core"] = cv["core"]
		}
	}
	writeJSON(w, out)
}

// plugins：GET 已安装列表（裸数组）。latest 只读缓存（Peek 不发网络）。
func (s *Server) plugins(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	snap, _ := s.cache.Peek(s.Dist.Registry)
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		v := installedView(p)
		if snap != nil {
			if c, ok := snap.PluginByID(p.ID); ok {
				if lat, ok2 := c.Pick(""); ok2 {
					v["latest"] = lat.Version
					v["status"] = orDefault(c.Status, "unverified")
				}
			}
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

// installedView 已安装插件视图（前端字段：plugin_id/name/version/kind/latest）。
func installedView(p store.Installed) map[string]any {
	return map[string]any{
		"plugin_id": p.ID, "name": orDefault(p.Name, p.ID), "version": p.Current,
		"kind": orDefault(p.Kind, "channel"), "description": p.Description,
		"capabilities": p.Capabilities, "icon": p.Icon, "color": p.Color,
		"installed": true, "latest": p.Current, "source": "official", "author": "wild-work3",
		"versions": p.Versions, "previous": p.Previous, "installed_at": p.InstalledAt,
	}
}

// pluginOne：/{id}、/{id}/activate、/{id}/rollback、/{id}/uninstall、/{id}/upgrade。
// 另兼容 POST /gw/plugins/install|upgrade|rollback|uninstall（body 带 plugin_id，动作在路径里）。
func (s *Server) pluginOne(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/gw/plugins/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少插件 ID")
		return
	}
	op := ""
	if len(parts) > 1 {
		op = parts[1]
	}
	// 兼容「动作在路径里、插件 ID 在 body 里」的调用（app.js 用这种形式）
	if op == "" && (id == "install" || id == "upgrade" || id == "rollback" || id == "uninstall") {
		var body struct {
			PluginID string `json:"plugin_id"`
			Version  string `json:"version"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		switch id {
		case "install":
			s.installFrom(w, r.Context(), body.PluginID, body.Version)
		case "upgrade":
			s.installFrom(w, r.Context(), body.PluginID, "")
		case "rollback":
			s.doRollback(w, body.PluginID)
		case "uninstall":
			s.doUninstall(w, body.PluginID)
		}
		return
	}
	switch op {
	case "":
		list, err := s.Store.List()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, p := range list {
			if p.ID == id {
				writeJSON(w, installedView(p))
				return
			}
		}
		writeErr(w, http.StatusNotFound, "插件未安装: "+id)
	case "activate":
		var body struct {
			Version string `json:"version"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		if err := s.Store.Activate(id, body.Version); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.reload()
		writeJSON(w, map[string]any{"ok": true, "message": "已切换到 " + body.Version})
	case "rollback":
		s.doRollback(w, id)
	case "uninstall":
		s.doUninstall(w, id)
	case "upgrade":
		s.installFrom(w, r.Context(), id, "")
	case "history":
		list, _ := s.Store.List()
		for _, p := range list {
			if p.ID == id {
				writeJSON(w, map[string]any{"ok": true, "history": p})
				return
			}
		}
		writeErr(w, http.StatusNotFound, "插件未安装: "+id)
	default:
		writeErr(w, http.StatusNotFound, "未知操作: "+op)
	}
}

func (s *Server) doRollback(w http.ResponseWriter, id string) {
	v, err := s.Store.Rollback(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.reload()
	writeJSON(w, map[string]any{"ok": true, "version": v, "message": "已回滚到 " + v})
}

// doUninstall 仅卸载安装树，不触碰 core 配置及插件凭据/统计；重装后渠道和账号可恢复。
func (s *Server) doUninstall(w http.ResponseWriter, id string) {
	if err := s.Store.Uninstall(id, false); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reload()
	writeJSON(w, map[string]any{"ok": true, "message": "已卸载 " + id + "（配置、凭据与统计已保留）", "purged": false})
}

// storeList：GET /gw/plugins/store（裸数组），POST 安装（body 带 plugin_id/version）。
func (s *Server) storeList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			PluginID string `json:"plugin_id"`
			Version  string `json:"version"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		s.installFrom(w, r.Context(), body.PluginID, body.Version)
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	snap, err := s.snapshot(r.Context(), force)
	if err != nil && snap == nil {
		writeErr(w, http.StatusBadGateway, "注册表拉取失败: "+err.Error())
		return
	}
	installed, _ := s.Store.List()
	byID := map[string]store.Installed{}
	for _, i := range installed {
		byID[i.ID] = i
	}
	out := make([]map[string]any, 0, len(snap.Plugins))
	for _, c := range snap.Plugins {
		cur := ""
		if i, ok := byID[c.ID]; ok {
			cur = i.Current
		}
		out = append(out, storeView(c, cur, snap, err))
	}
	writeJSON(w, out)
}

// storeErrors：被丢弃的注册文件/版本条目（§1.2 必须透出，不许静默吞掉）。
func (s *Server) storeErrors(w http.ResponseWriter, r *http.Request) {
	out := s.cache.Errors(s.Dist.Registry.Key())
	if out == nil {
		out = []registry.FileError{}
	}
	writeJSON(w, out)
}

// storeView 商店条目（前端字段：plugin_id/name/kind/installed/version/latest/…）。
//
// 与 v1 的差异：`catalog_signed` 删除（v2 不验签），`kind` 直接取注册文件的 `type`。
func storeView(c registry.PluginEntry, cur string, snap *registry.Snapshot, fetchErr error) map[string]any {
	latest := ""
	vers := make([]map[string]any, 0, len(c.Versions))
	for _, v := range c.Versions {
		vers = append(vers, map[string]any{"version": v.Version, "file": v.File, "os": v.OS, "arch": v.Arch,
			"size": v.Size, "changelog": v.Changelog})
		if latest == "" || registry.CmpSemver(v.Version, latest) > 0 {
			latest = v.Version
		}
	}
	conf := map[string]any{}
	if c.Conformance != nil {
		conf = map[string]any{"pass": c.Conformance.Pass, "warn": c.Conformance.Warn, "fail": c.Conformance.Fail,
			"mode": c.Conformance.Mode, "at": c.Conformance.At}
	}
	from, at := "", ""
	if snap != nil {
		from = snap.Source
		if !snap.FetchedAt.IsZero() {
			at = snap.FetchedAt.UTC().Format(time.RFC3339)
		}
	}
	v := map[string]any{
		"plugin_id": c.ID, "name": orDefault(c.Name, c.ID), "description": c.Description, "kind": c.Type,
		"repo": c.Repo, "source": "official", "icon": c.Icon, "color": c.Color,
		"capabilities": c.Capabilities, "author": orDefault(c.Author, "wild-work3"),
		"installed": cur != "", "version": cur, "latest": latest, "versions": vers,
		"installable": c.Installable(),
		// W3-48 透明度字段：注册表从哪拿的、多旧、准入结果
		"status": orDefault(c.Status, "unverified"), "conformance": conf,
		"catalog_source": from, "catalog_updated_at": at,
	}
	if fetchErr != nil {
		v["catalog_error"] = fetchErr.Error() // 陈旧快照：面板要能看出「这是上次的」
	}
	return v
}

// snapshot 取注册表快照（走 TTL 缓存；失败时返回旧快照 + 错误）。
func (s *Server) snapshot(ctx context.Context, force bool) (*registry.Snapshot, error) {
	src := s.Dist.Registry
	if src.Empty() {
		return nil, fmt.Errorf("未配置注册表仓（registry_repo / --registry）")
	}
	if force {
		s.cache.Invalidate(src.Key())
	}
	return s.cache.Get(ctx, src, s.Dist.Opts(), s.Dist.indexTTL())
}

// installFrom 安装/升级：按 id 在注册表快照里找条目 → 选版本 → 定位发布包 → 下载校验 → 安装。
//
// v2 只有一条来源（官方注册表），没有 `source_url` 覆盖（§9.1）。
func (s *Server) installFrom(w http.ResponseWriter, ctx context.Context, pluginID, version string) {
	if pluginID == "" {
		writeErr(w, http.StatusBadRequest, "plugin_id 必填")
		return
	}
	snap, err := s.snapshot(ctx, false)
	if err != nil && snap == nil {
		writeErr(w, http.StatusBadGateway, "注册表拉取失败: "+err.Error())
		return
	}
	p, ok := snap.PluginByID(pluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "注册表中无此插件: "+pluginID)
		return
	}
	ver, ok := p.Pick(version)
	if !ok {
		avail := make([]string, 0, len(p.Versions))
		for _, v := range p.Versions {
			avail = append(avail, v.Version)
		}
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("注册表条目 %s 无可用版本 %q（可用：%s）",
			pluginID, version, strings.Join(avail, ", ")))
		return
	}
	from, err := s.Store.Install(p, ver, s.deps())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reload()
	writeJSON(w, map[string]any{"ok": true, "plugin_id": p.ID, "version": ver.Version,
		"source": from, "message": orDefault(p.Name, p.ID) + " 已安装 " + ver.Version})
}

// deps 组装 store 安装参数（host allowlist + 版本门 + 竞速判据 + 发布包定位器）。
func (s *Server) deps() store.Deps {
	o := s.Dist.Opts()
	return store.Deps{
		Proxies:     s.Dist.Proxies,
		AllowHosts:  registry.AllowHosts(s.Dist.Proxies, s.Dist.ExtraHosts...),
		ProtocolVer: s.Dist.ProtocolVer,
		GatewayVer:  s.Version["gateway"],
		Opt:         o,
		Locator:     registry.NewLocator(o),
	}
}

// registryRefresh 强制重拉注册表（面板「重新拉清单」按钮：装了新插件不想等 TTL）。
func (s *Server) registryRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	src := s.Dist.Registry
	if src.Empty() {
		writeErr(w, http.StatusBadRequest, "未配置注册表仓（registry_repo / --registry）")
		return
	}
	s.cache.Invalidate(src.Key())
	snap, err := s.snapshot(r.Context(), true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "注册表拉取失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "plugins": len(snap.Plugins), "source": snap.Source,
		"updated_at": snap.FetchedAt.UTC().Format(time.RFC3339), "errors": len(snap.Errors),
		"message": "已刷新注册表"})
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
	if s.OnQuit != nil {
		go func() { time.Sleep(100 * time.Millisecond); s.OnQuit() }()
	}
}

func (s *Server) reload() {
	if s.ReloadFn != nil {
		s.ReloadFn()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

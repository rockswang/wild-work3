// Package gw 实现 gateway 管理面 HTTP API（/gw/*）：版本、插件商店/安装/升级/回滚/卸载、更新、退出。
//
// 返回结构以 gateway/web/app.js 为权威契约：
//
//	GET /gw/version      → {gateway, core, protocol, ok}
//	GET /gw/plugins      → 裸数组 [{plugin_id,name,version,latest,kind,...}]
//	GET /gw/plugins/store→ 裸数组（同上 + installed/source/author/description）
//	GET /gw/updates      → {gateway:{current,latest,update}, core:{current,latest,update,changelog}}
package gw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"wild-work3/main/gateway/internal/registry"
	"wild-work3/main/gateway/internal/store"
)

// Server gateway 管理面。
type Server struct {
	Store *store.Manager
	// Dist 分发配置（W3-38：registry 独立仓 + 单一信任根 + 多源竞速）。
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
	// UpdateDir 更新包目录（downloaded 的 core 更新包）
	UpdateDir string
	// GatewayExe 自身可执行路径（仅用于「手动升级」提示里写清要换哪个文件，W3-38）
	GatewayExe string

	cache *registry.Cache // index.json 按来源缓存

	mu      sync.Mutex
	update2 *updateJob // core 更新任务状态（idle/downloading/ready/failed/applied/rolled_back）
	main_   *mainSlot  // update.json 拉取结果（TTL）
}

// Routes 注册 /gw/* 路由。
func (s *Server) Routes(mux *http.ServeMux) {
	if s.cache == nil {
		s.cache = &registry.Cache{}
	}
	mux.HandleFunc("/gw/version", s.version)
	mux.HandleFunc("/gw/plugins", s.plugins)
	mux.HandleFunc("/gw/plugins/store", s.storeList)
	mux.HandleFunc("/gw/plugins/", s.pluginOne)
	mux.HandleFunc("/gw/updates", s.updates)
	mux.HandleFunc("/gw/updates/status", s.updateStatus)
	mux.HandleFunc("/gw/updates/download", s.updateDownload)
	mux.HandleFunc("/gw/updates/apply", s.updateApply)
	mux.HandleFunc("/gw/updates/rollback", s.updateRollback)
	// 清单刷新：面板上的「重新拉清单」按钮（缓存 TTL 未到也能强制重拉，装新插件后不用等）
	mux.HandleFunc("/gw/registry/refresh", s.registryRefresh)
	mux.HandleFunc("/gw/shutdown", s.shutdown)
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

// plugins：GET 已安装列表（裸数组）。
func (s *Server) plugins(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// latest 从清单缓存里抹（Peek 不发网络请求：商店列表页才会拉，这里只能用已缓存的）；
	// 没缓存就退化成「当前版本即最新」，不故弄出可升级红点。
	ix, _ := s.cache.Peek(s.Dist.Registry)
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		v := installedView(p)
		if ix != nil {
			if c, ok := ix.PluginByID(p.ID); ok {
				if lat, ok2 := pickVersion(c.Versions, ""); ok2 {
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
// 另兼容 POST /gw/plugins/install|upgrade|rollback|uninstall（body 带 plugin_id）。
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
			Source   string `json:"source_url"`
			Purge    bool   `json:"purge"` // 连旧布局遗留的 auths/logs/stat 一起删（默认只退软件）
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch id {
		case "install":
			s.installFrom(w, body.PluginID, body.Version, body.Source)
		case "upgrade":
			s.installLatest(w, body.PluginID)
		case "rollback":
			s.doRollback(w, body.PluginID)
		case "uninstall":
			s.doUninstall(w, body.PluginID, body.Purge)
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
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := s.Store.Activate(id, body.Version); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.reload()
		writeJSON(w, map[string]any{"ok": true, "message": "已切换到 " + body.Version})
	case "rollback":
		s.doRollback(w, id)
	case "uninstall":
		s.doUninstall(w, id, false)
	case "upgrade":
		s.installLatest(w, id)
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

// doUninstall 卸载插件。默认**只删安装树里的二进制**（W3-39 第 1 条：数据已搬到 core 数据根，
// 安装树里的三个契约目录属旧布局遗留，同样保留）；`purge=true` 才连遗留数据一起删。
func (s *Server) doUninstall(w http.ResponseWriter, id string, purge bool) {
	if err := s.Store.Uninstall(id, purge); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reload()
	msg := "已卸载 " + id + "（凭据与统计已保留）"
	if purge {
		msg = "已卸载 " + id + "（含凭据与统计）"
	}
	writeJSON(w, map[string]any{"ok": true, "message": msg, "purged": purge})
}

// storeList：GET /gw/plugins/store（裸数组），POST 安装（body 带 plugin_id/version）。
//
// 清单来源优先级：`?source=` / body.source_url（用户导入的社区清单 URL）> Dist.Registry（官方 registry 仓）。
// 列表页只拉一份（指定来源或官方），安装时再按 id 到各来源里找。
func (s *Server) storeList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			PluginID string `json:"plugin_id"`
			Version  string `json:"version"`
			Source   string `json:"source_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.installFrom(w, body.PluginID, body.Version, body.Source)
		return
	}
	src, err := s.listSource(r.URL.Query().Get("source"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ix, from, signed, err := s.index(r.Context(), src)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "清单拉取失败: "+err.Error())
		return
	}
	installed, _ := s.Store.List()
	byID := map[string]store.Installed{}
	for _, i := range installed {
		byID[i.ID] = i
	}
	list := ix.Plugins
	if r.URL.Query().Get("expand") == "1" {
		list = s.expandPlugins(r.Context(), src, list) // 点开版本下拉时才逐条取详情（有限并发）
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		cur := ""
		if i, ok := byID[c.ID]; ok {
			cur = i.Current
		}
		out = append(out, storeView(c, cur, from, signed, ix.UpdatedAt))
	}
	writeJSON(w, out)
}

// expandPlugins 把指针条目展开成完整条目（有限并发 6，一条失败不影响其他，原样保留指针形状）。
// 只在显式 ?expand=1 时跑：20 个插件串着拉就是 20 次外网往返，商店首屏不该等它。
func (s *Server) expandPlugins(ctx context.Context, src registry.Source, in []registry.PluginEntry) []registry.PluginEntry {
	out := make([]registry.PluginEntry, len(in))
	copy(out, in)
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, p := range in {
		if !p.IsPointer() {
			continue
		}
		wg.Add(1)
		go func(i int, p registry.PluginEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if got, err := registry.LoadPlugin(ctx, src, p, s.Dist.Opts()); err == nil {
				out[i] = got
			}
		}(i, p)
	}
	wg.Wait()
	return out
}

// storeView 商店条目（前端字段：plugin_id/name/kind/installed/version/latest/…）。
func storeView(c registry.PluginEntry, cur, from string, signed bool, updatedAt string) map[string]any {
	latest := c.Latest // 指针条目靠索引声明的 latest（不在列表里隔次展开详情）
	vers := make([]map[string]any, 0, len(c.Versions))
	for _, v := range c.Versions {
		vers = append(vers, map[string]any{"version": v.Version, "os": v.OS, "arch": v.Arch,
			"size": v.Size, "asset": v.Asset, "changelog": v.Changelog})
		if latest == "" || registry.CmpSemver(v.Version, latest) > 0 {
			latest = v.Version
		}
	}
	conf := map[string]any{}
	if c.Conformance != nil {
		conf = map[string]any{"pass": c.Conformance.Pass, "warn": c.Conformance.Warn, "fail": c.Conformance.Fail,
			"mode": c.Conformance.Mode, "at": c.Conformance.At}
	}
	return map[string]any{
		"plugin_id": c.ID, "name": orDefault(c.Name, c.ID), "description": c.Description, "kind": c.Kind,
		"repo": c.Repo, "source": orDefault(c.Channel, "official"), "icon": c.Icon, "color": c.Color,
		"capabilities": c.Capabilities, "author": orDefault(c.Author, "wild-work3"),
		"installed": cur != "", "version": cur, "latest": latest, "versions": vers,
		// detail=false 表示这是指针条目，版本列表需要展开（前端点开下拉时用 ?expand=1 再拉一次）
		"detail": len(c.Versions) > 0,
		// W3-38 透明度字段：清单从哪拿的、多旧、有没有验签、准入结果
		"status": orDefault(c.Status, "unverified"), "verified_at": c.VerifiedAt,
		"conformance": conf, "catalog_source": from, "catalog_signed": signed,
		"catalog_updated_at": updatedAt,
	}
}

// listSource 解析「列商店」用的清单来源。
func (s *Server) listSource(give string) (registry.Source, error) {
	if give != "" {
		return registry.Source{URL: give, Name: "custom"}, nil
	}
	if s.Dist.Registry.Ref.OwnerRepo != "" || s.Dist.Registry.URL != "" {
		return s.Dist.Registry, nil
	}
	return registry.Source{}, fmt.Errorf("未配置插件清单来源（registry_repo / --registry）")
}

// index 取清单（走 TTL 缓存；失败时上层会拿到旧副本 + 错误）。
func (s *Server) index(ctx context.Context, src registry.Source) (*registry.Index, string, bool, error) {
	ix, from, signed, _, err := s.cache.Get(ctx, src, s.Dist.PubKeys, s.Dist.Opts(), s.Dist.indexTTL())
	return ix, from, signed, err
}

func (s *Server) installFrom(w http.ResponseWriter, pluginID, version, source string) {
	if pluginID == "" {
		writeErr(w, http.StatusBadRequest, "plugin_id 必填")
		return
	}
	srcs := s.installSources(source)
	if len(srcs) == 0 {
		writeErr(w, http.StatusBadRequest, "未配置插件清单来源（registry_repo / --registry）")
		return
	}
	ctx := context.Background()
	for _, src := range srcs {
		ix, _, _, err := s.index(ctx, src)
		if err != nil || ix == nil {
			continue // 某一份清单拉不到不阻断，继续找下一个来源
		}
		p, ok := ix.PluginByID(pluginID)
		if !ok {
			continue
		}
		if p.IsPointer() {
			// 详情文件与索引同位置解析：registry 仓走同一 release（同 tag），社区清单走同目录
			got, err := registry.LoadPlugin(ctx, src, p, s.Dist.Opts())
			if err != nil {
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
			p = got
		}
		ver, ok := pickVersion(p.Versions, version)
		if !ok {
			writeErr(w, http.StatusBadRequest, "清单中无本平台可用版本: "+pluginID)
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
		return
	}
	writeErr(w, http.StatusNotFound, "清单中无此插件: "+pluginID)
}

func (s *Server) installLatest(w http.ResponseWriter, id string) {
	s.installFrom(w, id, "", "")
}

// installSources 安装时按序查找的清单来源：显式指定 > 官方 > 已导入的社区。
func (s *Server) installSources(source string) []registry.Source {
	if source != "" {
		return []registry.Source{{URL: source, Name: "custom"}}
	}
	var out []registry.Source
	if s.Dist.Registry.Ref.OwnerRepo != "" || s.Dist.Registry.URL != "" {
		out = append(out, s.Dist.Registry)
	}
	out = append(out, s.Dist.Community...)
	return out
}

// deps 组装 store 安装参数（host allowlist + 版本门 + 竞速判据）。
func (s *Server) deps() store.Deps {
	return store.Deps{
		Proxies:     s.Dist.Proxies,
		AllowHosts:  registry.AllowHosts(s.Dist.Proxies, s.Dist.ExtraHosts...),
		ProtocolVer: s.Dist.ProtocolVer,
		GatewayVer:  s.Version["gateway"],
		Opt:         s.Dist.Opts(),
	}
}

// ---- 更新（gateway 自身 / core）----
// 实现在 update.go：gateway 分支按 W3-38 恒回「需手动全包替换」，core 分支走真更新（rename trick）。

// registryRefresh 强制重拉清单（面板「重新拉清单」按钮：装了新插件不想等 30min TTL）。
// body 可带 `{"source_url": "..."}` 刷当前正在看的那一份（社区清单同样适用）。
func (s *Server) registryRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var body struct {
		SourceURL string `json:"source_url"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) // 无 body 也算正常
	src := s.Dist.Registry
	if body.SourceURL != "" {
		src = registry.Source{URL: body.SourceURL, Name: "custom"}
	}
	if src.Ref.OwnerRepo == "" && src.URL == "" {
		writeErr(w, http.StatusBadRequest, "未配置插件清单来源（registry_repo / --registry）")
		return
	}
	s.cache.Invalidate(src.Key())
	ix, from, signed, err := s.index(r.Context(), src)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "清单拉取失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "plugins": len(ix.Plugins), "source": from,
		"signed": signed, "updated_at": ix.UpdatedAt, "message": "已刷新清单"})
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

// pickVersion 选版本：指定了就精确匹配，否则取当前平台（os/arch 缺省视为通用）里版本号最高的，
// 不依赖清单的书写顺序（registry 里版本可能倒序写）。
func pickVersion(vers []registry.AssetEntry, want string) (registry.AssetEntry, bool) {
	if len(vers) == 0 {
		return registry.AssetEntry{}, false
	}
	if want != "" {
		for _, v := range vers {
			if v.Version == want {
				return v, true
			}
		}
		return registry.AssetEntry{}, false
	}
	best := -1
	for i, v := range vers {
		if v.OS != "" && v.OS != runtime.GOOS {
			continue
		}
		if v.Arch != "" && v.Arch != runtime.GOARCH {
			continue
		}
		if best < 0 || registry.CmpSemver(v.Version, vers[best].Version) > 0 {
			best = i
		}
	}
	if best < 0 {
		return registry.AssetEntry{}, false
	}
	return vers[best], true
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

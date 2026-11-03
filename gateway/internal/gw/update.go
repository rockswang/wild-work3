// 更新管理器（W3-38）。
//
// 两条分支，待遇完全不同：
//   - **gateway 自身不做自动升级**：自更新代码活在永不自更新的进程里才有意义，「升级完重启后若变砖则回滚」
//     在单进程自举下不可能兑现。/gw/updates 的 gateway 分支恒报「需手动全包替换」并给出可执行路径与 release 直链。
//   - **core 走真更新**：main 仓 update.json → 多源并发竞速下载（fetch.go）→ sha256 必填 →
//     Windows rename trick 替换（运行中的 exe 无法覆盖，但可以被改名）→ 重新拉起。
//
// 保留 `.old`：它是唯一的回滚路径，所以不像旧版 PR188 那样启动时清理。
package gw

import (
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"wild-work3/main/gateway/internal/registry"
	"wild-work3/main/gateway/internal/store"
)

// Dist 分发配置（由 main.go 从 config.json 的 gateway 段组装）。
type Dist struct {
	// Registry 官方插件清单来源（默认 = registry 独立仓的 index.json release 资产）。
	Registry registry.Source
	// Main main 仓 update.json 的来源（gateway/core/官方样板的版本门与包清单）。
	// 支持 release 资产形态（默认）与绝对 URL 形态（本地测试/自建镜像）。
	Main registry.Source
	// Community 用户导入的社区清单来源（商店合并、安装按 id 逐份找）。
	Community []registry.Source
	// PubKeys 内置 ed25519 公钥（hex）。空=不验签（开发期），面板会标「未验签」。
	PubKeys []string
	// Proxies 下载代理前缀集合（与直连一起并发竞速）。
	Proxies []string
	// ExtraHosts 额外允许的下载主机（社区清单把包放在自建站时用）。
	ExtraHosts []string
	// ProtocolVer 当前 IPC 协议版本（版本门）。
	ProtocolVer int
	// Fetch 竞速判据（零值即用缺省）；Proxies 由 Opts() 注入。
	Fetch registry.Options
	// TTL 清单缓存时长，零值 → 30min。
	TTL time.Duration
}

// Opts 补齐代理后的竞速参数。
func (d Dist) Opts() registry.Options {
	o := d.Fetch
	o.Proxies = d.Proxies
	return o
}

func (d Dist) indexTTL() time.Duration {
	if d.TTL <= 0 {
		return 30 * time.Minute
	}
	return d.TTL
}

// updateJob 一次 core 更新的状态（内存态：gateway 重启即丢，重新检查即可）。
type updateJob struct {
	State     string    `json:"state"` // idle | downloading | ready | failed | applied | rolled_back
	Pct       int       `json:"progress"`
	Version   string    `json:"version,omitempty"`
	Asset     string    `json:"asset,omitempty"`
	Src       string    `json:"source,omitempty"`
	Path      string    `json:"-"` // 已落盘的包（或解出的 exe）
	Bytes     int64     `json:"downloaded_bytes,omitempty"`
	Total     int64     `json:"total_bytes,omitempty"`
	Rate      float64   `json:"rate_bps,omitempty"`
	Message   string    `json:"message"`
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func (j *updateJob) view() map[string]any {
	if j == nil {
		return map[string]any{"state": "idle", "progress": 0, "message": "无进行中的更新"}
	}
	c := *j
	out := map[string]any{
		"state": c.State, "progress": c.Pct, "message": c.Message,
		"version": c.Version, "asset": c.Asset, "source": c.Src,
		"downloaded_bytes": c.Bytes, "total_bytes": c.Total, "rate_bps": c.Rate,
	}
	if !c.StartedAt.IsZero() {
		out["started_at"] = c.StartedAt.UTC().Format(time.RFC3339)
	}
	if !c.UpdatedAt.IsZero() {
		out["updated_at"] = c.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// mainSlot main 仓 update.json 的 TTL 缓存。
type mainSlot struct {
	mu     sync.Mutex
	ent    *registry.MainEntry
	src    string
	signed bool
	at     time.Time
	err    error
}

// main 清单拉取的固定超时与失败重试间隔。
const (
	mainFetchTimeout = 12 * time.Second
	mainErrTTL       = 60 * time.Second
)

// job 取当前任务快照（锁内拷贝，避免与下载 goroutine 竞读）。
func (s *Server) job() *updateJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.update2 == nil {
		return nil
	}
	c := *s.update2
	return &c
}

// setJob 更新任务状态（fn 在锁内改，返回的快照供调用方直接序列化）。
func (s *Server) setJob(fn func(j *updateJob)) *updateJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.update2 == nil {
		s.update2 = &updateJob{State: "idle", StartedAt: time.Now()}
	}
	fn(s.update2)
	s.update2.UpdatedAt = time.Now()
	c := *s.update2
	return &c
}

// mainEntry 拉 main 仓 update.json（TTL 缓存；force=手动「检查更新」时忽略 TTL）。
// 拉取失败时返回旧副本 + 错误，让面板仍能显示「上次已知版本」。
//
// 两个细节：① 失败也要短 TTL 负缓存——面板会轮询本页，没负缓存时每次都被拖成一次外网请求；
// ② 给外网取数包一层固定超时且不随客户端断连取消，否则 /gw/updates 会挂在面板请求的 ctx 上。
func (s *Server) mainEntry(ctx context.Context, force bool) (*registry.MainEntry, string, bool, error) {
	if s.Dist.Main.Ref.OwnerRepo == "" && s.Dist.Main.URL == "" {
		return nil, "", false, fmt.Errorf("未配置 main 仓（main_repo / --main-repo）")
	}
	s.mu.Lock()
	if s.main_ == nil {
		s.main_ = &mainSlot{}
	}
	sl := s.main_
	s.mu.Unlock()

	sl.mu.Lock()
	defer sl.mu.Unlock()
	ttl := s.Dist.indexTTL()
	if sl.ent == nil {
		ttl = mainErrTTL // 从未成功过：失败后的重试间隔
	}
	if !force && !sl.at.IsZero() && time.Since(sl.at) < ttl {
		return sl.ent, sl.src, sl.signed, sl.err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mainFetchTimeout)
	defer cancel()
	ent, from, signed, err := registry.LoadMain(cctx, s.Dist.Main, s.Dist.PubKeys, s.Dist.Opts())
	sl.at = time.Now()
	if err != nil {
		sl.err = err // 保留旧副本（如有），调用方看 err 就知道拿到的是陈的
		return sl.ent, sl.src, sl.signed, err
	}
	sl.ent, sl.src, sl.signed, sl.err = ent, from, signed, nil
	return ent, from, signed, nil
}

// coreExeName 待替换的 core 可执行文件名（Windows 带 .exe）。
func coreExeName(path string) string {
	if path != "" {
		return filepath.Base(path)
	}
	if runtime.GOOS == "windows" {
		return "ww3-core.exe"
	}
	return "ww3-core"
}

// ---- /gw/updates* 实现 ----

func (s *Server) updates(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	cur := ""
	if s.CoreVersionFn != nil {
		if cv := s.CoreVersionFn(); cv != nil {
			cur = cv["core"]
		}
	}
	gate := s.Version["gateway"]

	// gateway 分支：只报状态 + 手动路径，永不 update=true（W3-38）
	release := s.releasePage()
	gwView := map[string]any{
		"current": gate, "latest": "", "update": false, "manual": true,
		"release": release,
		"note": "gateway 不参与自动升级：请整包替换 " + exeOr(s.GatewayExe) +
			"。运行中的 exe 无法覆盖，先把它改名（如 " + exeOr(s.GatewayExe) + ".old）再放入新文件；" +
			"协议兼容由清单版本门把关，core 与插件仍可自动更新。",
	}

	out := map[string]any{"ok": true, "gateway": gwView, "job": s.jobView()}
	ent, from, signed, err := s.mainEntry(r.Context(), force)
	core := map[string]any{"current": cur, "latest": cur, "update": false, "changelog": "",
		"catalog_source": from, "catalog_signed": signed}
	if err != nil {
		core["error"] = err.Error()
	} else if ent != nil {
		name := coreExeName(s.CoreExe)
		if a, ok := ent.CurrentAssetFor(name); ok {
			core["latest"] = a.Version
			core["changelog"] = orDefault(a.Changelog, ent.Notes)
			core["asset"] = a.Asset
			// 版本比当前高才算可更新；比较失败（自产版本怪形状）就保守地不提示
			if registry.CmpSemver(a.Version, cur) > 0 {
				core["update"] = true
			}
		} else {
			core["error"] = fmt.Sprintf("update.json 里没有当前平台（%s/%s）的 %s 包", runtime.GOOS, runtime.GOARCH, name)
		}
		core["version_gate"] = map[string]any{"min": ent.Gate.Min, "max": ent.Gate.Max}
	}
	out["core"] = core
	// 回滚可用性：只看 .old 在不在（W3-38 保留 .old 作为唯一回滚路径）
	if s.CoreExe != "" {
		if _, err := os.Stat(s.CoreExe + ".old"); err == nil {
			core["rollback_available"] = true
		}
	}
	// registry 清单缓存状态（用户要能看出「清单从哪拿的、多旧」）
	out["catalog"] = s.catalogState()
	writeJSON(w, out)
}

// catalogState 官方清单的缓存元信息。
func (s *Server) catalogState() map[string]any {
	if s.cache == nil || (s.Dist.Registry.URL == "" && s.Dist.Registry.Ref.OwnerRepo == "") {
		return nil
	}
	src, at, signed, err := s.cache.State(s.Dist.Registry.Key())
	v := map[string]any{"source": src, "signed": signed}
	if !at.IsZero() {
		v["fetched_at"] = at.UTC().Format(time.RFC3339)
	}
	if err != nil {
		v["error"] = err.Error()
	}
	return v
}

// jobView 序列化当前任务。
func (s *Server) jobView() map[string]any { return s.job().view() }

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.jobView())
}

// updateDownload 起一次后台下载（立即返回，前端轮询 /gw/updates 的 job 看进度）。
//
// 之所以不阻塞本请求：实测 18MB 包经 gh-proxy 12s 只走 994KB，同步接口会让面板挂在一次长请求上。
func (s *Server) updateDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	ent, from, _, err := s.mainEntry(r.Context(), true)
	if err != nil || ent == nil {
		writeErr(w, http.StatusBadGateway, "取 update.json 失败: "+errString(err))
		return
	}
	name := coreExeName(s.CoreExe)
	asset, ok := ent.CurrentAssetFor(name)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("update.json 无当前平台（%s/%s）的 %s 包", runtime.GOOS, runtime.GOARCH, name))
		return
	}
	// 版本门：main 清单条目可声明 gateway 区间（core 包要求 gateway 够新时拦下来）
	if err := checkMainGate(ent, s.Version["gateway"]); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if j := s.job(); j != nil && j.State == "downloading" {
		writeJSON(w, map[string]any{"ok": true, "job": j.view(), "message": "已有下载在进行中"})
		return
	}
	dir := s.updateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setJob(func(j *updateJob) {
		*j = updateJob{State: "downloading", Version: asset.Version, Asset: asset.Asset,
			Src: from, StartedAt: time.Now(), Message: "下载中：" + asset.Asset}
	})
	go s.runDownload(ent, asset, dir)
	writeJSON(w, map[string]any{"ok": true, "job": s.jobView(), "message": "已开始下载 " + asset.Version})
}

// runDownload 后台跑一次包下载（多源竞速 + sha256 必填 + Range 续传）。
func (s *Server) runDownload(ent *registry.MainEntry, asset registry.AssetEntry, dir string) {
	urls := s.packageURLs(asset)
	if len(urls) == 0 {
		s.failJob("包定位无法展开（update.json 既没 url 也没 asset 名）")
		return
	}
	urls = filterHosts(urls, registry.AllowHosts(s.Dist.Proxies, s.Dist.ExtraHosts...))
	if len(urls) == 0 {
		s.failJob("所有源主机都不在允许列表内")
		return
	}
	dest := filepath.Join(dir, filepath.Base(asset.Asset))
	opt := s.Dist.Opts()
	prog := func(src string, done, total int64, rate float64) {
		pct := 0
		if total > 0 {
			pct = int(done * 100 / total)
		}
		s.setJob(func(j *updateJob) {
			j.Bytes, j.Total, j.Rate, j.Src = done, total, rate, src
			if pct > j.Pct {
				j.Pct = pct // 多源并发，进度只前进不回退
			}
		})
	}
	from, err := registry.FetchFile(context.Background(), urls, dest, asset.SHA256, opt, prog)
	if err != nil {
		s.failJob(err.Error())
		return
	}
	exe := dest
	if strings.HasSuffix(strings.ToLower(dest), ".zip") {
		xd := strings.TrimSuffix(dest, filepath.Ext(dest)) + "-x"
		_ = os.RemoveAll(xd)
		if err := store.Extract(dest, xd); err != nil {
			s.failJob("解压失败: " + err.Error())
			return
		}
		got, err := findExe(xd, coreExeName(s.CoreExe))
		if err != nil {
			s.failJob(err.Error())
			return
		}
		exe = got
	}
	s.setJob(func(j *updateJob) {
		j.State, j.Path, j.Pct, j.Src = "ready", exe, 100, from
		j.Message = "已下载 " + j.Version + "，可应用（应用会短暂中断对话）"
	})
}

// packageURLs 包的候选源：绝对 URL 优先，否则沿用 main 清单的发布位置（同仓同 tag 换个资产名）。
func (s *Server) packageURLs(asset registry.AssetEntry) []string {
	if asset.URL != "" {
		return registry.AbsoluteSources(asset.URL, s.Dist.Proxies)
	}
	if asset.Asset == "" {
		return nil
	}
	return registry.IndexFrom(s.Dist.Main, asset.Asset).Sources(s.Dist.Proxies)
}

// releasePage gateway 手动升级时要打开的 release 页（从 main 清单来源反推）。
func (s *Server) releasePage() string {
	if s.Dist.Main.Ref.OwnerRepo != "" {
		return "https://github.com/" + s.Dist.Main.Ref.OwnerRepo + "/releases/latest"
	}
	if u := s.Dist.Main.URL; u != "" {
		if i := strings.LastIndex(u, "/"); i > 0 {
			return u[:i]
		}
	}
	return ""
}

// updateApply 停 core → rename trick 替换 → 重新拉起。
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	j := s.job()
	if j == nil || j.State != "ready" || j.Path == "" {
		writeErr(w, http.StatusBadRequest, "请先下载更新包（当前无待应用版本）")
		return
	}
	if s.CoreExe == "" {
		writeErr(w, http.StatusBadRequest, "未配置 core 可执行文件路径，无法替换")
		return
	}
	if _, err := os.Stat(j.Path); err != nil {
		writeErr(w, http.StatusBadRequest, "更新包已不在（"+j.Path+"），请重新下载")
		return
	}
	if s.HaltCore != nil {
		if err := s.HaltCore(); err != nil {
			s.failJob("停止 core 失败: " + err.Error())
			writeErr(w, http.StatusInternalServerError, "停止 core 失败: "+err.Error())
			return
		}
	}
	if err := replaceExe(j.Path, s.CoreExe); err != nil {
		// 替换失败（内部已尝试回滚 .old）：core 必须重新拉起，不能留在停止态
		if s.StartCore != nil {
			s.StartCore()
		}
		s.setJob(func(x *updateJob) { x.State, x.Message = "failed", err.Error() })
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.StartCore != nil {
		s.StartCore() // Halt 置了 stopped，必须走 Resume（ReloadFn 走的 Restart 不清该标）
	} else if s.ReloadFn != nil {
		s.ReloadFn()
	}
	s.setJob(func(x *updateJob) {
		x.State, x.Pct = "applied", 100
		x.Message = "core 已更新到 " + x.Version + "，正在重启"
	})
	writeJSON(w, map[string]any{"ok": true, "version": j.Version,
		"message": "core 已更新到 " + j.Version + "，正在重启"})
}

// updateRollback 把 .old 改回原位（唯一可用的回滚路径）。
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if s.CoreExe == "" {
		writeErr(w, http.StatusBadRequest, "未配置 core 可执行文件路径")
		return
	}
	bak := s.CoreExe + ".old"
	if _, err := os.Stat(bak); err != nil {
		writeErr(w, http.StatusBadRequest, "无可回滚版本（未找到 "+filepath.Base(bak)+"）")
		return
	}
	if s.HaltCore != nil {
		if err := s.HaltCore(); err != nil {
			writeErr(w, http.StatusInternalServerError, "停止 core 失败: "+err.Error())
			return
		}
	}
	if err := os.Rename(bak, s.CoreExe); err != nil {
		if s.StartCore != nil {
			s.StartCore()
		}
		writeErr(w, http.StatusInternalServerError, "回滚失败: "+err.Error())
		return
	}
	if s.StartCore != nil {
		s.StartCore()
	} else if s.ReloadFn != nil {
		s.ReloadFn()
	}
	s.setJob(func(x *updateJob) { x.State, x.Message = "rolled_back", "已回滚到上一版 core" })
	writeJSON(w, map[string]any{"ok": true, "message": "已回滚到上一版 core"})
}

// failJob 把任务置失败并留可读原因。
func (s *Server) failJob(msg string) {
	s.setJob(func(j *updateJob) { j.State, j.Message = "failed", msg })
}

func (s *Server) updateDir() string {
	if s.UpdateDir != "" {
		return s.UpdateDir
	}
	return filepath.Join(os.TempDir(), "ww3-update")
}

// checkMainGate 校验 main 清单条目的 gateway 版本门。
func checkMainGate(ent *registry.MainEntry, gate string) error {
	if ent == nil {
		return nil
	}
	if ent.Gate.Min != "" && registry.CmpSemver(gate, ent.Gate.Min) < 0 {
		return &registry.ErrGate{Msg: fmt.Sprintf("该 core 更新需要 gateway ≥%s，当前为 %s（请先手动升级 gateway）", ent.Gate.Min, gate)}
	}
	if ent.Gate.Max != "" && registry.CmpSemver(gate, ent.Gate.Max) > 0 {
		return &registry.ErrGate{Msg: fmt.Sprintf("该 core 更新只支持 gateway ≤%s，当前为 %s", ent.Gate.Max, gate)}
	}
	return nil
}

// filterHosts 按 allowlist 过滤候选源。
func filterHosts(urls []string, allow map[string]bool) []string {
	var out []string
	for _, u := range urls {
		if hostAllowed(u, allow) {
			out = append(out, u)
		}
	}
	return out
}

// hostAllowed URL 主机是否在允许列表（W3-38：不给任意 URL 送 API Key 的风险留口子）。
func hostAllowed(raw string, allow map[string]bool) bool {
	if len(allow) == 0 {
		return true
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	return allow[strings.ToLower(u.Hostname())]
}

// copyAll 带同步语义的拷贝。
func copyAll(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}

// replaceExe Windows 关键约束：运行中的 exe 被文件锁占用、无法原地覆盖，但**可以改名**。
// 于是：旧文件改名 .old → 新文件挪到位 → 任一步失败则把 .old 改回（回滚）。
// .old 保留不删：它是 updateRollback 的唯一依据。
func replaceExe(newPath, target string) error {
	bak := target + ".old"
	had := false
	if _, err := os.Stat(target); err == nil {
		// 先清掉上一轮的 .old，否则改名会撞已存在文件
		_ = os.Remove(bak)
		if err := os.Rename(target, bak); err != nil {
			return fmt.Errorf("旧版改名失败（core 进程可能还没退出或文件被占用）: %w", err)
		}
		had = true
	}
	if err := moveInto(newPath, target); err != nil {
		if had {
			if rb := os.Rename(bak, target); rb != nil {
				return fmt.Errorf("写入新包失败: %v；回滚也失败: %v（请手工把 %s 改回 %s）", err, rb, filepath.Base(bak), filepath.Base(target))
			}
		}
		return fmt.Errorf("写入新包失败（已回滚旧版）: %w", err)
	}
	_ = os.Chmod(target, 0o755)
	return nil
}

// moveInto 同盘直接 rename（原子），跨盘退化为 tmp+copy+rename：
// 绝不直接截断目标路径，避免「写一半就是新 exe」的半包状态。
func moveInto(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	tmp := dst + ".new"
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = copyAll(out, in)
	_ = out.Sync()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(src)
	return nil
}

// findExe 在解压目录里找目标 exe（包内可能带一层子目录）。
func findExe(dir, name string) (string, error) {
	var found string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, e error) error {
		if e != nil || info == nil || info.IsDir() {
			return e
		}
		if strings.EqualFold(info.Name(), name) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("解压包里找不到 %s", name)
	}
	return found, nil
}

func exeOr(p string) string {
	if p == "" {
		return "wild-work3.exe"
	}
	return p
}

func errString(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}

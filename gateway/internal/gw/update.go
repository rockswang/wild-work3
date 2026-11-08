// 更新管理器（W3-38 + W3-48 v2）。
//
// 两条分支，待遇完全不同：
//   - **gateway 自身不做自动升级**：自更新代码活在永不自更新的进程里才有意义，「升级完重启后若变砖则回滚」
//     在单进程自举下不可能兑现。/gw/updates 的 gateway 分支恒报「需手动全包替换」并给出可执行路径。
//   - **main.zip（core + 两个官方样板）走真更新**：注册表 `main.json` → 发布页定位包 →
//     多源并发竞速下载 → sha256 强制校验 → Windows rename trick 替换 core exe → 装两个插件 → 重新拉起。
//
// 保留 `.old`：它是 core 唯一的回滚路径，所以不像旧版 PR188 那样启动时清理。
package gw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wild-work3/main/gateway/internal/registry"
	"wild-work3/main/gateway/internal/store"
)

// Dist 分发配置（由 main.go 从 config.json 的 gateway 段组装）。
type Dist struct {
	// Registry 官方注册表来源（`owner/repo` + ref；URL 形态只为离线夹具留口子）。
	Registry registry.Source
	// Proxies 下载代理前缀集合（与直连一起并发竞速）。
	Proxies []string
	// ExtraHosts 额外允许的下载主机（测试/mock 用；生产应为空）。
	ExtraHosts []string
	// ProtocolVer 当前 IPC 协议版本（版本门）。
	ProtocolVer int
	// Fetch 竞速判据（零值即用缺省）；Proxies 由 Opts() 注入。
	Fetch registry.Options
	// TTL 注册表缓存时长，零值 → 30min。
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

// updateJob 一次 main.zip 更新的状态（内存态：gateway 重启即丢，重新检查即可）。
type updateJob struct {
	State     string    `json:"state"` // idle | downloading | ready | failed | applied | rolled_back
	Pct       int       `json:"progress"`
	Version   string    `json:"version,omitempty"`
	Asset     string    `json:"asset,omitempty"`
	Src       string    `json:"source,omitempty"`
	Path      string    `json:"-"` // 已落盘的包（zip）
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

// mainEntry 从注册表快照里取 main 条目（走与插件同一个 Cache；TTL + 失败负缓存由 Cache 管）。
func (s *Server) mainEntry(ctx context.Context, force bool) (registry.PluginEntry, *registry.Snapshot, error) {
	snap, err := s.snapshot(ctx, force)
	if err != nil && snap == nil {
		return registry.PluginEntry{}, nil, fmt.Errorf("注册表拉取失败: %w", err)
	}
	if snap.Main == nil {
		return registry.PluginEntry{}, snap, fmt.Errorf("注册表里没有 main.json（主程序条目）")
	}
	return *snap.Main, snap, nil
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
	gwView := map[string]any{
		"current": gate, "latest": "", "update": false, "manual": true,
		"release": s.releasePage(),
		"note": "gateway 不参与自动升级：请整包替换 " + exeOr(s.GatewayExe) +
			"。运行中的 exe 无法覆盖，先把它改名（如 " + exeOr(s.GatewayExe) + ".old）再放入新文件；" +
			"协议兼容由注册表版本门把关，core 与插件仍可自动更新。",
	}

	out := map[string]any{"ok": true, "gateway": gwView, "job": s.jobView()}
	ent, snap, err := s.mainEntry(r.Context(), force)
	core := map[string]any{"current": cur, "latest": cur, "update": false, "changelog": "",
		"catalog_source": "", "catalog_updated_at": ""}
	if snap != nil {
		core["catalog_source"] = snap.Source
		if !snap.FetchedAt.IsZero() {
			core["catalog_updated_at"] = snap.FetchedAt.UTC().Format(time.RFC3339)
		}
	}
	if err != nil {
		core["error"] = err.Error()
	} else {
		if ent.Gateway != nil {
			// gateway 只展示（不自动替换），给面板说清「该换哪个文件」
			gwView["latest"] = ent.Gateway.Version
			gwView["sha256"] = ent.Gateway.SHA256
			if ent.Gateway.Note != "" {
				// 注册表给的说明是**补充**，不能顶掉「换哪个文件、怎么换」这句可执行指引
				gwView["note"] = gwView["note"].(string) + " 注册表说明：" + ent.Gateway.Note
			}
		}
		// main.zip 的版本条目描述的是「core + 两个官方样板」这一整包
		if a, ok := ent.Pick(""); ok {
			core["latest"] = a.Version
			core["changelog"] = a.Changelog
			core["asset"] = "main.zip"
			core["sha256"] = a.SHA256
			if registry.CmpSemver(a.Version, cur) > 0 {
				core["update"] = true
			}
			core["version_gate"] = map[string]any{"min": a.Gate.Min, "max": a.Gate.Max}
		} else {
			core["error"] = fmt.Sprintf("main.json 里没有当前平台（%s/%s）的包", runtime.GOOS, runtime.GOARCH)
		}
	}
	out["core"] = core
	// 回滚可用性：只看 .old 在不在（保留 .old 作为唯一回滚路径）
	if s.CoreExe != "" {
		if _, err := os.Stat(s.CoreExe + ".old"); err == nil {
			core["rollback_available"] = true
		}
	}
	out["catalog"] = s.catalogState()
	writeJSON(w, out)
}

// catalogState 官方注册表的缓存元信息。
func (s *Server) catalogState() map[string]any {
	if s.cache == nil || s.Dist.Registry.Empty() {
		return nil
	}
	src, at, err := s.cache.State(s.Dist.Registry.Key())
	v := map[string]any{"source": src}
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

// updateDownload 起一次后台下载（立即返回，前端轮询 job 看进度）。
//
// 之所以不阻塞本请求：实测 18MB 包经 gh-proxy 12s 只走 994KB，同步接口会让面板挂在一次长请求上。
func (s *Server) updateDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	ent, _, err := s.mainEntry(r.Context(), true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "取 main.json 失败: "+errString(err))
		return
	}
	asset, ok := ent.Pick("")
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("main.json 无当前平台（%s/%s）的包", runtime.GOOS, runtime.GOARCH))
		return
	}
	if err := checkMainGate(asset, s.Version["gateway"]); err != nil {
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
		*j = updateJob{State: "downloading", Version: asset.Version, Asset: "main.zip",
			StartedAt: time.Now(), Message: "定位发布包中…"}
	})
	go s.runDownload(ent.Repo, asset, dir)
	writeJSON(w, map[string]any{"ok": true, "job": s.jobView(), "message": "已开始下载 " + asset.Version})
}

// runDownload 后台跑一次包下载（发布页定位 + 多源竞速 + sha256 必填 + Range 续传）。
func (s *Server) runDownload(repo string, asset registry.VersionEntry, dir string) {
	opt := s.Dist.Opts()
	urls, err := registry.NewLocator(opt).URLs(context.Background(), repo, asset)
	if err != nil {
		s.failJob(err.Error())
		return
	}
	urls = filterHosts(urls, registry.AllowHosts(s.Dist.Proxies, s.Dist.ExtraHosts...))
	if len(urls) == 0 {
		s.failJob("所有源主机都不在允许列表内")
		return
	}
	dest := filepath.Join(dir, "main-"+asset.Version+".zip")
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
	s.setJob(func(j *updateJob) {
		j.State, j.Path, j.Pct, j.Src = "ready", dest, 100, from
		j.Message = "已下载 " + j.Version + "（core + 官方样板），可应用（应用会短暂中断对话）"
	})
}

// releasePage gateway 手动升级时要打开的 release 页（从注册表来源反推）。
func (s *Server) releasePage() string {
	if s.Dist.Registry.Repo != "" {
		return "https://github.com/" + s.Dist.Registry.Repo + "/releases"
	}
	if u := s.Dist.Registry.URL; u != "" {
		if i := strings.LastIndex(u, "/"); i > 0 {
			return u[:i]
		}
	}
	return ""
}

// updateApply 停 core → 解包 → rename trick 替换 core exe → 装两个官方插件 → 重新拉起。
//
// 顺序固定，任一步失败就停并留可读原因（§8.2）。
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
	// 先解包（解包失败不该动 core）
	xd := strings.TrimSuffix(j.Path, filepath.Ext(j.Path)) + "-x"
	_ = os.RemoveAll(xd)
	if err := store.Extract(j.Path, xd); err != nil {
		s.failJob("解压失败: " + err.Error())
		writeErr(w, http.StatusInternalServerError, "解压失败: "+err.Error())
		return
	}
	exe, err := findExe(xd, coreExeName(s.CoreExe))
	if err != nil {
		s.failJob(err.Error())
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if s.HaltCore != nil {
		if err := s.HaltCore(); err != nil {
			s.failJob("停止 core 失败: " + err.Error())
			writeErr(w, http.StatusInternalServerError, "停止 core 失败: "+err.Error())
			return
		}
	}
	if err := replaceExe(exe, s.CoreExe); err != nil {
		// 替换失败（内部已尝试回滚 .old）：core 必须重新拉起，不能留在停止态
		if s.StartCore != nil {
			s.StartCore()
		}
		s.setJob(func(x *updateJob) { x.State, x.Message = "failed", err.Error() })
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 包内的两个官方插件按各自 manifest 落到版本目录（§8.2 第 3 步）；同版本跳过，不重写 current
	installed, perr := s.installBundled(xd)
	if s.StartCore != nil {
		s.StartCore() // Halt 置了 stopped，必须走 Resume
	} else if s.ReloadFn != nil {
		s.ReloadFn()
	}
	msg := "core 已更新到 " + j.Version + "，正在重启"
	if perr != nil {
		msg += "；但官方插件安装失败: " + perr.Error()
	} else if len(installed) > 0 {
		msg += "；已同步插件 " + strings.Join(installed, ", ")
	}
	s.setJob(func(x *updateJob) {
		x.State, x.Pct, x.Message = "applied", 100, msg
	})
	writeJSON(w, map[string]any{"ok": true, "version": j.Version, "plugins": installed, "message": msg})
}

// installBundled 装 main.zip 里带的官方插件（`<exe 词干>.manifest.json` 配对）。
// 已是同版本则跳过（避免每次 apply 都改 current 触发 core 不必要的重扫）。
func (s *Server) installBundled(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	have := map[string]string{}
	if list, err := s.Store.List(); err == nil {
		for _, p := range list {
			have[p.ID] = p.Current
		}
	}
	var done []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".manifest.json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".manifest.json")
		exePath := filepath.Join(dir, stem)
		if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(exePath), ".exe") {
			exePath += ".exe"
		}
		if _, err := os.Stat(exePath); err != nil {
			continue // 没有配套 exe，跳过（不是插件条目）
		}
		if err := s.Store.InstallBundle(exePath, filepath.Join(dir, name)); err != nil {
			return done, err
		}
		var man struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			_ = json.Unmarshal(b, &man)
		}
		if man.ID != "" && have[man.ID] != man.Version {
			done = append(done, man.ID+"@"+man.Version)
		}
	}
	return done, nil
}

// updateRollback 把 .old 改回原位（core 唯一可用的回滚路径）。
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

// checkMainGate 校验 main 版本条目的 gateway 版本门。
func checkMainGate(v registry.VersionEntry, gate string) error {
	if v.Gate.Min != "" && registry.CmpSemver(gate, v.Gate.Min) < 0 {
		return &registry.ErrGate{Msg: fmt.Sprintf("该 core 更新需要 gateway ≥%s，当前为 %s（请先手动升级 gateway）", v.Gate.Min, gate)}
	}
	if v.Gate.Max != "" && registry.CmpSemver(gate, v.Gate.Max) > 0 {
		return &registry.ErrGate{Msg: fmt.Sprintf("该 core 更新只支持 gateway ≤%s，当前为 %s", v.Gate.Max, gate)}
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

// hostAllowed URL 主机是否在允许列表。
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

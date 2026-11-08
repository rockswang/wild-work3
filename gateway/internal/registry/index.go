// 注册表快照：codeload tarball 一次拿全仓 → 逐个根目录 `<id>.json` 解析 → 单文件失败只丢该文件。
//
// 为什么是 tarball 而不是逐个文件：平铺形态没有 index.json，客户端必须先枚举才知道有哪些注册文件；
// tarball 是唯一能「一次枚举 + 一次取全」的形态，且实测三代理 200 且 sha256 完全一致（746KB）。
// 条件请求（If-None-Match → 304）让刷新几乎零流量（实测 codeload 支持，Cache-Control max-age=60）。
package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// fileRe 注册文件命名规则（§1.1，硬）：kebab-case，小写字母开头，只允许 a-z0-9-。
var fileRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*\.json$`)

// MainID 保留 ID：主程序（`main.json`）。插件不得占用。
const MainID = "main"

// Source 注册表来源：一个 GitHub 仓库（`owner/repo`）+ 分支/tag/commit。
//
// URL 是给离线夹具与 mock 上游留的口子（直接给 tarball 地址），生产永远走 Repo。
type Source struct {
	Repo string // owner/repo
	Ref  string // 分支/tag/commit，空 → "main"
	URL  string // 绝对 tarball URL（测试/mirror）
	Name string // 展示名（"official"）
}

// Key 缓存键（同一来源共用一份快照与 TTL）。
func (s Source) Key() string {
	if s.URL != "" {
		return "url:" + s.URL
	}
	return "repo:" + s.Repo + "@" + s.ref()
}

func (s Source) ref() string {
	if strings.TrimSpace(s.Ref) == "" {
		return "main"
	}
	return strings.TrimSpace(s.Ref)
}

// TarballURL 该来源的 tarball 直连地址（不含代理前缀）。
func (s Source) TarballURL(o Options) string {
	if s.URL != "" {
		return s.URL
	}
	return o.codeloadBase() + "/" + s.Repo + "/tar.gz/refs/heads/" + s.ref()
}

// URLs 全部候选源（直连 + 各代理前缀，并发竞速）。
func (s Source) URLs(o Options) []string {
	return AbsoluteSources(s.TarballURL(o), o.Proxies)
}

// Empty 是否未配置。
func (s Source) Empty() bool { return s.URL == "" && s.Repo == "" }

// Snapshot 一次注册表拉取的完整结果。
type Snapshot struct {
	Plugins   []PluginEntry // 全部插件（不含 main）
	Main      *PluginEntry  // main.json（可缺）
	Errors    []FileError   // 被丢弃的文件/版本条目（必须透出给面板，§1.2）
	Source    string        // 命中的源 URL
	ETags     map[string]string
	FetchedAt time.Time
}

// PluginByID 按 id 取插件条目（含 main）。
func (s *Snapshot) PluginByID(id string) (PluginEntry, bool) {
	if s == nil {
		return PluginEntry{}, false
	}
	if s.Main != nil && s.Main.ID == id {
		return *s.Main, true
	}
	for _, p := range s.Plugins {
		if p.ID == id {
			return p, true
		}
	}
	return PluginEntry{}, false
}

// Installable 有当前平台可用版本才算可安装（§1.2：versions 为空 → 显示但不可安装）。
func (p PluginEntry) Installable() bool {
	_, ok := p.Pick("")
	return ok
}

// Load 拉取并解析注册表。prev 非空时对同源发条件请求，304 直接复用（只刷新时间）。
func Load(ctx context.Context, src Source, o Options, prev *Snapshot) (*Snapshot, error) {
	if src.Empty() {
		return nil, fmt.Errorf("未配置注册表仓（registry_repo / --registry）")
	}
	var etags map[string]string
	if prev != nil {
		etags = prev.ETags
	}
	body, from, etag, notModified, err := FetchConditional(ctx, src.URLs(o), o, etags)
	if err != nil {
		return nil, err
	}
	if notModified && prev != nil {
		c := *prev
		c.FetchedAt = time.Now()
		c.Source = from
		if c.ETags == nil {
			c.ETags = map[string]string{}
		}
		if etag != "" {
			c.ETags[from] = etag
		}
		return &c, nil
	}
	snap, err := ParseTarball(body)
	if err != nil {
		return nil, err
	}
	snap.Source = from
	snap.FetchedAt = time.Now()
	snap.ETags = map[string]string{}
	if etag != "" {
		snap.ETags[from] = etag
	}
	return snap, nil
}

// ParseTarball 解析注册表仓的 codeload tarball：只看「顶层目录 + 一个 .json」的条目。
//
// 分级处置：文件名不合规则 → 静默忽略（可能只是别人的杂项 JSON）；
// JSON/字段/id 问题 → 只丢该文件并计入 Errors；版本条目问题 → 只丢该条。
func ParseTarball(b []byte) (*Snapshot, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("注册表包不是 gzip（可能连到了 HTML 错误页）: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	snap := &Snapshot{}
	seen := map[string]string{} // id → 文件名（撞 id 时只留先到的）
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读注册表包失败: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		dir, file := path.Split(strings.TrimPrefix(h.Name, "./"))
		// 只认「根」或「顶层目录」下的文件：codeload tarball 是 <repo>-<ref>/<file>，
		// 但手工 `tar -czf x.tgz .` 会平铺，两种都收（否则换个打包方式就整份清单为空）；
		// 更深层的（如 .github/workflows/x.json）忽略。
		if strings.Count(strings.Trim(dir, "/"), "/") > 0 {
			continue
		}
		if !fileRe.MatchString(file) {
			continue // 命名不合规：忽略，不记错误
		}
		if h.Size > 1<<20 {
			snap.Errors = append(snap.Errors, FileError{File: file, Reason: "文件过大（>1MiB），已跳过"})
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			snap.Errors = append(snap.Errors, FileError{File: file, Reason: "读取失败: " + err.Error()})
			continue
		}
		p, warns, err := ParseFile(file, raw)
		snap.Errors = append(snap.Errors, warns...)
		if err != nil {
			snap.Errors = append(snap.Errors, FileError{File: file, Reason: err.Error()})
			continue
		}
		if prev, dup := seen[p.ID]; dup {
			snap.Errors = append(snap.Errors, FileError{File: file, Reason: "id 与 " + prev + " 重复，已跳过"})
			continue
		}
		seen[p.ID] = file
		if p.ID == MainID {
			if p.Type != TypeMain {
				snap.Errors = append(snap.Errors, FileError{File: file, Reason: "保留 ID main 必须 type=main，已跳过"})
				continue
			}
			cp := p
			snap.Main = &cp
			continue
		}
		if p.Type == TypeMain {
			snap.Errors = append(snap.Errors, FileError{File: file, Reason: "type=main 只允许用于 main.json，已跳过"})
			continue
		}
		snap.Plugins = append(snap.Plugins, p)
	}
	sort.Slice(snap.Plugins, func(i, j int) bool { return snap.Plugins[i].ID < snap.Plugins[j].ID })
	return snap, nil
}

// Cache 按来源缓存注册表快照（TTL）+ 失败负缓存（60s）。
//
// 拉取失败时**继续用旧快照**并把错误一并透出：调用方看 err 非空就知道拿到的是陈旧数据
// （W3-08：清单获取失败不影响现有插件运行）。
type Cache struct {
	mu    sync.Mutex
	items map[string]*cacheItem
	load  sync.Mutex // 串行化远端拉取，避免并发刷新打两份外网
}

type cacheItem struct {
	snap *Snapshot
	at   time.Time
	err  error
}

// negTTL 失败负缓存：面板会 3s 轮询，没负缓存会次次拖成一次外网请求。
const negTTL = 60 * time.Second

// Get 取某来源的快照；ttl 内直接用缓存，否则重拉（重拉失败回旧快照 + 错误）。
func (c *Cache) Get(ctx context.Context, src Source, o Options, ttl time.Duration) (*Snapshot, error) {
	key := src.Key()
	c.mu.Lock()
	it := c.items[key]
	c.mu.Unlock()
	if it != nil {
		age := time.Since(it.at)
		if it.err != nil && age < negTTL {
			return it.snap, it.err
		}
		if it.err == nil && it.snap != nil && age < ttl {
			return it.snap, nil
		}
	}

	c.load.Lock()
	defer c.load.Unlock()
	c.mu.Lock()
	var prev *Snapshot
	if it := c.items[key]; it != nil {
		prev = it.snap
	}
	c.mu.Unlock()

	snap, err := Load(ctx, src, o, prev)
	now := time.Now()
	c.mu.Lock()
	if c.items == nil {
		c.items = map[string]*cacheItem{}
	}
	it = c.items[key]
	if it == nil {
		it = &cacheItem{}
		c.items[key] = it
	}
	if err != nil {
		it.err, it.at = err, now // 保留旧 snap
		out := *it
		c.mu.Unlock()
		return out.snap, err
	}
	it.snap, it.at, it.err = snap, now, nil
	c.mu.Unlock()
	return snap, nil
}

// Peek 只看缓存、不发网络（已安装列表与面板首屏不能阻塞在拉注册表上）。
func (c *Cache) Peek(src Source) (*Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it := c.items[src.Key()]; it != nil && it.snap != nil {
		return it.snap, true
	}
	return nil, false
}

// State 缓存概览（面板展示「清单从哪拿的、多旧、上次错误」）。
func (c *Cache) State(key string) (src string, at time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it := c.items[key]; it != nil {
		if it.snap != nil {
			src = it.snap.Source
		}
		return src, it.at, it.err
	}
	return "", time.Time{}, nil
}

// Errors 缓存里上次解析出的被丢弃条目（`/gw/plugins/store/errors` 用，不发网络）。
func (c *Cache) Errors(key string) []FileError {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it := c.items[key]; it != nil && it.snap != nil {
		return it.snap.Errors
	}
	return nil
}

// Invalidate 让下一次 Get 强制重拉；key 为空则清全部。
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key == "" {
		c.items = nil
		return
	}
	if it := c.items[key]; it != nil {
		it.at = time.Time{}
	}
}

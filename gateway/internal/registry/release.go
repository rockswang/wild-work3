// 发布包定位：`repo + version + file` 拼出下载地址（W3-49）。
//
// 三个普通网页端点，两个用途：
//
//	<gh>/<o>/<r>/tags.atom   7KB，10 个 tag（首选，两代理全通）——只用来把 version 换成 tag
//	<gh>/<o>/<r>/releases.atom   406KB，10 个 release（备用，ghfast 恒 403）
//	<gh>/<o>/<r>/releases/download/<tag>/<file>  下载（302 → release-assets）
//
// **不再枚举资产列表**（早期版本要拉 `expanded_assets` 57KB 再猜哪个 zip 是本平台的，
// 猜错就装错包）；文件名由注册文件的 `versions[].file` 直接给出，语义确定。
//
// 为什么不用 api.github.com：实测未认证 60req/h/IP 极易耗完（本机出口 X-RateLimit-Remaining: 0），
// 且 gh-proxy/ghfast 对它 403（代理全废）；本机 github.com 整域还会阶段性 TCP 挂死。
//
// 两个 atom 都**不支持分页**（实测 ?page=2/3 返回同一份内容），所以只能看到最近 10 个；
// 目标版本不在其中时直接失败并提示手动安装，**不猜 tag 拼 URL**（实测 v1.7.1/1.7.1 全 404）。
package registry

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"sync"
	"time"
)

// releaseTTL 发布页缓存（tag 集合 + 各 tag 的资产列表）。
//
// atom/expanded 是 must-revalidate（§4.4），TTL 只用于**失败后的负缓存与并发去重**；
// 真正定位资产时仍发条件请求（304 零字节，成本极低）。
const releaseTTL = 5 * time.Minute

var (
	// tags.atom / releases.atom 里的 tag 链接（实测都在 releases/tag/ 下）。
	reAtomTag = regexp.MustCompile(`href="[^"]*/releases/tag/([^"]+)"`)
	// semver 片段（从 tag 里抽 x.y.z）。
	reSemver = regexp.MustCompile(`\d+(?:\.\d+){1,3}`)
)

// ErrLocate 定位不到发布包（可读中文，直接透面板）。
type ErrLocate struct{ Msg string }

func (e *ErrLocate) Error() string { return e.Msg }

// Release 一次「按版本号定位到资产」的结果。
type Release struct {
	Tag   string // 命中的 tag（原样）
	File  string // 包文件名（来自注册文件 versions[].file）
	URL   string // 直连下载地址（不含代理前缀）
	TagOK bool
}

// releaseCache 发布页缓存：key = `owner/repo`，value = tag 集合 + 各 tag 资产表。
type releaseCache struct {
	mu sync.Mutex
	m  map[string]*releaseItem
}

type releaseItem struct {
	v   []string // tags 或 assets（两种缓存共用同一形状）
	at  time.Time
	err error
}

// LocateRelease 按 semver 在已发布 tag 里找目标版本，再用注册文件给的文件名拼下载地址。
//
// 三步（W3-49）：取 tag 列表（tags.atom → 退 releases.atom）→ semver 匹配 →
// `<gh>/<repo>/releases/download/<tag>/<file>`。
//
// 早期版本还有第四步「拉 expanded_assets 枚举资产再挑 zip」，已删除：
// 文件名已知时枚举只能带来歧义（多个 zip 挑哪个？靠平台关键字猜就会装错平台）。
func LocateRelease(ctx context.Context, repo string, v VersionEntry, o Options, c *releaseCache) (Release, error) {
	if v.Version == "" {
		return Release{}, &ErrLocate{Msg: "注册文件未声明版本，无法定位发布包"}
	}
	if v.File == "" {
		return Release{}, &ErrLocate{Msg: fmt.Sprintf(
			"注册条目 %s 的版本 %s 缺 file 字段（发布包文件名），无法定位发布包", repo, v.Version)}
	}
	tags, err := c.tags(ctx, repo, o)
	if err != nil {
		return Release{}, err
	}
	tag, ok := matchTag(tags, v.Version)
	if !ok {
		return Release{}, &ErrLocate{Msg: fmt.Sprintf(
			"版本 %s 不在 %s 最近 %d 个 tag 内（%s），无法自动定位发布包；请手动安装或更新注册表条目",
			v.Version, repo, len(tags), strings.Join(tags, ", "))}
	}
	return Release{Tag: tag, File: v.File, TagOK: true,
		URL: o.githubBase() + "/" + repo + "/releases/download/" + tag + "/" + v.File}, nil
}

// PackageURLs 定位结果的全部候选源（直连 + 各代理，并发竞速）。
func (r Release) PackageURLs(o Options) []string { return AbsoluteSources(r.URL, o.Proxies) }

// matchTag 在 tag 列表里找与 version 语义相等的 tag。
//
// 匹配口径：抽 tag 里的 semver 片段（`v1.4.2`/`1.4.2`/`release-1.4.2`/`jq-1.7.1` 都能抽出来），
// 与目标版本 `CmpSemver` 相等即命中；预发布后缀（rc/beta）要求 tag 里也含该后缀。
// 多个命中时取**最靠前**的（atom 是时间倒序，越靠前越新）。
func matchTag(tags []string, version string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(version))
	wantNum, pre := want, ""
	if i := strings.IndexByte(want, '-'); i >= 0 {
		wantNum, pre = want[:i], leadingLetters(want[i+1:])
	}
	for _, t := range tags {
		m := reSemver.FindString(t)
		if m == "" || CmpSemver(m, wantNum) != 0 {
			continue
		}
		// 预发布必须两边一致：`v2.0.0-rc1` 不得被当成正式版 2.0.0（反之亦然）
		if tagPre(t, m) != pre {
			continue
		}
		return t, true
	}
	return "", false
}

// tagPre 抽出 tag 里 semver 片段之后的前导字母后缀（`v2.0.0-rc1` → `rc`，`1.9.0` → 空）。
func tagPre(tag, semver string) string {
	i := strings.Index(tag, semver)
	if i < 0 {
		return ""
	}
	s := tag[i+len(semver):]
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	return leadingLetters(s)
}

// leadingLetters 取字符串开头连续字母（小写），`rc1` → `rc`、`beta.2` → `beta`。
func leadingLetters(s string) string {
	end := 0
	for end < len(s) && ((s[end] >= 'a' && s[end] <= 'z') || (s[end] >= 'A' && s[end] <= 'Z')) {
		end++
	}
	return strings.ToLower(s[:end])
}

// ---- 缓存层 ----

// tags 取 tag 列表（tags.atom 首选 7KB，失败退 releases.atom 406KB）。
func (c *releaseCache) tags(ctx context.Context, repo string, o Options) ([]string, error) {
	key := "tags:" + repo
	if v, ok := c.cached(key); ok {
		return v, nil
	}
	base := o.githubBase() + "/" + repo
	b, from, err := FetchURLs(ctx, AbsoluteSources(base+"/tags.atom", o.Proxies), withSmallLimit(o))
	var tags []string
	if err == nil {
		tags = parseAtomTags(b)
	}
	if len(tags) == 0 {
		// 退 releases.atom（ghfast 对它恒 403，但 gh-proxy 可用）
		b2, from2, err2 := FetchURLs(ctx, AbsoluteSources(base+"/releases.atom", o.Proxies), withSmallLimit(o))
		if err2 != nil {
			msg := err2.Error()
			if err != nil {
				msg = err.Error() + "；" + msg
			}
			return nil, &ErrLocate{Msg: fmt.Sprintf("取 %s 的 tag 列表失败（tags.atom 与 releases.atom 均不可用）: %s", repo, msg)}
		}
		tags, from = parseAtomTags(b2), from2
	}
	if len(tags) == 0 {
		return nil, &ErrLocate{Msg: fmt.Sprintf("%s 的 atom feed 里没有可识别的 tag（源 %s）", repo, shortURL(from))}
	}
	c.put(key, tags, nil)
	return tags, nil
}

// cached 命中未过期的缓存（失败项 60s 负缓存，避免面板轮询反复打外网）。
func (c *releaseCache) cached(key string) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	it := c.m[key]
	if it == nil {
		return nil, false
	}
	age := time.Since(it.at)
	if it.err != nil && age < negTTL {
		return nil, false
	}
	if age > releaseTTL {
		return nil, false
	}
	if it.err != nil {
		return nil, false
	}
	return it.v, true
}

func (c *releaseCache) put(key string, v []string, err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*releaseItem{}
	}
	c.m[key] = &releaseItem{v: v, at: time.Now(), err: err}
}

// Locator 发布包定位器（持有 tag 列表的短 TTL 缓存，给 store/gw 共用）。
type Locator struct {
	o Options
	c releaseCache
}

// NewLocator 构造定位器（Options 里的 GitHubBase/CodeloadBase 只为离线夹具留口子）。
func NewLocator(o Options) *Locator { return &Locator{o: o} }

// URLs 按 repo + 版本条目定位包，返回全部候选源（直连 + 各代理）。
//
// 定位只需 `repo + version + file`（W3-49）：文件名的唯一性由注册文件保证，
// 客户端不再枚举资产列表，因此也没有「挑哪个 zip」的歧义。
func (l *Locator) URLs(ctx context.Context, repo string, v VersionEntry) ([]string, error) {
	rel, err := LocateRelease(ctx, repo, v, l.o, &l.c)
	if err != nil {
		return nil, err
	}
	return rel.PackageURLs(l.o), nil
}

// Locate 定位结果（tag/文件名/直连 URL），供面板展示「包从哪来」。
func (l *Locator) Locate(ctx context.Context, repo string, v VersionEntry) (Release, error) {
	return LocateRelease(ctx, repo, v, l.o, &l.c)
}

// withSmallLimit 发布页响应体上限：tags.atom 7KB / releases.atom 406KB，
// 给 2MiB 余量（防上游改版变大），同时挡住「连到 HTML 错误页」的超大响应。
func withSmallLimit(o Options) Options {
	o.MaxBody = 2 << 20
	return o
}

// parseAtomTags 从 atom feed 里抓 tag（保留 feed 顺序 = 时间倒序）。
func parseAtomTags(b []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reAtomTag.FindAllSubmatch(b, -1) {
		t := html.UnescapeString(string(m[1]))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

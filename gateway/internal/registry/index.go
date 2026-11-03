// 清单加载：取 index.json + index.sig → 验签 → 解析；外加按来源缓存的 Cache。
//
// 签名只覆盖 index.json 的**原始字节**（不是重新序列化后的 JSON）：清单一旦规范化再签，
// 任何一次 map 序变化都会让验签失败，而 GitHub 资产是原样字节返回的，取到什么就验什么。
package registry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SigSuffix 签名资产名约定：清单资产名 + ".sig"。
const SigSuffix = ".sig"

// SigRefOf 由清单定位推出签名文件定位（同 repo、同 tag、资产名加 .sig）。
func SigRefOf(g GitHubRelease) GitHubRelease {
	g.Asset += SigSuffix
	return g
}

// Source 一份清单的来源：registry 仓的 release 资产（Ref），或用户粘贴的绝对 URL。
// 两者都走同一套多源竞速与验签规则（W3-38：镜像必须校验同一份可信签名清单）。
type Source struct {
	Ref  GitHubRelease // 官方 registry（wild-work3/ww3-registry 的 index.json）
	URL  string        // 绝对 URL 形态（社区清单、本地文件、离线夹具）
	Name string        // 展示名（"official" / "community" / 自定义）
}

// Key 缓存键（同一来源共用一份缓存与 TTL）。
func (s Source) Key() string {
	if s.URL != "" {
		return "url:" + s.URL
	}
	return "ref:" + s.Ref.OwnerRepo + "/" + s.Ref.Asset + "@" + s.Ref.Tag
}

// Sources 该清单的全部候选 URL。
func (s Source) Sources(proxies []string) []string {
	if s.URL != "" {
		return AbsoluteSources(s.URL, proxies)
	}
	return s.Ref.Sources(proxies)
}

// IndexFrom 把来源换成「同一发布位置下的另一个资产」（索引 → 签名、索引 → 插件详情）。
// 沿用原来源的 tag 与 URL 目录，不让详情文件跳到另一个 release。
func IndexFrom(src Source, asset string) Source {
	if src.Ref.OwnerRepo != "" {
		return Source{Ref: GitHubRelease{OwnerRepo: src.Ref.OwnerRepo, Asset: asset, Tag: src.Ref.Tag}}
	}
	if src.URL != "" {
		if i := strings.LastIndex(src.URL, "/"); i > 0 {
			return Source{URL: src.URL[:i+1] + asset}
		}
	}
	return src
}

// LoadIndex 取并解析清单。返回 (index, 命中源, 是否已验签, error)。
//
// 验签策略（fail closed，但不阻塞尚未发布密钥的开发期）：
//   - 配了公钥：必须能取到 <清单>.sig 且验过，否则整份清单不采信；
//   - 没配公钥：照常返回但 signed=false，由上层在面板上标「未验签」，发布前必须内置公钥。
func LoadIndex(ctx context.Context, src Source, pubKeys []string, o Options) (*Index, string, bool, error) {
	raw, from, err := FetchURLs(ctx, src.Sources(o.Proxies), o)
	if err != nil {
		return nil, "", false, fmt.Errorf("取清单失败: %w", err)
	}
	signed := false
	if len(pubKeys) > 0 {
		sig, _, serr := FetchURLs(ctx, AbsoluteSources(sigURL(src), o.Proxies), o)
		if serr != nil {
			return nil, from, false, fmt.Errorf("取清单签名失败: %w", serr)
		}
		if err := Verify(pubKeys, raw, strings.TrimSpace(string(sig))); err != nil {
			return nil, from, false, err
		}
		signed = true
	}
	ix, err := ParseIndex(raw)
	if err != nil {
		return nil, from, signed, err
	}
	return ix, from, signed, nil
}

// sigURL 清单签名文件的 URL（资产定位加 .sig 后缀 / 绝对 URL 直接拼后缀）。
func sigURL(src Source) string {
	if src.URL != "" {
		return src.URL + SigSuffix
	}
	return SigRefOf(src.Ref).Direct()
}

// LoadMain 取并解析 main 仓的 update.json（gateway/core/官方样板的版本门与包清单）。
// 与 LoadIndex 同规则：走 Source（registry 仓 release 资产或绝对 URL），签名覆 update.json 原始字节。
func LoadMain(ctx context.Context, src Source, pubKeys []string, o Options) (*MainEntry, string, bool, error) {
	raw, from, err := FetchURLs(ctx, src.Sources(o.Proxies), withSmallLimit(o))
	if err != nil {
		return nil, "", false, fmt.Errorf("取 update.json 失败: %w", err)
	}
	signed := false
	if len(pubKeys) > 0 {
		sig, _, serr := FetchURLs(ctx, IndexFrom(src, src.assetName()+SigSuffix).Sources(o.Proxies), withSmallLimit(o))
		if serr != nil {
			return nil, from, false, fmt.Errorf("取 update.json.sig 失败: %w", serr)
		}
		if err := Verify(pubKeys, raw, strings.TrimSpace(string(sig))); err != nil {
			return nil, from, false, err
		}
		signed = true
	}
	m, err := ParseMain(raw)
	if err != nil {
		return nil, from, signed, err
	}
	return m, from, signed, nil
}

// withSmallLimit 清单/签名是 KB 级 JSON：给内存取加上限，避免连到 HTML 页或代理错误页。
func withSmallLimit(o Options) Options {
	if o.MaxBody == 0 {
		o.MaxBody = 8 << 20
	}
	return o
}

// assetName 来源的资产名（签名/详情文件沿用同一发布位置时用）。
func (s Source) assetName() string {
	if s.Ref.Asset != "" {
		return s.Ref.Asset
	}
	if s.URL != "" {
		if i := strings.LastIndex(s.URL, "/"); i >= 0 {
			return s.URL[i+1:]
		}
	}
	return "index.json"
}

// Cache 按来源缓存清单（TTL）：registry 是低频变更数据，面板每次刷新不该都打外网。
// 拉取失败时**继续用旧副本**（W3-08：清单获取失败不影响现有插件运行），但把错误一并透出，
// 调用方看 err 非空就知道拿到的是陈旧数据。
type Cache struct {
	mu    sync.Mutex
	items map[string]*cacheItem
	load  sync.Mutex // 串行化远端拉取，避免并发刷新打两份外网
}

type cacheItem struct {
	idx    *Index
	src    string
	signed bool
	at     time.Time
	err    error
}

// Get 取某来源的清单；ttl 内直接用缓存，否则重拉（重拉失败回旧副本 + 错误）。
func (c *Cache) Get(ctx context.Context, src Source, pubKeys []string, o Options, ttl time.Duration) (*Index, string, bool, time.Time, error) {
	key := src.Key()
	c.mu.Lock()
	it := c.items[key]
	c.mu.Unlock()
	if it != nil && it.idx != nil && time.Since(it.at) < ttl {
		return it.idx, it.src, it.signed, it.at, it.err
	}

	c.load.Lock()
	defer c.load.Unlock()
	ix, from, signed, err := LoadIndex(ctx, src, pubKeys, o)
	at := time.Now()
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
		it.err = err // 保留旧 idx（若有），只更新错误
		out := *it   // 锁内拷一份快照再返回，避免与后续刷新竞读
		c.mu.Unlock()
		return out.idx, out.src, out.signed, out.at, err
	}
	it.idx, it.src, it.signed, it.at, it.err = ix, from, signed, at, nil
	c.mu.Unlock()
	return ix, from, signed, at, nil
}

// Peek 只看缓存、不发网络（面板渲染「已安装插件是否有新版」这类不能阻塞在拉清单上）。
func (c *Cache) Peek(src Source) (*Index, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it := c.items[src.Key()]; it != nil && it.idx != nil {
		return it.idx, true
	}
	return nil, false
}

// State 返回某来源的缓存概览（面板展示「清单从哪拿的、多旧、是否验签、上次错误」）。
func (c *Cache) State(key string) (src string, at time.Time, signed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it := c.items[key]; it != nil {
		return it.src, it.at, it.signed, it.err
	}
	return "", time.Time{}, false, nil
}

// Invalidate 让下一次 Get 强制重拉（手动「刷新商店」）；key 为空则清全部。
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

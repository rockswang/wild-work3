// Package registry 是插件分发清单（registry）的读取与校验层（W3-38）。
//
// 三件事：
//  1. 源集合生成：清单/包一律按 `owner/repo + 资产名` 展开成「直连 + 各代理」多个 URL，
//     不用 api.github.com（实测未认证 403 限流：`API rate limit exceeded for <IP>`），
//     也不用 raw.githubusercontent.com / cdn.jsdelivr.net（实测国内直连 code 000 不可达）。
//  2. 信任根：只验签 registry 的 index.json（ed25519，公钥内置 gateway），
//     插件包由清单里的 sha256 背书（sha256 必填，见 store.Install）。
//     之所以不签每个插件包：那要求 N 个插件作者各自管密钥，比单一信任根复杂一个数量级。
//  3. 版本门：清单条目带 `protocol{min,max}` 与 `gateway{min,max}`，装前必过
//     （此前全仓无一处校验 meta.ProtocolRng，属 W3-38 补的洞）。
package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// GitHubRelease 资产定位：owner/repo + release 资产名。
// tag 留空走 `releases/latest/download/`（免 API、免知道版本号）；非空走 `releases/download/<tag>/`。
type GitHubRelease struct {
	OwnerRepo string `json:"repo"`  // 形如 wild-work3/ww3-registry
	Asset     string `json:"asset"` // 形如 index.json / ww3-workbuddy-ext.zip
	Tag       string `json:"tag,omitempty"`
}

// Direct 返回直连 URL。
func (g GitHubRelease) Direct() string {
	if g.Tag != "" {
		return "https://github.com/" + g.OwnerRepo + "/releases/download/" + g.Tag + "/" + g.Asset
	}
	return "https://github.com/" + g.OwnerRepo + "/releases/latest/download/" + g.Asset
}

// Sources 展开候选源：直连优先，其后每个代理拼 `<proxy><direct>`。
// 代理值允许写 `https://gh-proxy.com/` 或 `https://gh-proxy.com`（统一补尾斜杠）。
// 顺序只是「先试谁」的提示，实际由 fetch.go 并发竞速取第一个通过校验的。
func (g GitHubRelease) Sources(proxies []string) []string {
	out := []string{g.Direct()}
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		// 代理前缀本身必须是 http(s) URL，否则拼出来没人能连（防配置写错成裸域名）
		if u, err := url.Parse(p); err != nil || u.Scheme == "" {
			continue
		}
		out = append(out, p+g.Direct())
	}
	return out
}

// Hosts 返回该条目允许触碰的主机名集合（host allowlist）。
// 目的：清单里若被塞进 `http://evil/x.zip`，即便 sha256 自洽也不该下（同源校验风险）。
func Hosts(refs []GitHubRelease, proxies []string) map[string]bool {
	m := map[string]bool{}
	add := func(u string) {
		if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
			m[strings.ToLower(parsed.Hostname())] = true
		}
	}
	for _, r := range refs {
		for _, s := range r.Sources(proxies) {
			add(s)
		}
	}
	for _, s := range AbsoluteSources("", proxies) { // 代理本身的主机也要在表内
		add(s)
	}
	return m
}

// AbsoluteSources 把「一个绝对 URL」展开成候选源（直连 + 各代理前缀）。
// 清单条目写死 URL（非 repo+asset 形态）时走这条路，代理前缀同样适用。
func AbsoluteSources(base string, proxies []string) []string {
	var out []string
	if base != "" {
		out = append(out, base)
	}
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" || base == "" {
			continue
		}
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		if u, err := url.Parse(p); err != nil || u.Scheme == "" {
			continue
		}
		out = append(out, p+base)
	}
	return out
}

// ---------- 清单形状 ----------

// ProtocolRng 协议兼容区间（max=0 表示不限）。
type ProtocolRng struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// VersionGate 版本区间（字符串，形如 "1.0.0"；空表示不限）。
type VersionGate struct {
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
}

// AssetEntry 一个可下载安装的具体包。
type AssetEntry struct {
	Version   string          `json:"version"`
	Asset     string          `json:"asset,omitempty"` // 缺省用插件条目的 asset
	SHA256    string          `json:"sha256"`          // **必填**（W3-38：由可选改必填）
	Size      int64           `json:"size,omitempty"`
	OS        string          `json:"os,omitempty"`
	Arch      string          `json:"arch,omitempty"`
	Protocol  ProtocolRng     `json:"protocol,omitempty"`
	Gate      VersionGate     `json:"gateway,omitempty"`
	Changelog string          `json:"changelog,omitempty"`
	URL       string          `json:"url,omitempty"` // 允许绝对 URL（仍受 host allowlist 约束）
	Raw       json.RawMessage `json:"-"`
}

// Conformance 准入测试摘要（社区插件登记的「已验证」证据）。
type Conformance struct {
	Pass int    `json:"pass"`
	Warn int    `json:"warn"`
	Fail int    `json:"fail"`
	Mode string `json:"mode,omitempty"` // mock | live
	At   string `json:"at,omitempty"`
}

// PluginEntry 目录条目（身份/仓库/用途/能力/准入证据）。
//
// index.json 里可以只放**指针**（File+Digest），详情另存 `plugins/<id>.json`（W3-38：
// 每插件一个文件，更新互不动、并发 PR 不冲突、客户端只拉关心的条目）。
// 指针条目由 LoadPlugin 按需展开，File 的摘要由已验签的 index 背书。
type PluginEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind"` // channel | hook
	Channel     string `json:"channel,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Asset       string `json:"asset,omitempty"`
	File        string `json:"file,omitempty"`   // 详情文件（registry 仓内的 release 资产名）
	Digest      string `json:"digest,omitempty"` // File 内容的 sha256
	Icon        string `json:"icon,omitempty"`
	Color       string `json:"color,omitempty"`
	Author      string `json:"author,omitempty"`
	Status      string `json:"status,omitempty"` // verified | unverified | broken
	VerifiedAt  string `json:"verified_at,omitempty"`
	// Latest 索引里声明的最新版（指针条目的快捷提示）。
	// 指针本身不含 versions，商店列表不隔个请求去展开每个详情文件；
	// 发布 CI 写索引时顺手填这个字段，面板就能显示「最新 x.y.z」而不多打一次外网。
	Latest       string         `json:"latest,omitempty"`
	Conformance  *Conformance   `json:"conformance,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
	Versions     []AssetEntry   `json:"versions,omitempty"`
}

// IsPointer 该条目是否只是指针（详情在 File 里）。
func (p PluginEntry) IsPointer() bool { return p.File != "" }

// validateEntry 单条目的静态校验（ParseIndex / ParsePlugin 共用）。
func validateEntry(p PluginEntry, requireKind bool) error {
	if p.ID == "" {
		return fmt.Errorf("清单条目缺 id")
	}
	// requireKind：索引条目必须自带 kind；详情文件可以不写（kind 由索引背书，LoadPlugin 里回填）
	if requireKind && p.Kind != "channel" && p.Kind != "hook" {
		return fmt.Errorf("清单条目 %s 的 kind 非法: %q（只允许 channel|hook）", p.ID, p.Kind)
	}
	if p.File != "" && !isHexSHA(p.Digest) {
		return fmt.Errorf("清单条目 %s 是指针（file=%s）但 digest 缺失或非 64 位十六进制", p.ID, p.File)
	}
	for _, v := range p.Versions {
		if !isHexSHA(v.SHA256) {
			return fmt.Errorf("清单条目 %s %s 的 sha256 缺失或非 64 位十六进制（W3-38 起必填）", p.ID, v.Version)
		}
		if v.URL == "" && p.Repo == "" {
			return fmt.Errorf("清单条目 %s %s 既无 repo/asset 也无 url，无法定位包", p.ID, v.Version)
		}
		if v.URL == "" && p.Repo != "" && v.Asset == "" && p.Asset == "" {
			return fmt.Errorf("清单条目 %s %s 有 repo 但未给 asset 名", p.ID, v.Version)
		}
	}
	return nil
}

// ParsePlugin 解析单个插件详情文件（plugins/<id>.json）。
func ParsePlugin(b []byte) (*PluginEntry, error) {
	var p PluginEntry
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("插件详情解析失败: %w", err)
	}
	if p.File != "" {
		return nil, fmt.Errorf("插件详情 %s 又指了一个 file（一层指针到底，不许递归）", p.ID)
	}
	if err := validateEntry(p, false); err != nil {
		return nil, err
	}
	if len(p.Versions) == 0 {
		return nil, fmt.Errorf("插件详情 %s 没有任何版本条目", p.ID)
	}
	return &p, nil
}

// LoadPlugin 按需展开指针条目：取 File 资产 → 校 digest（由已验签的 index 背书）→ 解析。
// base 是 index.json 的定位（同一发布位置下换资产名；绝对 URL 形态按同目录解析，
// 这样社区清单「粘一个 URL」也能用逐插件详情文件的形态）。
func LoadPlugin(ctx context.Context, base Source, p PluginEntry, o Options) (PluginEntry, error) {
	if !p.IsPointer() {
		return p, nil
	}
	urls := IndexFrom(base, p.File).Sources(o.Proxies)
	if len(urls) == 0 {
		return p, fmt.Errorf("插件 %s 的详情文件 %s 无法定位", p.ID, p.File)
	}
	b, src, err := FetchURLs(ctx, urls, o)
	if err != nil {
		return p, fmt.Errorf("取插件详情 %s 失败: %w", p.File, err)
	}
	if sum := sha256Hex(b); !strings.EqualFold(sum, p.Digest) {
		return p, fmt.Errorf("插件详情 %s 摘要不符（源 %s）: 清单 %s 实际 %s", p.File, shortURL(src), p.Digest, sum)
	}
	det, err := ParsePlugin(b)
	if err != nil {
		return p, err
	}
	if det.ID != p.ID {
		return p, fmt.Errorf("插件详情 id 不符: 索引 %s 文件 %s", p.ID, det.ID)
	}
	// 索引里的身份字段优先（名字/图标是商店展示用的，详情文件可能被插件作者改动）
	if det.Kind == "" {
		det.Kind = p.Kind // kind 是拒装/运行形态的判据，只能以索引为准，缺了会被 validateEntry 打回
	}
	if det.Name == "" {
		det.Name = p.Name
	}
	if det.Icon == "" {
		det.Icon = p.Icon
	}
	if det.Color == "" {
		det.Color = p.Color
	}
	if det.Author == "" {
		det.Author = p.Author
	}
	if det.Conformance == nil {
		det.Conformance = p.Conformance
	}
	return *det, nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// refFor 该条目的包定位（绝对 URL 优先，否则 repo+asset 生成源）。
func (p PluginEntry) refFor(v AssetEntry) GitHubRelease {
	asset := v.Asset
	if asset == "" {
		asset = p.Asset
	}
	return GitHubRelease{OwnerRepo: p.Repo, Asset: asset}
}

// PackageSources 给出该版本包的全部候选源（绝对 URL 条目也能走代理前缀）。
func (p PluginEntry) PackageSources(v AssetEntry, proxies []string) []string {
	if v.URL != "" {
		return AbsoluteSources(v.URL, proxies)
	}
	return p.refFor(v).Sources(proxies)
}

// Index registry 仓的 index.json。
type Index struct {
	Version   int           `json:"version"`
	UpdatedAt string        `json:"updated_at,omitempty"`
	Gate      VersionGate   `json:"gateway,omitempty"` // 整份清单适用的 gateway 版本门
	Plugins   []PluginEntry `json:"plugins"`
	// Main 是 main 仓（gateway/core/官方样板）自身的更新条目，供 /gw/updates 用。
	Main *MainEntry `json:"main,omitempty"`
}

// PluginByID 按 id 取索引条目（未命中返回零值与 false）。
func (ix *Index) PluginByID(id string) (PluginEntry, bool) {
	for _, p := range ix.Plugins {
		if p.ID == id {
			return p, true
		}
	}
	return PluginEntry{}, false
}

// MainEntry main 仓 release 的 update.json 摘要。
type MainEntry struct {
	Version string       `json:"version"`
	Gate    VersionGate  `json:"gateway,omitempty"`
	Notes   string       `json:"notes,omitempty"`
	Assets  []AssetEntry `json:"assets,omitempty"`
	URL     string       `json:"url,omitempty"` // release 页面（手动替换时给用户的直链）
}

// CurrentAssetFor 取指定二进制在当前平台的包（name 形如 `ww3-core.exe`）。
// 匹配规则：资产名同名，或以去 .exe 后的词干开头（分平台 zip 形态 `ww3-core-windows-amd64.zip`）。
// 同一条目里取版本号最高的（update.json 会累积历史资产）。
func (m *MainEntry) CurrentAssetFor(name string) (AssetEntry, bool) {
	if m == nil {
		return AssetEntry{}, false
	}
	stem := strings.TrimSuffix(name, ".exe")
	var best *AssetEntry
	for i, a := range m.Assets {
		if a.OS != "" && a.OS != runtime.GOOS {
			continue
		}
		if a.Arch != "" && a.Arch != runtime.GOARCH {
			continue
		}
		base := filepath.Base(a.Asset)
		if base != name && !strings.HasPrefix(base, stem) {
			continue
		}
		if best == nil || cmpSemver(a.Version, best.Version) > 0 {
			best = &m.Assets[i]
		}
	}
	if best == nil {
		return AssetEntry{}, false
	}
	return *best, true
}

// ---------- 校验 ----------

// ErrGate 版本门不满足（可读中文，直接透给面板）。
type ErrGate struct{ Msg string }

func (e *ErrGate) Error() string { return e.Msg }

// CheckGate 校验协议区间与 gateway 版本门。protocolVer/gatewayVer 为当前生效值。
func CheckGate(p PluginEntry, v AssetEntry, protocolVer int, gatewayVer string) error {
	if v.Protocol.Min > 0 && protocolVer < v.Protocol.Min {
		return &ErrGate{Msg: fmt.Sprintf("插件 %s %s 需要 IPC 协议 ≥%d，当前为 %d", p.ID, v.Version, v.Protocol.Min, protocolVer)}
	}
	if v.Protocol.Max > 0 && protocolVer > v.Protocol.Max {
		return &ErrGate{Msg: fmt.Sprintf("插件 %s %s 只支持 IPC 协议 ≤%d，当前为 %d", p.ID, v.Version, v.Protocol.Max, protocolVer)}
	}
	if err := checkSemverGate(fmt.Sprintf("插件 %s %s 需要 gateway ≥%s，当前为 %s", p.ID, v.Version, v.Gate.Min, gatewayVer),
		v.Gate.Min, gatewayVer, true); err != nil {
		return err
	}
	if err := checkSemverGate(fmt.Sprintf("插件 %s %s 只支持 gateway ≤%s，当前为 %s", p.ID, v.Version, v.Gate.Max, gatewayVer),
		v.Gate.Max, gatewayVer, false); err != nil {
		return err
	}
	return nil
}

func checkSemverGate(msg, bound, ver string, isMin bool) error {
	if strings.TrimSpace(bound) == "" {
		return nil
	}
	if cmpSemver(ver, bound) < 0 == isMin {
		return &ErrGate{Msg: msg}
	}
	return nil
}

// ParseIndex 解析并静态校验 index.json（sha256 必填、id 唯一、kind 合法）。
func ParseIndex(b []byte) (*Index, error) {
	var ix Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, fmt.Errorf("清单解析失败: %w", err)
	}
	seen := map[string]bool{}
	for _, p := range ix.Plugins {
		if p.ID != "" && seen[p.ID] {
			return nil, fmt.Errorf("清单条目 id 重复: %s", p.ID)
		}
		seen[p.ID] = true
		if err := validateEntry(p, true); err != nil {
			return nil, err
		}
	}
	if ix.Version == 0 {
		ix.Version = 1 // 清单自身也有版本，缺省当 1 处理（不要求写）
	}
	return &ix, nil
}

// AllowHosts 构造下载主机 allowlist：github 及其 release 重定向落点 + 各代理主机 + 额外项。
//
// 目的（W3-38）：清单里若被塞进 `http://evil/x.zip`，即便 sha256 自洽也不该下。
// 注意只校**请求 URL** 的主机，不追重定向落点（github 会 302 到 objects.githubusercontent.com，
// 该主机已在表内）。
func AllowHosts(proxies []string, extra ...string) map[string]bool {
	m := map[string]bool{
		"github.com": true, "objects.githubusercontent.com": true,
		"codeload.github.com": true, "release-assets.githubusercontent.com": true,
	}
	for _, p := range AbsoluteSources("https://github.com/o/r/releases/latest/download/x", proxies) {
		if u, err := url.Parse(p); err == nil && u.Hostname() != "" {
			m[strings.ToLower(u.Hostname())] = true
		}
	}
	for _, e := range extra {
		if u, err := url.Parse(e); err == nil && u.Hostname() != "" {
			m[strings.ToLower(u.Hostname())] = true
			continue
		}
		m[strings.ToLower(strings.TrimSpace(e))] = true
	}
	return m
}

// ParseMain 解析 main 仓的 update.json。
func ParseMain(b []byte) (*MainEntry, error) {
	var m MainEntry
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("update.json 解析失败: %w", err)
	}
	for _, a := range m.Assets {
		if !isHexSHA(a.SHA256) {
			return nil, fmt.Errorf("update.json 资产 %s 的 sha256 缺失（必填）", a.Version)
		}
	}
	return &m, nil
}

// Verify 用内置公钥集合验签 payload（sigHex 是 ed25519 对 payload 原始字节的签名 hex）。
// 任一公钥验过即通过（留轮换余地）；全不匹配才失败。
func Verify(pubKeys []string, payload []byte, sigHex string) error {
	sig, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("清单签名格式错误（应为 128 位 hex）")
	}
	var lastErr error
	for _, k := range pubKeys {
		pk, err := hex.DecodeString(strings.TrimSpace(k))
		if err != nil || len(pk) != ed25519.PublicKeySize {
			lastErr = fmt.Errorf("内置公钥格式错误: %s", k)
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(pk), payload, sig) {
			return nil
		}
		lastErr = fmt.Errorf("签名与内容不匹配（清单可能被篡改或镜像返回了伪造内容）")
	}
	if len(pubKeys) == 0 {
		return fmt.Errorf("未配置任何 registry 公钥，拒绝采信清单")
	}
	return lastErr
}

func isHexSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// cmpSemver 比较两个版本号：按 . 分段数字比，非数字段按字典序；缺段视为 0。
// 只需支撑 "1.0.0" / "1.2" 这类自产版本，不实现完整 SemVer2 预发布语义。
func cmpSemver(a, b string) int {
	as := strings.Split(strings.TrimPrefix(strings.TrimSpace(a), "v"), ".")
	bs := strings.Split(strings.TrimPrefix(strings.TrimSpace(b), "v"), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		// 缺段一律当 0：否则 "1.0" 与 "1.0.0" 会被判成不等（尾段与非空段比字典序）
		x, y := "0", "0"
		if i < len(as) {
			x = strings.TrimSpace(as[i])
		}
		if i < len(bs) {
			y = strings.TrimSpace(bs[i])
		}
		if xi, e1 := strconv.Atoi(x); e1 == nil {
			if yi, e2 := strconv.Atoi(y); e2 == nil {
				if xi != yi {
					if xi < yi {
						return -1
					}
					return 1
				}
				continue
			}
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// CmpSemver 导出版本比较（面板/测试用）。
func CmpSemver(a, b string) int { return cmpSemver(a, b) }

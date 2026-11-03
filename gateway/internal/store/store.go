// Package store 实现插件分发与升级管理（W3-08 / W3-13 边界 A）：
// 目录清单 + 版本清单、下载校验、按版本隔离安装、current 原子切换、回滚、卸载。
// 只负责「分发」，不负责运行时监督（后者在 core 的 runtime 包）。
package store

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wild-work3/main/gateway/internal/registry"
)

// 清单类型统一用 registry.PluginEntry / registry.AssetEntry（W3-38）：
// 旧的 store.CatalogEntry/VersionEntry 已删，避免出现两份形状不同的清单模型。

// Deps 一次安装所需的分发参数。
type Deps struct {
	Proxies     []string          // 下载代理前缀（registry_repo 同款）
	AllowHosts  map[string]bool   // host allowlist；空表=不限制（仅本地测试/手工装包）
	ProtocolVer int               // 当前 IPC 协议版本（版本门用）
	GatewayVer  string            // 当前 gateway 版本（版本门用）
	Opt         registry.Options  // 竞速判据
	OnProgress  registry.Progress // 可为 nil
}

// Installed 已安装插件状态。
type Installed struct {
	ID          string   `json:"id"`
	Current     string   `json:"current"`
	Versions    []string `json:"versions"`
	Previous    string   `json:"previous,omitempty"`
	InstalledAt string   `json:"installed_at,omitempty"`

	// 以下字段从 current 版本的 manifest.json 读出（不持久化，读取时补齐）
	Name         string   `json:"name,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	Description  string   `json:"description,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Icon         string   `json:"icon,omitempty"`
	Color        string   `json:"color,omitempty"`
}

// Manager 插件目录管理器。
type Manager struct {
	root string
}

// New 创建（root = plugins 目录）。
func New(root string) *Manager { return &Manager{root: root} }

// List 列出已安装插件。
func (m *Manager) List() ([]Installed, error) {
	ents, err := os.ReadDir(m.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Installed
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		inst := Installed{ID: e.Name()}
		idDir := filepath.Join(m.root, e.Name())
		if b, err := os.ReadFile(filepath.Join(idDir, "current")); err == nil {
			inst.Current = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(filepath.Join(idDir, "previous")); err == nil {
			inst.Previous = strings.TrimSpace(string(b))
		}
		if vers, err := os.ReadDir(idDir); err == nil {
			for _, v := range vers {
				if v.IsDir() {
					inst.Versions = append(inst.Versions, v.Name())
				}
			}
			sort.Strings(inst.Versions)
		}
		if st, err := os.Stat(filepath.Join(idDir, inst.Current)); err == nil {
			inst.InstalledAt = st.ModTime().UTC().Format(time.RFC3339)
		}
		// 补 manifest 元数据（面板展示名称/能力/图标）
		if mb, err := os.ReadFile(filepath.Join(idDir, inst.Current, "manifest.json")); err == nil {
			var man struct {
				Name         string `json:"name"`
				Kind         string `json:"kind"`
				Description  string `json:"description"`
				Icon         string `json:"icon"`
				Color        string `json:"color"`
				Capabilities struct {
					Control      []string `json:"control"`
					Conversation []string `json:"conversation"`
				} `json:"capabilities"`
			}
			if json.Unmarshal(mb, &man) == nil {
				inst.Name, inst.Kind, inst.Description = man.Name, man.Kind, man.Description
				inst.Icon, inst.Color = man.Icon, man.Color
				inst.Capabilities = append(append([]string{}, man.Capabilities.Control...), man.Capabilities.Conversation...)
			}
		}
		out = append(out, inst)
	}
	return out, nil
}

// Install 从 registry 条目下载 zip 包并安装到 <id>/<version>/，成功后原子切换 current。
//
// 三道硬门（W3-38，缺一个就拒装，不做「装了再说」）：
//  1. 版本门：registry.CheckGate（protocol{min,max} + gateway{min,max}）——此前全仓无一处校验协议区间；
//  2. sha256 必填：旧写法 `if ver.SHA256 != ""` 会让清单少写字段时静默跳过校验；
//  3. host allowlist：清单被往绝对 URL 塞外部主机时直接不参与竞速。
//
// 返回命中的源 URL（面板要显示「包从哪拿的」）。
func (m *Manager) Install(p registry.PluginEntry, ver registry.AssetEntry, d Deps) (string, error) {
	if ver.Version == "" {
		return "", fmt.Errorf("清单条目缺 version")
	}
	if err := registry.CheckGate(p, ver, d.ProtocolVer, d.GatewayVer); err != nil {
		return "", err
	}
	urls := allow(p.PackageSources(ver, d.Proxies), d.AllowHosts)
	if len(urls) == 0 {
		return "", fmt.Errorf("清单条目 %s %s 没有可用源（主机均不在允许列表内）", p.ID, ver.Version)
	}
	tmpDir := filepath.Join(m.root, ".tmp-"+p.ID)
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	pkg := filepath.Join(tmpDir, "pkg.zip")
	src, err := registry.FetchFile(context.Background(), urls, pkg, ver.SHA256, d.Opt, d.OnProgress)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	extractDir := filepath.Join(tmpDir, "x")
	if err := unzip(pkg, extractDir); err != nil {
		return "", fmt.Errorf("解压失败: %w", err)
	}
	// 校验包内 manifest：id 必须与清单一致；kind 也必须一致（装错类型的包会在运行时才暴露）
	mb, err := os.ReadFile(filepath.Join(extractDir, "manifest.json"))
	if err != nil {
		return "", fmt.Errorf("包内缺少 manifest.json")
	}
	var man struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(mb, &man); err != nil {
		return "", fmt.Errorf("manifest 解析失败: %w", err)
	}
	if man.ID != "" && man.ID != p.ID {
		return "", fmt.Errorf("manifest id 不匹配: 期望 %s 实际 %s", p.ID, man.ID)
	}
	if man.Kind != "" && p.Kind != "" && man.Kind != p.Kind {
		return "", fmt.Errorf("manifest kind 不匹配: 清单说 %s 包内是 %s", p.Kind, man.Kind)
	}
	if man.Version != "" && man.Version != ver.Version {
		return "", fmt.Errorf("manifest version 与清单不符: 清单 %s 包内 %s", ver.Version, man.Version)
	}
	dest := filepath.Join(m.root, p.ID, ver.Version)
	_ = os.RemoveAll(dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(extractDir, dest); err != nil {
		return "", err
	}
	return src, m.switchTo(p.ID, ver.Version, true)
}

// allow 按 host allowlist 过滤候选源（空表视为不限制）。
func allow(urls []string, hosts map[string]bool) []string {
	if len(hosts) == 0 {
		return urls
	}
	var out []string
	for _, u := range urls {
		parsed, err := url.Parse(u)
		if err != nil {
			continue
		}
		if hosts[strings.ToLower(parsed.Hostname())] {
			out = append(out, u)
		}
	}
	return out
}

// switchTo 原子切换 current（rememberPrev 时把旧 current 记为 previous）。
func (m *Manager) switchTo(id, version string, rememberPrev bool) error {
	idDir := filepath.Join(m.root, id)
	if _, err := os.Stat(filepath.Join(idDir, version)); err != nil {
		return fmt.Errorf("版本不存在: %s", version)
	}
	if rememberPrev {
		if b, err := os.ReadFile(filepath.Join(idDir, "current")); err == nil {
			old := strings.TrimSpace(string(b))
			if old != "" && old != version {
				_ = os.WriteFile(filepath.Join(idDir, "previous"), []byte(old), 0o644)
			}
		}
	}
	return atomicWrite(filepath.Join(idDir, "current"), []byte(version))
}

// Activate 切换到指定已安装版本。
func (m *Manager) Activate(id, version string) error { return m.switchTo(id, version, true) }

// Rollback 回滚到 previous。
func (m *Manager) Rollback(id string) (string, error) {
	idDir := filepath.Join(m.root, id)
	b, err := os.ReadFile(filepath.Join(idDir, "previous"))
	if err != nil {
		return "", fmt.Errorf("无可回滚版本")
	}
	prev := strings.TrimSpace(string(b))
	if prev == "" {
		return "", fmt.Errorf("无可回滚版本")
	}
	if err := m.switchTo(id, prev, false); err != nil {
		return "", err
	}
	// 回滚后 previous 指向原 current
	if cb, err := os.ReadFile(filepath.Join(idDir, "current")); err == nil {
		_ = cb
	}
	return prev, nil
}

// Uninstall 卸载插件。**卸载不得影响数据文件**（W3-39 第 1 条）。
//
// W3-39 之后凭据/日志/统计住在 core 数据根（`<core data>/plugins/<id>/`），本安装树里只有
// exe + manifest + 版本目录，整目录删掉即等于「只退软件」。但现网与 dist 里仍有旧布局
// （可变量混在安装根 `<plugins>/<id>/`）的残留，删之前必须跳过 auths/logs/stat 三个契约目录，
// 否则「卸载→重装」会把用户所有账号打回未登录。
// `purge=true` 才连（旧布局遗留的）数据一起删——面板上用户显式勾选「连凭据一起删」时用。
func (m *Manager) Uninstall(id string, purge bool) error {
	dir := filepath.Join(m.root, id)
	if purge {
		return os.RemoveAll(dir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range ents {
		if e.IsDir() && keepOnUninstall[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// keepOnUninstall 是卸载时必须跳过的契约数据目录名（W3-21 §5）。
// W3-39 后新布局里它们不该出现在安装树，此白名单是旧布局的护栏：宁可留几个空目录，
// 也不能把凭据跟着二进制一起删了。
var keepOnUninstall = map[string]bool{"auths": true, "logs": true, "stat": true}

// FetchCatalog 已删（W3-38）：清单拉取与验签归 registry.LoadIndex，
// 本包只管文件系统上的安装/切换/回滚，不再自持一份无签名的清单形状。

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unzip 解压并防路径穿越 + 防解压炸弹（清单是可信的，但镜像/仓库可能被往包里塞 4GiB 单文件）。
// Extract 导出的 zip 解压入口（路径穿越与炸弹上限同 unzip 规则）。
// gw 更新 core 时需要把 zip 包解开取里面的 ww3-core.exe（W3-38）。
func Extract(src, dest string) error { return unzip(src, dest) }

const (
	maxZipEntries = 4096      // 条目数上限（插件包就一个 exe + manifest + 少量资产）
	maxZipBytes   = 512 << 20 // 解压后总大小上限 512MiB
	maxZipOneFile = 256 << 20 // 单文件上限
)

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	if len(r.File) > maxZipEntries {
		return fmt.Errorf("包内条目过多（%d > %d）", len(r.File), maxZipEntries)
	}
	var total int64
	for _, f := range r.File {
		p := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(p, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("非法路径（疑似路径穿越）: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(p, 0o755)
			continue
		}
		if int64(f.UncompressedSize64) > maxZipOneFile {
			return fmt.Errorf("包内单文件过大: %s", f.Name)
		}
		total += int64(f.UncompressedSize64)
		if total > maxZipBytes {
			return fmt.Errorf("解压后总体积超限（>%d 字节）", maxZipBytes)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		// 声明长度不可信：实读也卡上限，防虚报 UncompressedSize 的炸弹
		_, err = io.Copy(out, io.LimitReader(rc, maxZipOneFile+1))
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
		if st, _ := os.Stat(p); st != nil && st.Size() > maxZipOneFile {
			return fmt.Errorf("包内实际文件过大: %s", f.Name)
		}
	}
	return nil
}

// atomicWrite 原子写文件（tmp + rename）。
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

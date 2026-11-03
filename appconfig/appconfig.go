// Package appconfig 实现整个 ww3 的唯一配置文件 <data>/config.json。
//
// 为什么合并成一份（用户要求）：规则替换段与其它配置项必须遵循同一套「内建默认 + 用户覆盖」
// 语义，分散在 gateway.json / core/core.json 三处会让「默认值从哪来」无法回答。
//
// 分段划分（各段归属固定，互不越界）：
//
//	gateway  监听地址 / API Key / 管理口令（gateway 写）
//	core     渠道 / 账号 / 路由 / 冷却 / 兼容层 / 钩子（core 写）
//	rules    请求改写规则，键为规则 ID（gateway 写，core 只读）
//
// 默认值从哪来（W3-37 定案 3）：rules 段的**默认表内嵌在 core**（main/core/internal/ruleset），
// 本层只负责「读出用户覆盖段」与「把生效集渲染成 ID→对象」；gateway 不持有默认表，
// 它的 Pass A 规则表由启动/热重载时向 core 拉取 effective 得到（见 main/gateway reloadRules）。
// gateway/core 段的默认值仍由各自代码里的 defaults()/normalize() 按「键缺失才补默认」提供——
// 那边需要区分「键缺失」与「显式零值」，合并默认后这个信息就没了，故不在本层合并。
//
// 兼容（W3-37 定案 4）：ww3 未发布过任何版本，除 auths/ 凭据格式外无向前兼容义务，
// 故本包不再有 Migrate（旧 gateway.json / core/core.json 的搬运已删）。
//
// 并发模型：两个进程都可能写同一个文件，故所有写入都走 Update —— 跨进程文件锁内
// 读-改-写，且只替换自己那一段，避免 read-modify-write 丢更新。
package appconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"wild-work3/main/rules"
)

// 分段名（契约：三个段名固定，前端与文档都按此引用）。
const (
	SectionGateway = "gateway"
	SectionCore    = "core"
	SectionRules   = "rules"
)

// 跨进程文件锁参数：5ms 轮询、5s 超时；持锁进程被强杀（联调常见）时按 mtime 判陈旧。
const (
	lockRetry   = 5 * time.Millisecond
	lockTimeout = 5 * time.Second
	lockStale   = 30 * time.Second
)

// Rule 是 rules.Rule 的别名，避免调用方为一个类型多引一个包。
type Rule = rules.Rule

// lockBusy 判「锁被他人持有」（需继续重试）而不是「真 I/O 错误」。
//
// Windows 上对一个被别的进程持有的文件做 O_CREATE|O_EXCL 得到的是 ERROR_ACCESS_DENIED
// （映射为 os.ErrPermission）而非 EEXIST；早期只认 os.IsExist 会让 core 启动直接致命
// （实测 [core] 加载配置失败: open data\config.json.lock: Access is denied.）。
func lockBusy(err error) bool {
	return errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES)
}

// Path 返回统一配置文件路径（数据根目录下的 config.json）。
func Path(dataDir string) string { return filepath.Join(dataDir, "config.json") }

// File 是统一配置文件的访问器（无状态，可多 goroutine 共用）。
type File struct{ path string }

// Open 打开（不创建）指定路径的统一配置文件。
func Open(path string) *File { return &File{path: path} }

// OpenData 打开 <dataDir>/config.json。
func OpenData(dataDir string) *File { return Open(Path(dataDir)) }

// Path 返回文件路径。
func (f *File) Path() string { return f.path }

// readDoc 读整份文档；文件不存在返回空文档（不是错误：首次启动即此状态）。
func (f *File) readDoc() (map[string]json.RawMessage, error) {
	doc := map[string]json.RawMessage{}
	b, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return nil, err
	}
	// 坏 JSON 一律 fail closed：静默退回默认值会让用户以为配置生效了。
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s 解析失败: %w", f.path, err)
	}
	return doc, nil
}

// Section 返回用户文件里某段的原始 JSON（不含默认；段不存在返回 nil）。
//
// 不做默认合并是刻意的：调用方要用「键是否存在」区分「未配置」与「显式零值」。
func (f *File) Section(section string) (json.RawMessage, error) {
	doc, err := f.readDoc()
	if err != nil {
		return nil, err
	}
	return doc[section], nil
}

// Sections 返回文件里出现过的所有段名（供面板展示）。
func (f *File) Sections() ([]string, error) {
	doc, err := f.readDoc()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc))
	for k := range doc {
		out = append(out, k)
	}
	return out, nil
}

// RuleOverrides 返回 rules 段的用户覆盖原始对象（ID→字段），段不存在返回 nil。
//
// 不在本层合并默认是刻意的（W3-37 定案 3）：默认表属 core 的内嵌数据，
// gateway 只用 core 下发的生效集，避免新增上游指纹被迫给 gateway 发版。
func (f *File) RuleOverrides() (map[string]json.RawMessage, error) {
	doc, err := f.readDoc()
	if err != nil {
		return nil, err
	}
	return ruleOverrides(doc[SectionRules])
}

// WriteRules 把生效规则集渲染成 {"<id>": {...}} 形态（供面板展示/编辑与跨进程下发）。
func WriteRules(rs []Rule) (json.RawMessage, error) {
	return json.Marshal(map[string]any{SectionRules: RulesToMap(rs)})
}

// ruleOverrides 把 {"<id>": {...}} 形态的用户段解成 ID→原始对象。
func ruleOverrides(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var over map[string]json.RawMessage
	if err := json.Unmarshal(raw, &over); err != nil {
		return nil, fmt.Errorf("rules 段解析失败: %w", err)
	}
	return over, nil
}

// Update 在跨进程锁内读-改-写某一段（只替换该段，其它段原样保留）。
//
// fn 收到当前段的用户原始 JSON（可能为 nil），返回新段 JSON；返回空表示删除该段。
func (f *File) Update(section string, fn func(cur json.RawMessage) (json.RawMessage, error)) error {
	unlock, err := f.lock()
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := f.readDoc()
	if err != nil {
		return err
	}
	next, err := fn(doc[section])
	if err != nil {
		return err
	}
	if len(next) == 0 || string(next) == "null" {
		delete(doc, section)
	} else {
		doc[section] = next
	}
	return f.writeDoc(doc)
}

// writeDoc 原子落盘（tmp + rename，0600：配置里有 API Key 与管理口令）。
func (f *File) writeDoc(doc map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// lock 取跨进程文件锁（O_EXCL 创建锁文件，Windows 与 POSIX 行为一致）。
func (f *File) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return nil, err
	}
	lp := f.path + ".lock"
	deadline := time.Now().Add(lockTimeout)
	for {
		fh, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fh.WriteString(strconv.Itoa(os.Getpid()))
			_ = fh.Close()
			return func() { _ = os.Remove(lp) }, nil
		}
		if !lockBusy(err) {
			return nil, err
		}
		// 陈旧锁：持锁进程被强杀时不能永久阻塞（联调常态是 taskkill）。
		if st, serr := os.Stat(lp); serr == nil && time.Since(st.ModTime()) > lockStale {
			_ = os.Remove(lp)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("配置文件被占用（等锁超时）: " + lp)
		}
		time.Sleep(lockRetry)
	}
}

// RulesToMap 把规则列表转成 {"<id>": {...}} 形态（与用户段同形状，便于面板编辑与跨进程下发）。
func RulesToMap(rs []rules.Rule) map[string]any {
	out := make(map[string]any, len(rs))
	for _, r := range rs {
		m := map[string]any{"kind": string(r.Kind)}
		if r.JSONPath != "" {
			m["jsonpath"] = r.JSONPath
		}
		if r.Regex != "" {
			m["regex"] = r.Regex
		}
		if r.Replacement != "" {
			m["replacement"] = r.Replacement
		}
		if r.Anchor != "" {
			m["anchor"] = r.Anchor
		}
		sc := map[string]any{}
		if len(r.Scope.Channels) > 0 {
			sc["channels"] = r.Scope.Channels
		}
		if len(r.Scope.Roles) > 0 {
			sc["roles"] = r.Scope.Roles
		}
		if len(sc) > 0 {
			m["scope"] = sc
		}
		if r.Order != 0 {
			m["order"] = r.Order
		}
		out[r.ID] = m
	}
	return out
}

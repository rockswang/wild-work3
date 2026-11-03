// Package rules implements configurable request rewriting rules.
package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind identifies the matcher implementation.
type Kind string

const (
	KindRegex    Kind = "regex"
	KindJSONPath Kind = "jsonpath"
)

// Scope restricts a rule to channel instances and/or message roles. Empty lists mean unrestricted.
type Scope struct {
	Channels []string `json:"channels,omitempty"`
	Roles    []string `json:"roles,omitempty"`
}

// Rule is intentionally JSON-shaped so configuration can be merged without losing future fields.
// Regex replaces matching text. JSONPath selects string values, then applies Regex (or replaces the
// whole selected value when Regex is empty). Supported paths are $.field, ['field'], [N], and [*].
//
// Anchor 是可选的预检子串（ASCII 大小写不敏感）：只有字符串包含它才跑正则。
// 空 Anchor 表示对所有字符串生效。存在意义是性能：Codex 类客户端每轮重放整段历史，
// 内联截图让请求体轻易上 30MiB，十条正则逐串扫描实测 252ms（16MB/s），
// 而带 anchor 预检后无命中的大串只付一次 strings/byte 扫描（详见 rules_test.go 的基准）。
type Rule struct {
	ID          string `json:"id"`
	Kind        Kind   `json:"kind"`
	JSONPath    string `json:"jsonpath,omitempty"`
	Regex       string `json:"regex,omitempty"`
	Replacement string `json:"replacement,omitempty"`
	Anchor      string `json:"anchor,omitempty"`
	Scope       Scope  `json:"scope,omitempty"`
	Order       int    `json:"order,omitempty"`
}

// Context describes the routing state for one application.
type Context struct {
	ChannelID string
}

type compiledRule struct {
	Rule
	re    *regexp.Regexp
	path  []pathStep
	index int
}

// Engine is immutable after construction and safe for concurrent use.
type Engine struct{ rules []compiledRule }

// Merge combines defaults with overrides by ID. Missing IDs inherit, a non-empty object recursively
// overrides fields, an empty object deletes that ID, and a new ID adds a rule. Result order is stable.
func Merge(defaults []Rule, overrides map[string]json.RawMessage) ([]Rule, error) {
	order := make([]string, 0, len(defaults)+len(overrides))
	objects := make(map[string]map[string]any, len(defaults)+len(overrides))
	for _, r := range defaults {
		if strings.TrimSpace(r.ID) == "" {
			return nil, errors.New("rule id is required")
		}
		if _, exists := objects[r.ID]; exists {
			return nil, fmt.Errorf("duplicate rule id %q", r.ID)
		}
		m, err := toMap(r)
		if err != nil {
			return nil, err
		}
		objects[r.ID] = m
		order = append(order, r.ID)
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, id := range keys {
		var patch map[string]any
		if err := json.Unmarshal(overrides[id], &patch); err != nil {
			return nil, fmt.Errorf("override %q: %w", id, err)
		}
		if len(patch) == 0 {
			delete(objects, id)
			continue
		}
		if base, ok := objects[id]; ok {
			objects[id] = mergeMap(base, patch)
		} else {
			patch["id"] = id
			objects[id] = patch
			order = append(order, id)
		}
	}
	out := make([]Rule, 0, len(objects))
	for _, id := range order {
		m, ok := objects[id]
		if !ok {
			continue
		}
		m["id"] = id
		b, _ := json.Marshal(m)
		var r Rule
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("rule %q: %w", id, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(b, &m)
	return m, err
}
func mergeMap(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = mergeMap(bm, pm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// New validates and compiles rules. Rules are ordered by Order, then input position.
func New(rules []Rule) (*Engine, error) {
	seen := map[string]bool{}
	cs := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		if strings.TrimSpace(r.ID) == "" {
			return nil, errors.New("rule id is required")
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		c := compiledRule{Rule: r, index: i}
		var err error
		switch r.Kind {
		case KindRegex:
			if r.Regex == "" {
				return nil, fmt.Errorf("rule %q: regex is required", r.ID)
			}
			c.re, err = regexp.Compile(r.Regex)
		case KindJSONPath:
			c.path, err = parsePath(r.JSONPath)
			if err == nil && r.Regex != "" {
				c.re, err = regexp.Compile(r.Regex)
			}
		default:
			return nil, fmt.Errorf("rule %q: unsupported kind %q", r.ID, r.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.ID, err)
		}
		cs = append(cs, c)
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Order < cs[j].Order })
	return &Engine{rules: cs}, nil
}

// PreRoute applies only rules without channel scope.
func (e *Engine) PreRoute(body map[string]any) bool { return e.apply(body, Context{}, false) }

// PostRoute applies only rules with channel scope and matching ChannelID.
func (e *Engine) PostRoute(body map[string]any, ctx Context) bool { return e.apply(body, ctx, true) }

// HasChannelRules 报告是否存在作用于该渠道的 channel 作用域规则。
//
// 调用方（core）靠它决定要不要为 Pass B 付一次请求体 JSON 往返：
// 没有渠道规则时绝不能往返（30MiB 级 body 往返会白白慢几十毫秒）。
func (e *Engine) HasChannelRules(channelID string) bool {
	for _, r := range e.rules {
		if len(r.Scope.Channels) > 0 && contains(r.Scope.Channels, channelID) {
			return true
		}
	}
	return false
}

func (e *Engine) apply(body map[string]any, ctx Context, post bool) bool {
	changed := false
	for _, r := range e.rules {
		scoped := len(r.Scope.Channels) > 0
		if scoped != post || (post && !contains(r.Scope.Channels, ctx.ChannelID)) {
			continue
		}
		if r.Kind == KindRegex {
			anchor, re, repl := r.Anchor, r.re, r.Replacement
			f := func(s string) string {
				if anchor != "" && !containsFold(s, anchor) {
					return s // 预检不中：跳过正则（大 body 的唯一性能保障）
				}
				return re.ReplaceAllString(s, repl)
			}
			if walkStrings(body, "", r.Scope.Roles, f) {
				changed = true
			}
		} else if applyPath(body, r.path, r.Scope.Roles, r.re, r.Replacement) {
			changed = true
		}
	}
	return changed
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// containsFold ASCII 大小写不敏感子串查找（不做内存分配，避免 ToLower 复制大 body）。
// 先走 strings.Contains 的 SIMD 快路径（纯小写指纹几乎都在这里命中），未中再做逐字节折叠比较。
func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	if strings.Contains(s, sub) {
		return true
	}
	n := len(sub)
	if n > len(s) {
		return false
	}
	c0 := lowerASCII(sub[0])
	for i := 0; i+n <= len(s); i++ {
		if lowerASCII(s[i]) != c0 {
			continue
		}
		if equalFoldASCII(s[i:i+n], sub) {
			return true
		}
	}
	return false
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func equalFoldASCII(a, b string) bool {
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

// blobMinLen / blobPrefix：内联二进制载荷（data URL / 裸 base64）不做规则改写。
// base64 编码后指纹不再逐字出现，扫描它们纯属浪费；旧 chat.sanitizeContent 也只动
// text part、从不碰 image_url，故此跳过不改变既有语义。
const blobMinLen = 8192

func isBlobString(s string) bool {
	if len(s) <= blobMinLen {
		return false
	}
	if strings.HasPrefix(s, "data:") {
		return true
	}
	for i := 0; i < len(s) && i < 512; i++ {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/', c == '=', c == '-', c == '_', c == '\n', c == '\r':
		default:
			return false
		}
	}
	return true
}

func roleAllowed(roles []string, role string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, r := range roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}
func walkStrings(v any, role string, roles []string, f func(string) string) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		nr := role
		if s, ok := x["role"].(string); ok {
			nr = s
		}
		for k, vv := range x {
			if s, ok := vv.(string); ok && k != "role" && roleAllowed(roles, nr) {
				if isBlobString(s) {
					continue
				}
				if n := f(s); n != s {
					x[k] = n
					changed = true
				}
			} else if walkStrings(vv, nr, roles, f) {
				changed = true
			}
		}
	case []any:
		for _, vv := range x {
			if walkStrings(vv, role, roles, f) {
				changed = true
			}
		}
	}
	return changed
}

type pathStep struct {
	key      string
	index    int
	wildcard bool
	isIndex  bool
}

func parsePath(p string) ([]pathStep, error) {
	if p == "" || p[0] != '$' {
		return nil, errors.New("jsonpath must start with $")
	}
	var out []pathStep
	for i := 1; i < len(p); {
		switch p[i] {
		case '.':
			i++
			start := i
			for i < len(p) && p[i] != '.' && p[i] != '[' {
				i++
			}
			if start == i {
				return nil, errors.New("empty jsonpath field")
			}
			out = append(out, pathStep{key: p[start:i]})
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				return nil, errors.New("unterminated jsonpath bracket")
			}
			end += i
			tok := p[i+1 : end]
			if tok == "*" {
				out = append(out, pathStep{wildcard: true, isIndex: true})
			} else if len(tok) >= 2 && (tok[0] == '\'' && tok[len(tok)-1] == '\'' || tok[0] == '"' && tok[len(tok)-1] == '"') {
				out = append(out, pathStep{key: tok[1 : len(tok)-1]})
			} else {
				n, e := strconv.Atoi(tok)
				if e != nil || n < 0 {
					return nil, fmt.Errorf("invalid jsonpath index %q", tok)
				}
				out = append(out, pathStep{index: n, isIndex: true})
			}
			i = end + 1
		default:
			return nil, fmt.Errorf("invalid jsonpath at %q", p[i:])
		}
	}
	return out, nil
}
func applyPath(root any, path []pathStep, roles []string, re *regexp.Regexp, repl string) bool {
	return applyNode(root, path, "", roles, re, repl)
}
func applyNode(v any, path []pathStep, role string, roles []string, re *regexp.Regexp, repl string) bool {
	if len(path) == 0 {
		return false
	}
	changed := false
	s := path[0]
	rest := path[1:]
	if m, ok := v.(map[string]any); ok && !s.isIndex {
		nr := role
		if x, ok := m["role"].(string); ok {
			nr = x
		}
		child, ok := m[s.key]
		if !ok {
			return false
		}
		if len(rest) == 0 {
			if old, ok := child.(string); ok && roleAllowed(roles, nr) {
				n := repl
				if re != nil {
					n = re.ReplaceAllString(old, repl)
				}
				if n != old {
					m[s.key] = n
					return true
				}
			}
			return false
		}
		return applyNode(child, rest, nr, roles, re, repl)
	}
	if a, ok := v.([]any); ok && s.isIndex {
		if s.wildcard {
			for _, child := range a {
				if applyNode(child, rest, role, roles, re, repl) {
					changed = true
				}
			}
		} else if s.index < len(a) {
			if len(rest) == 0 {
				if old, ok := a[s.index].(string); ok && roleAllowed(roles, role) {
					n := repl
					if re != nil {
						n = re.ReplaceAllString(old, repl)
					}
					if n != old {
						a[s.index] = n
						changed = true
					}
				}
			} else {
				changed = applyNode(a[s.index], rest, role, roles, re, repl)
			}
		}
	}
	return changed
}

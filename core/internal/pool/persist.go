// 账号池状态持久化（旧版 state.json 等价物）。
//
// 不持久化的后果是「重启即遗忘冷却」：刚把全池账号打到 402/限流，重启 gateway 或 core 后
// 第一波请求又会原样撞一遍上游，既浪费额度又容易触发风控（W3-20 起插件无常驻，
// core 重启频率比旧版高，这个缺口比旧版更疼）。
package pool

import (
	"encoding/json"
	"os"
	"sort"
	"time"
)

// persisted 状态文件结构。字段直接复用 Entry（导出字段即 JSON 键），
// 身份字段（nickname/credits）以 config.Accounts 为准，恢复时只回填运行时状态。
type persisted struct {
	SavedAt time.Time `json:"saved_at"`
	Entries []Entry   `json:"entries"`
}

// SetPersist 指定状态文件并立即加载。path 为空 = 关闭持久化（单测、CLI 只读模式）。
// 文件缺失/损坏都不报错：宁可从空池开始，也不能因为一个状态文件让 core 起不来。
func (p *Pool) SetPersist(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = path
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var st persisted
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	now := p.now()
	for _, e := range st.Entries {
		if e.UID == "" {
			continue
		}
		if e.ModelCool == nil {
			e.ModelCool = map[string]time.Time{}
		}
		for m, u := range e.ModelCool { // 过期的模型级冷却不复活
			if !u.After(now) {
				delete(e.ModelCool, m)
			}
		}
		// 终止态（人工停用/登录态失效）与时钟无关，原样恢复；时间性冷却已过期则清零
		if e.Cool != CoolDisable && e.Cool != CoolDead && !e.CoolUntil.After(now) {
			e.Cool, e.CoolUntil, e.LastReason = CoolNone, time.Time{}, ""
		}
		cur, ok := p.entries[e.UID]
		if !ok {
			cp := e
			if cp.ModelCool == nil {
				cp.ModelCool = map[string]time.Time{}
			}
			p.entries[e.UID] = &cp
			continue
		}
		cur.Cool, cur.CoolUntil, cur.ModelCool, cur.ErrCount, cur.LastReason =
			e.Cool, e.CoolUntil, e.ModelCool, e.ErrCount, e.LastReason
	}
	return nil
}

// saveLocked 原子落盘（调用方持锁）。写失败静默忽略：状态文件是加速器不是真相源，
// 丢一次冷却记录不应让请求链路失败。
func (p *Pool) saveLocked() {
	if p.path == "" {
		return
	}
	st := persisted{SavedAt: p.now(), Entries: make([]Entry, 0, len(p.entries))}
	for _, e := range p.entries {
		st.Entries = append(st.Entries, *e)
	}
	sort.Slice(st.Entries, func(i, j int) bool { return st.Entries[i].UID < st.Entries[j].UID })
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	if os.Rename(tmp, p.path) != nil {
		os.Remove(tmp) // 不留垃圾（同 stat trimJSONL 的教训）
	}
}

package api

// 规则段管理端点（W3-35 单一 config.json 的 rules 分段）。
//
// 定位（W3-37 定案 3）：默认规则表内嵌在 core（main/rules/ruleset），core 是规则权威。
// 本端点同时服务两件事：
//   - 面板做「默认规则 / 用户覆盖 / 生效结果」三栏对照；
//   - **gateway 的 Pass A 规则表从这里拉**（effective 字段）。gateway 不持有默认表，
//     新增上游指纹只发 core 即可生效，不必给（低频冻结的）gateway 发版。
//
// 执行位置不搬进 core 的理由（实测）：core 拿到的是 chat.GenerateRequest 结构体，
// 要跑规则得多做一趟 struct↔map JSON 往返，33MB 请求 +334ms/+162MB（temp/bench_rules_roundtrip_test.go）；
// 而 gateway 入站本来就是 map，就地改零往返。
//
// 语义（与 core/internal/config.Store.Rules 一致）：
//   - 用户段按规则 ID 合并默认集：给对象 = 覆盖同名字段；给 {} = 删除该默认规则；
//     新 ID = 追加；给 null / 空 = 回到全默认。
//   - 写前先 Merge + New 编译校验，坏规则（正则不合法、路径不支持、ID 缺失）当场报错不落盘。

import (
	"encoding/json"
	"io"
	"net/http"

	"wild-work3/main/rules"
	"wild-work3/main/rules/ruleset"
)

// rulesView 返回生效规则与默认规则两份。
//
// 为什么两份都给：面板要能显示「这条是默认的 / 这条被用户改过 / 这条被删了」，
// 只给生效结果无法区分。默认集内嵌在二进制里，不会随用户配置变化。
func (s *Server) rulesView(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodGet:
		eff, err := s.Store.RulesJSON()
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{
			"ok":        true,
			"effective": eff,
			"defaults":  json.RawMessage(ruleset.DefaultRulesJSON()),
		})
		return nil
	case http.MethodPut:
		b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			return err
		}
		if err := s.Store.SetRules(b); err != nil {
			return err
		}
		// 重建引擎挂到 Router（Pass B 立即生效；在途请求用旧引擎跑完）。
		s.reloadRules()
		eff, err := s.Store.RulesJSON()
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"ok": true, "effective": eff})
		return nil
	}
	writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
	return nil
}

// reloadRules 重建 Pass B 规则引擎。失败时保留旧引擎（宁可规则不生效，也不要整条链路无规则）。
func (s *Server) reloadRules() {
	if s.Router == nil {
		return
	}
	rs, err := s.Store.Rules()
	if err != nil {
		return
	}
	if eng, err := rules.New(rs); err == nil {
		s.Router.Rules = eng
	}
}

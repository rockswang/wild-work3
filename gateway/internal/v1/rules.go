// rules.go 规则引擎在 gateway 侧（Pass A：channel 无关规则）的挂载点。
//
// 分层（W3-37 定案 3，修正 W3-35 的「默认表内嵌在主包、两端各自合并」）：
//   - **规则权威在 core**：默认表内嵌在 main/rules/ruleset（core 链接），用户段在
//     config.json 的 rules 段按 ID 覆盖，core 合并后由 GET /core/config/rules 下发 effective。
//   - **Pass A 执行留在 gateway**：入站三协议已收敛成 map[string]any，就地改写零往返；
//     搬进 core 要为每个请求多做一趟 struct↔map（实测 33MB body +334ms/+162MB）。
//   - **Pass B**（依赖最终渠道的规则）仍由 core 在每个候选渠道调用前施加。
//
// 因此 gateway 不 import ruleset、不持有默认表：新增上游指纹只发 core，gateway 不改代码不发版。
// 边界由 main/gateway/deps_test.go 用 go list -deps 断言。
package v1

import (
	"encoding/json"
	"sync/atomic"

	"wild-work3/main/rules"
)

// activeRules 当前生效规则引擎（nil = 未加载，Pass A 跳过）。
var activeRules atomic.Pointer[rules.Engine]

// Rules 取当前规则引擎（可能为 nil）。
func Rules() *rules.Engine { return activeRules.Load() }

// SetRules 设置规则引擎；传 nil 表示停用 Pass A。
//
// 允许热更新：core 侧改了规则后 gateway 重新拉取、重编译引擎再调本函数，在途请求用旧引擎跑完即可。
func SetRules(e *rules.Engine) { activeRules.Store(e) }

// Effective 是 GET /core/config/rules 的响应里 gateway 关心的部分。
type Effective struct {
	OK        bool            `json:"ok"`
	Effective json.RawMessage `json:"effective"` // {"rules":{"<id>":{...}}}
}

// SetRulesFromEffective 从 core 下发的生效规则 JSON 编译引擎并生效，返回条数。
//
// effective 已是「默认 + 用户覆盖」的结果，这里只解析不合并（Merge(nil, …) 等于按 Order 稳定排序）。
// 失败时保留原引擎并返回 error（调用方决定是记日志还是致命）。
func SetRulesFromEffective(raw []byte) (int, error) {
	var env Effective
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, err
	}
	if len(env.Effective) == 0 {
		return 0, errNoEffective
	}
	rs, err := rules.ParseRules(env.Effective)
	if err != nil {
		return 0, err
	}
	e, err := rules.New(rs)
	if err != nil {
		return 0, err
	}
	SetRules(e)
	return len(rs), nil
}

// errNoEffective core 返回体里没有 effective 段（版本不符或 core 未就绪）。
var errNoEffective = rulesErr("core 未下发 effective 规则表")

type rulesErr string

func (e rulesErr) Error() string { return string(e) }

package rules

import (
	"encoding/json"
	"fmt"
)

// ParseRules 解析 {"rules": {"<id>": {...}}} 形态的规则文件。
//
// 与 Merge 的 overrides 同一形状：键即规则 ID，空对象表示删除（用于用户配置）。
// 返回值按 Order、再按文件中的书写顺序稳定排序，便于 diff 与测试。
// 默认表本身在 main/rules/ruleset（W3-37 定案 3：gateway 不链接默认表）。
func ParseRules(b []byte) ([]Rule, error) {
	var f struct {
		Rules map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("规则文件解析失败: %w", err)
	}
	if len(f.Rules) == 0 {
		return nil, nil
	}
	return Merge(nil, f.Rules)
}

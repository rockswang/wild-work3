// Package ruleset 内建 ww3 的**默认规则表**（上游指纹改写与结构归一的缺省值）。
//
// 为什么单独成包（W3-37 定案 3）：默认表是「随上游文案变」的快变数据，而 gateway 发布后
// 要求低频冻结（只走安全修复与全包替换）。若表内嵌在 main/rules（gateway 为拿引擎也必须链接的包），
// 新增一条指纹就被迫给 gateway 发版。放到独立包后：
//   - **core** 链接本包（core 可自动更新），把「默认 + 用户覆盖」的生效集通过
//     GET /core/config/rules 下发；
//   - **gateway** 不 import 本包、不持有默认表，只做 Pass A 执行（规则表来自 core 下发）。
//     边界由 deps_test.go 用 go list -deps 断言，防止以后有人图省事在 gateway 里直接引用。
//
// 为什么不放进 main/core/internal：协议层的等价性回归（protocol/chat/rules_equiv_test.go）
// 与本包的生成脚本都要引用默认表，internal 会挡住跨模块引用。
package ruleset

import (
	_ "embed"

	"wild-work3/main/rules"
)

// defaultRulesJSON 是默认规则配置文件。
//
// 用配置文件而不是 Go 字面量的原因：用户 config.json 的 rules 段按 ID 覆盖默认规则，
// 两边形状一致才好维护；且这些字面量含上游指纹，经任何工具输出都会被中间层改写
// （陷阱 29），必须由 temp/gen_rules_json.py 从 chat/sanitize.go 程序化生成、
// 再用 temp/verify_defaults.py 比对 sha256，不允许手抄。
//
//go:embed default_rules.json
var defaultRulesJSON []byte

// Defaults 是内建规则集（声明形态，可被用户 rules 段按 ID 增/删/改覆盖）。
//
// 分两类：
//   - 结构归一（role.developer）：只改 role 值，不做文本替换；
//   - 出站脱敏（sanitize.*）：剥离上游内容审核黑名单指纹。客户端（Claude Code 类 CLI）
//     在 system prompt 注入若干固定模板句，上游按逐字精确匹配拦截（非语义审核），
//     一字改动即可绕过。策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写。
//
// 语义要点：
//   - 身份句匹配串不带结尾标点，兼容 CLI 版句号收尾与桌面版逗号接续；
//   - header 键值形态整段删除，残留裸键名做最小缩写（header→hdr），两者正则不可合并；
//   - 11-128 是上游反探测码，出现在请求体里即拦截，改写不可避免；
//   - DSH 两句为上游按 system 开头逐字锚定的串，插入虚词即绕过。
//
// 三条剥离规则带 Anchor 预检（性能必需）：内联截图的请求体常上 30MiB，
// 无预检时每条正则都要全串扫一遍（实测十条规则 252ms / 16MB/s）。
//
// 与旧 chat.SanitizeText 的行为差异：规则管线不做 strings.TrimSpace（那会静默改动用户文本），
// 其余等价，见 protocol/chat/rules_equiv_test.go 的等价性用例。
var Defaults = mustParse()

// DefaultRulesJSON 返回默认规则的原始 JSON（供面板「默认 / 用户覆盖 / 生效」三栏对照）。
func DefaultRulesJSON() []byte { return defaultRulesJSON }

func mustParse() []rules.Rule {
	rs, err := rules.ParseRules(defaultRulesJSON)
	if err != nil {
		panic("ruleset: 默认规则文件损坏: " + err.Error())
	}
	return rs
}

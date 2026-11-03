package main

// gateway 的依赖边界断言（W3-37 定案 1/3）。
//
// gateway 发布后按「全包手动升级」收敛，所以它不得链接任何随上游指纹/前端变化的数据包：
//   - wild-work3/main/rules/ruleset —— 内嵌 default_rules.json（上游指纹表），归 core；
//     gateway 只消费 core 下发的生效规则表（v1.SetRulesFromEffective）。
//   - webview/systray 之外的 GUI 依赖不在本断言范围（托盘是 gateway 的既有职责，W3-23）。
// 用 go list 现算依赖闭包，比翻 import 语句可靠（间接导入也算）。

import (
	"os/exec"
	"strings"
	"testing"
)

// depClosure 用 go list 现算依赖闭包（间接导入也算），比翻 import 语句可靠。
func depClosure(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

func TestGatewayDoesNotLinkRulesDefaults(t *testing.T) {
	banned := "wild-work3/main/rules/ruleset"
	for _, dep := range depClosure(t, ".") {
		if dep == banned {
			t.Fatalf("gateway 链接了 %s：默认规则表是快变数据，必须只由 core 内嵌并下发（W3-37 定案 3）", banned)
		}
	}
}

// TestGatewayLinksRulesEngine 反向确认：引擎本身是稳定代码，gateway 需要它在入站 map 上跑 Pass A。
func TestGatewayLinksRulesEngine(t *testing.T) {
	want := "wild-work3/main/rules"
	for _, dep := range depClosure(t, ".") {
		if dep == want {
			return
		}
	}
	t.Fatalf("gateway 应链接规则引擎 %s（Pass A 执行位置）", want)
}

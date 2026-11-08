package channel

import (
	"encoding/json"
	"strings"
	"testing"
)

// 自定义模型走「实例由用户自建」形态（W3-44）：不预置占位渠道，面板按 multi_channels
// 出一个入口 + Provider 列表。占位渠道会让 dist 里凭空多一个空配置渠道（面板显示不可用）。
func TestManifestDeclaresUserOwnedProviders(t *testing.T) {
	m := Manifest()
	if !m.MultiChannels {
		t.Fatal("自定义模型必须声明 multi_channels=true（实例由用户自建，可任意多个）")
	}
	if len(m.DefaultChannels) != 0 {
		t.Fatalf("不应预置占位渠道: %+v", m.DefaultChannels)
	}
	// 账号页入口按钮文案直接取 Name，沿用旧版叫法
	if m.Name != "自定义模型" {
		t.Fatalf("Name 应为面板入口文案，实际 %q", m.Name)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["multi_channels"] != true {
		t.Fatalf("manifest 线上格式需带 multi_channels: %s", b)
	}
	if _, ok := wire["default_channels"]; ok {
		t.Fatalf("multi_channels 插件不该序列化 default_channels: %s", b)
	}
	// 实例 ID 就是模型命名空间前缀，其余必填项得让面板收齐
	req, _ := configSchema()["required"].([]string)
	have := strings.Join(req, ",")
	for _, k := range []string{"base_url", "api_key"} {
		if !strings.Contains(have, k) {
			t.Fatalf("config.schema required 缺 %s: %s", k, have)
		}
	}
}

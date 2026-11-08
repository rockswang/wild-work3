package main

import (
	"path/filepath"
	"testing"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/runtime"
	"wild-work3/main/protocol/meta"
)

func TestSeedDefaultChannelsPreservesExistingAndReinstall(t *testing.T) {
	st, err := config.OpenFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(t.TempDir(), t.TempDir())
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "one", Kind: "channel", DefaultChannels: []meta.DefaultChannel{{ID: "one", Name: "Initial", Config: map[string]any{"region": "default"}}, {ID: "", Name: "invalid"}}}})
	if err := seedDefaultChannels(st, rt); err != nil {
		t.Fatal(err)
	}
	ch := st.Channel("one")
	if ch == nil || ch.PluginID != "one" || ch.DisplayName != "Initial" || !ch.Enabled || ch.Config["region"] != "default" {
		t.Fatalf("seed: %+v", ch)
	}
	if err := st.Update(func(c *config.Config) error {
		c.Channels[0].DisplayName = "Edited"
		c.Channels[0].Enabled = false
		c.Channels[0].Config = map[string]any{"region": "personal"}
		c.Accounts = append(c.Accounts, &config.Account{UID: "saved", ChannelID: "one", LoginKind: "oauth"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate uninstall: the binary is gone but its config/account must survive.
	empty := runtime.New(t.TempDir(), t.TempDir())
	if err := seedDefaultChannels(st, empty); err != nil {
		t.Fatal(err)
	}
	if err := seedDefaultChannels(st, rt); err != nil {
		t.Fatal(err)
	}
	got := st.Get()
	if len(got.Channels) != 1 || len(got.Accounts) != 1 || got.Accounts[0].UID != "saved" {
		t.Fatalf("lost persisted data: %+v", got)
	}
	ch = got.Channels[0]
	if ch.DisplayName != "Edited" || ch.Enabled || ch.Config["region"] != "personal" {
		t.Fatalf("overwrote user settings: %+v", ch)
	}
}

// 内建渠道被用户改名后，重启不得按 def.ID 重建一份（W3-45）。
//
// 不存 DefaultID 印记时的表现：改名后配置里已无 def.ID，seed 判为「缺失」→ 同一个上游
// 凭空多出一个渠道实例，用户会看到两个按钮、两份账号，且新实例的配置是空的。
func TestSeedDefaultChannelsRespectsRename(t *testing.T) {
	st, err := config.OpenFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(t.TempDir(), t.TempDir())
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "one", Kind: "channel",
		DefaultChannels: []meta.DefaultChannel{{ID: "one", Name: "初始"}}}})
	if err := seedDefaultChannels(st, rt); err != nil {
		t.Fatal(err)
	}
	// 用户改名（模拟 PUT 改名分支：配置改 ID，DefaultID 原样保留）
	if err := st.Update(func(c *config.Config) error {
		c.Channels[0].ID = "one-renamed"
		c.Channels[0].DisplayName = "改过名"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 重启：再 seed 一次
	if err := seedDefaultChannels(st, rt); err != nil {
		t.Fatal(err)
	}
	got := st.Get()
	if len(got.Channels) != 1 || got.Channels[0].ID != "one-renamed" || got.Channels[0].DisplayName != "改过名" {
		t.Fatalf("改名后被重建: %+v", got.Channels)
	}
	if got.Channels[0].DefaultID != "one" {
		t.Fatalf("default 印记丢失: %+v", got.Channels[0])
	}
	// 自建实例（无 DefaultID）不受影响：另一个声明了同名 def.ID 的插件仍可正常预置
	rt2 := runtime.New(t.TempDir(), t.TempDir())
	rt2.Register(&runtime.Plugin{Met: meta.Manifest{ID: "two", Kind: "channel",
		DefaultChannels: []meta.DefaultChannel{{ID: "two", Name: "第二个"}}}})
	if err := seedDefaultChannels(st, rt2); err != nil {
		t.Fatal(err)
	}
	if n := len(st.Get().Channels); n != 2 {
		t.Fatalf("另一个插件的预置被跳过: %+v", st.Get().Channels)
	}
}

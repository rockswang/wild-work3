package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/protocol/cli"
	"wild-work3/main/protocol/meta"
)

// Exercise the real on-disk manifest discovery and core startup path without ever
// invoking the fixture executable (or contacting an upstream service).
func TestDefaultChannelsDiscoverUninstallReinstall(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "core")
	pluginsDir := filepath.Join(root, "plugins")
	installDir := filepath.Join(pluginsDir, "workbuddy", "1.0.0")
	manifest := meta.Manifest{
		ID: "workbuddy", Name: "WorkBuddy", Version: "1.0.0", Kind: "channel",
		DefaultChannels: []meta.DefaultChannel{
			{ID: "workbuddy-personal", Name: "Personal", Config: map[string]any{"region": "default"}},
			{ID: "workbuddy-team", Name: "Team", Config: map[string]any{"region": "team"}},
		},
	}
	install := func() {
		t.Helper()
		if err := os.MkdirAll(installDir, 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(installDir, "manifest.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		// Discovery requires a binary; this inert fixture must never be run.
		if err := os.WriteFile(filepath.Join(installDir, "workbuddy-ext"), []byte("not an executable"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	start := func() *application {
		t.Helper()
		log, err := cli.NewLogger("test", filepath.Join(root, "core.log"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { log.Close() })
		a := &application{dataDir: dataDir, pluginsDir: pluginsDir, log: log}
		if err := a.init(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.led.Close() })
		return a
	}
	get := func(a *application, path string, dst any) {
		t.Helper()
		r := httptest.NewRecorder()
		a.srv.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", path, r.Code, r.Body.String())
		}
		if err := json.Unmarshal(r.Body.Bytes(), dst); err != nil {
			t.Fatalf("GET %s: decode %s: %v", path, r.Body.String(), err)
		}
	}
	visible := func(a *application, wantChannels, wantAccounts []string) {
		t.Helper()
		var channels []struct {
			ID string `json:"channel_id"`
		}
		get(a, "/api/channels", &channels)
		ids := make([]string, 0, len(channels))
		for _, ch := range channels {
			ids = append(ids, ch.ID)
		}
		sort.Strings(ids)
		if !reflect.DeepEqual(ids, wantChannels) {
			t.Fatalf("GET /api/channels: got %v, want %v", ids, wantChannels)
		}
		var accounts []struct {
			UID string `json:"uid"`
		}
		get(a, "/api/accounts", &accounts)
		uids := make([]string, 0, len(accounts))
		for _, ac := range accounts {
			uids = append(uids, ac.UID)
		}
		sort.Strings(uids)
		if !reflect.DeepEqual(uids, wantAccounts) {
			t.Fatalf("GET /api/accounts: got %v, want %v", uids, wantAccounts)
		}
		var state struct {
			Channels []json.RawMessage `json:"channels"`
			Accounts []json.RawMessage `json:"accounts"`
		}
		get(a, "/api/state", &state)
		if len(state.Channels) != len(wantChannels) || len(state.Accounts) != len(wantAccounts) {
			t.Fatalf("GET /api/state: channels=%d accounts=%d, want %d/%d", len(state.Channels), len(state.Accounts), len(wantChannels), len(wantAccounts))
		}
	}
	checkSaved := func(a *application) {
		t.Helper()
		cfg := a.store.Get()
		if len(cfg.Channels) != 2 || len(cfg.Accounts) != 3 {
			t.Fatalf("persisted channels/accounts lost or duplicated: %d/%d", len(cfg.Channels), len(cfg.Accounts))
		}
		personal := a.store.Channel("workbuddy-personal")
		team := a.store.Channel("workbuddy-team")
		if personal == nil || personal.PluginID != "workbuddy" || personal.DisplayName != "My WorkBuddy" || personal.Enabled || personal.Config["region"] != "custom" ||
			team == nil || team.PluginID != "workbuddy" || team.DisplayName != "Team" || !team.Enabled || team.Config["region"] != "team" {
			t.Fatalf("user/default settings lost: personal=%+v team=%+v", personal, team)
		}
		got := map[string]*config.Account{}
		for _, ac := range cfg.Accounts {
			got[ac.UID] = ac
		}
		for _, spec := range []struct{ uid, remote, channel, nickname string }{
			{"alice@workbuddy-personal", "alice", "workbuddy-personal", "Alice"},
			{"bob@workbuddy-personal", "bob", "workbuddy-personal", "Bob"},
			{"carol@workbuddy-team", "carol", "workbuddy-team", "Carol"},
		} {
			ac := got[spec.uid]
			if ac == nil || ac.RemoteUID != spec.remote || ac.ChannelID != spec.channel || ac.Nickname != spec.nickname {
				t.Fatalf("account %s lost/changed: %+v", spec.uid, ac)
			}
		}
	}

	install()
	first := start()
	if first.rt.Plugin("workbuddy") == nil || len(first.store.Get().Channels) != 2 || len(first.store.Get().Accounts) != 0 {
		t.Fatalf("initial discovery/seed: plugins=%v channels=%+v accounts=%+v", first.rt.Plugins(), first.store.Get().Channels, first.store.Get().Accounts)
	}
	for _, def := range manifest.DefaultChannels {
		ch := first.store.Channel(def.ID)
		if ch == nil || ch.PluginID != manifest.ID || ch.DisplayName != def.Name || !ch.Enabled || !reflect.DeepEqual(ch.Config, def.Config) {
			t.Fatalf("initial default %s: %+v", def.ID, ch)
		}
	}
	visible(first, []string{"workbuddy-personal", "workbuddy-team"}, []string{})
	if err := first.store.Update(func(c *config.Config) error {
		c.Channels[0].DisplayName = "My WorkBuddy"
		c.Channels[0].Enabled = false
		c.Channels[0].Config = map[string]any{"region": "custom"}
		for _, spec := range []struct{ remote, channel, nickname string }{
			{"alice", "workbuddy-personal", "Alice"},
			{"bob", "workbuddy-personal", "Bob"},
			{"carol", "workbuddy-team", "Carol"},
		} {
			c.Accounts = append(c.Accounts, &config.Account{
				UID: spec.remote + "@" + spec.channel, RemoteUID: spec.remote,
				ChannelID: spec.channel, Nickname: spec.nickname, LoginKind: "oauth",
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(pluginsDir, "workbuddy")); err != nil {
		t.Fatal(err)
	}
	uninstalled := start() // fresh runtime simulates a core restart after gateway uninstall
	if uninstalled.rt.Plugin("workbuddy") != nil {
		t.Fatal("uninstalled plugin rediscovered")
	}
	checkSaved(uninstalled)
	visible(uninstalled, []string{}, []string{})

	install()
	reinstalled := start() // reinstall and restart must reuse the original config and accounts
	if reinstalled.rt.Plugin("workbuddy") == nil {
		t.Fatal("reinstalled plugin not discovered")
	}
	checkSaved(reinstalled)
	visible(reinstalled, []string{"workbuddy-personal", "workbuddy-team"}, []string{"alice@workbuddy-personal", "bob@workbuddy-personal", "carol@workbuddy-team"})
}

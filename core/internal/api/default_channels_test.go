package api

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/pool"
	"wild-work3/main/core/internal/runtime"
	"wild-work3/main/protocol/meta"
)

func TestChannelPostVirtualAccountOnlyWithoutAuthBegin(t *testing.T) {
	st, err := config.OpenFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(t.TempDir(), t.TempDir())
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "oauth", Kind: "channel", Capabilities: meta.Capabilities{Control: []string{"auth.begin"}}}})
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "key", Kind: "channel"}})
	s := &Server{Store: st, RT: rt, Pool: pool.New(time.Second, time.Second)}
	for _, id := range []string{"oauth", "key"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/channels", strings.NewReader(`{"channel_id":"`+id+`","plugin_id":"`+id+`"}`)))
		if rec.Code != 200 {
			t.Fatalf("POST %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	got := st.Get()
	if len(got.Accounts) != 1 || got.Accounts[0].ChannelID != "key" || len(s.Pool.List()) != 1 {
		t.Fatalf("unexpected virtual accounts: %+v", got.Accounts)
	}
}

func TestDeleteChannelRemovesAllAccounts(t *testing.T) {
	st, err := config.OpenFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *config.Config) error {
		c.Channels = append(c.Channels, &config.ChannelConfig{ID: "ch", PluginID: "plugin", Enabled: true})
		for _, uid := range []string{"alice", "bob", "carol"} {
			c.Accounts = append(c.Accounts, &config.Account{UID: uid, ChannelID: "ch"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(t.TempDir(), t.TempDir())
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "plugin", Kind: "channel"}})
	p := pool.New(time.Second, time.Second)
	for _, uid := range []string{"alice", "bob", "carol"} {
		p.Upsert(pool.Entry{UID: uid, ChannelID: "ch"})
	}
	s := &Server{Store: st, RT: rt, Pool: p}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/channels/ch", nil))
	if rec.Code != 200 || len(st.Get().Accounts) != 0 || len(st.Get().Channels) != 0 || len(p.List()) != 0 {
		t.Fatalf("DELETE: status=%d channels=%+v accounts=%+v pool=%+v", rec.Code, st.Get().Channels, st.Get().Accounts, p.List())
	}
}

func TestUninstalledPluginHiddenFromChannelAndAccountViews(t *testing.T) {
	st, err := config.OpenFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *config.Config) error {
		for _, id := range []string{"installed", "removed"} {
			c.Channels = append(c.Channels, &config.ChannelConfig{ID: id, PluginID: id, Enabled: true})
			c.Accounts = append(c.Accounts, &config.Account{UID: id, ChannelID: id})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(t.TempDir(), t.TempDir())
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "installed", Kind: "channel"}})
	s := &Server{Store: st, RT: rt, Pool: pool.New(time.Second, time.Second)}
	for _, path := range []string{"/api/channels", "/api/accounts", "/api/state"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), `"removed"`) || !strings.Contains(rec.Body.String(), `"installed"`) {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	got := st.Get()
	if len(got.Channels) != 2 || len(got.Accounts) != 2 {
		t.Fatalf("uninstall erased data: %+v", got)
	}
	rt.Register(&runtime.Plugin{Met: meta.Manifest{ID: "removed", Kind: "channel"}})
	if len(s.channelsView()) != 2 || len(s.accountsView("")) != 2 {
		t.Fatal("reinstall did not restore views")
	}
}

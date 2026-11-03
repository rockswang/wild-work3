package rules

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMergeSemanticsAndStableOrder(t *testing.T) {
	d := []Rule{{ID: "a", Kind: KindRegex, Regex: "a", Replacement: "A", Scope: Scope{Channels: []string{"old"}}}, {ID: "b", Kind: KindRegex, Regex: "b", Replacement: "B"}}
	o := map[string]json.RawMessage{"a": json.RawMessage(`{"replacement":"AA","scope":{"roles":["user"]}}`), "b": json.RawMessage(`{}`), "z": json.RawMessage(`{"kind":"regex","regex":"z","replacement":"Z"}`), "c": json.RawMessage(`{"kind":"regex","regex":"c","replacement":"C"}`)}
	got, err := Merge(d, o)
	if err != nil {
		t.Fatal(err)
	}
	if ids := []string{got[0].ID, got[1].ID, got[2].ID}; !reflect.DeepEqual(ids, []string{"a", "c", "z"}) {
		t.Fatalf("order=%v", ids)
	}
	if got[0].Regex != "a" || got[0].Replacement != "AA" || !reflect.DeepEqual(got[0].Scope.Channels, []string{"old"}) || !reflect.DeepEqual(got[0].Scope.Roles, []string{"user"}) {
		t.Fatalf("recursive merge: %#v", got[0])
	}
}

func TestPrePostScopeRoleAndOrdering(t *testing.T) {
	e, err := New([]Rule{
		{ID: "late", Kind: KindRegex, Regex: "B", Replacement: "C", Order: 2, Scope: Scope{Roles: []string{"user"}}},
		{ID: "early", Kind: KindRegex, Regex: "A", Replacement: "B", Order: 1, Scope: Scope{Roles: []string{"user"}}},
		{ID: "channel", Kind: KindRegex, Regex: "C", Replacement: "D", Scope: Scope{Channels: []string{"ch1"}, Roles: []string{"user"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "A"}, map[string]any{"role": "assistant", "content": "A"}}}
	if !e.PreRoute(body) {
		t.Fatal("pre unchanged")
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "C" || msgs[1].(map[string]any)["content"] != "A" {
		t.Fatalf("roles/order: %#v", msgs)
	}
	if e.PostRoute(body, Context{ChannelID: "other"}) {
		t.Fatal("wrong channel applied")
	}
	if !e.PostRoute(body, Context{ChannelID: "ch1"}) || msgs[0].(map[string]any)["content"] != "D" {
		t.Fatalf("post: %#v", msgs)
	}
}

func TestRestrictedJSONPath(t *testing.T) {
	e, err := New([]Rule{{ID: "p", Kind: KindJSONPath, JSONPath: `$.messages[*]['content']`, Regex: `foo(\d+)`, Replacement: `bar$1`, Scope: Scope{Roles: []string{"system"}}}})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "foo12"}, map[string]any{"role": "user", "content": "foo13"}}}
	if !e.PreRoute(body) {
		t.Fatal("unchanged")
	}
	m := body["messages"].([]any)
	if m[0].(map[string]any)["content"] != "bar12" || m[1].(map[string]any)["content"] != "foo13" {
		t.Fatalf("%#v", m)
	}
	if _, err = New([]Rule{{ID: "bad", Kind: KindJSONPath, JSONPath: "$..x"}}); err == nil {
		t.Fatal("recursive descent accepted")
	}
}

func TestJSONPathWholeReplacementAndIndex(t *testing.T) {
	e, err := New([]Rule{{ID: "x", Kind: KindJSONPath, JSONPath: "$.items[1]", Replacement: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	b := map[string]any{"items": []any{"old0", "old1"}}
	if !e.PreRoute(b) || b["items"].([]any)[1] != "new" {
		t.Fatalf("%#v", b)
	}
}

func TestValidation(t *testing.T) {
	cases := [][]Rule{{{Kind: KindRegex, Regex: "x"}}, {{ID: "x", Kind: "nope"}}, {{ID: "x", Kind: KindRegex, Regex: "["}}, {{ID: "x", Kind: KindJSONPath, JSONPath: "messages"}}, {{ID: "x", Kind: KindRegex, Regex: "x"}, {ID: "x", Kind: KindRegex, Regex: "y"}}}
	for i, c := range cases {
		if _, err := New(c); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

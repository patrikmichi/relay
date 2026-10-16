package agentsync

import (
	"strings"
	"testing"
)

func TestSetJSONMember_KeepsCommentsAndOtherMembers(t *testing.T) {
	doc := "{\n  // my editor settings\n  \"editor.fontSize\": 14,\n  \"chat.plugins.marketplaces\": [\n    \"https://old.example/x.git\" // keep\n  ],\n}\n"
	got, err := SetJSONMember([]byte(doc), nil, "chat.plugins.marketplaces", []string{"https://old.example/x.git", "https://gw.example/m/acme/git/stable.git"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"// my editor settings", "\"editor.fontSize\": 14", "https://gw.example/m/acme/git/stable.git"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("result lacks %q:\n%s", want, got)
		}
	}
	v, found, err := ReadJSONValue(got, nil, "chat.plugins.marketplaces")
	if err != nil || !found || len(v.([]any)) != 2 {
		t.Errorf("read back = %v, %v, %v", v, found, err)
	}
}

func TestSetJSONMember_CreatesNestedObjectsAndDeletes(t *testing.T) {
	got, err := SetJSONMember(nil, []string{"extraKnownMarketplaces"}, "zuano-acme", map[string]any{
		"source": map[string]any{"source": "url", "url": "https://gw.example/m/acme/claude/stable/marketplace.json"}, "headersHelper": "zuano auth header --json",
	})
	if err != nil {
		t.Fatal(err)
	}
	v, found, _ := ReadJSONValue(got, []string{"extraKnownMarketplaces", "zuano-acme"}, "headersHelper")
	if !found || v != "zuano auth header --json" {
		t.Fatalf("created = %s", got)
	}
	got, err = SetJSONMember(got, []string{"enabledPlugins"}, "toolkit@zuano-acme", true)
	if err != nil {
		t.Fatal(err)
	}
	got, err = DeleteJSONMember(got, []string{"extraKnownMarketplaces"}, "zuano-acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := ReadJSONValue(got, []string{"extraKnownMarketplaces"}, "zuano-acme"); found {
		t.Errorf("not deleted: %s", got)
	}
	if v, _, _ := ReadJSONValue(got, []string{"enabledPlugins"}, "toolkit@zuano-acme"); v != true {
		t.Errorf("sibling lost: %s", got)
	}
	if _, err := SetJSONMember([]byte(`{"a": 1`), nil, "b", 2); err == nil {
		t.Error("a broken document was edited")
	}
	if _, err := SetJSONMember([]byte(`{"a": 1}`), []string{"a"}, "b", 2); err == nil {
		t.Error("a path through a non-object was accepted")
	}
}

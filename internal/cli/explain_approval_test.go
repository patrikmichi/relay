package cli

import (
	"encoding/json"
	"testing"
)

func TestExplain_NamesWhatAnAgentNeeds(t *testing.T) {
	isolate(t)
	srv := newRiskServer(t)
	var served riskCatalog
	if err := json.Unmarshal(riskFixture(t), &served); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"read": "", "write": "grant", "destructive": "approval", "outward": "approval"}
	for _, row := range served.Tools {
		got := runExplain(t, "call", row.Service, row.Tool, "--json", "--gateway-url", srv.URL)
		if got.Approval != want[row.Risk] {
			t.Errorf("%s.%s (%s): approval = %q, want %q", row.Service, row.Tool, row.Risk, got.Approval, want[row.Risk])
		}
	}
	if got := runExplain(t, "call", "nosuch", "tool", "--json", "--gateway-url", srv.URL); got.Approval != "" {
		t.Errorf("an unlisted tool has no known requirement, got %q", got.Approval)
	}
}

func TestExplainResult_TextNamesTheRequirement(t *testing.T) {
	cat := &riskCatalog{Tools: []riskEntry{
		{Service: "a", Tool: "destroy", Risk: "destructive"},
		{Service: "a", Tool: "send", Risk: "outward"},
		{Service: "a", Tool: "update", Risk: "write"},
		{Service: "a", Tool: "list", Risk: "read"},
	}}
	for tool, want := range map[string]string{
		"destroy": "a destroy: destructive -> ask (approval)",
		"send":    "a send: outward -> ask (approval)",
		"update":  "a update: write -> ask (grant)",
		"list":    "a list: read -> allow",
		"missing": "a missing: unknown -> ask",
	} {
		if got := explainCall(cat, "a", tool).String(); got != want {
			t.Errorf("%s: got %q, want %q", tool, got, want)
		}
	}
}

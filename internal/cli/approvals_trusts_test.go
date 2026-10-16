package cli

import (
	"net/http"
	"strings"
	"testing"
)

func TestApprovalsTrusts(t *testing.T) {
	var scopes []string
	gw := a2aGateway(t, routeFns{
		"GET /api/approvals/session-grants": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			scopes = append(scopes, r.URL.Query().Get("scope"))
			jsonReply(200, `{"grants":[{"id":"sgr_1","client":"Claude Code","session":"s1","tool":"slack.post_message","constraint":"channel C0123","createdBy":"patrik","expiresAt":"2026-10-16T14:00:00Z","uses":3},{"id":"sgr_2","client":"Cursor","class":"write","createdBy":"ana","expiresAt":"2026-10-16T13:30:00Z","uses":0}]}`)(nil, w, r)
		},
		"DELETE /api/approvals/session-grants/sgr_1":    jsonReply(200, `{}`),
		"POST /api/approvals/session-grants/revoke-all": jsonReply(200, `{"revoked":2}`),
	})
	out, err := runCommand(t, ApprovalsCmd(), "trusts", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, want := range []string{"sgr_1", "Claude Code", "slack.post_message", "channel C0123", "all write calls", "patrik", "2026-10-16T14:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if _, err := runCommand(t, ApprovalsCmd(), "trusts", "list", "--org", "--json"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(scopes, ",") != "mine,org" {
		t.Errorf("scopes = %v", scopes)
	}
	if out, err := runCommand(t, ApprovalsCmd(), "trusts", "revoke", "sgr_1"); err != nil || !strings.Contains(out, "Ended sgr_1") {
		t.Errorf("revoke: %v\n%s", err, out)
	}
	if _, err := runCommand(t, ApprovalsCmd(), "trusts", "revoke", "a/b"); err == nil {
		t.Error("a path as an id")
	}
	if _, err := runCommand(t, ApprovalsCmd(), "trusts", "revoke-all"); err == nil {
		t.Error("revoke-all without --yes")
	}
	if out, err := runCommand(t, ApprovalsCmd(), "trusts", "revoke-all", "--yes"); err != nil || !strings.Contains(out, "Ended 2 trust(s)") {
		t.Errorf("revoke-all: %v\n%s", err, out)
	}
	if _, err := runCommand(t, ApprovalsCmd(), "trusts", "revoke-all", "--yes", "--org"); err != nil || gw.count("POST /api/approvals/session-grants/revoke-all") != 2 {
		t.Errorf("revoke-all --org: %v", err)
	}
	// Ending a trust only tightens, so an agent session may.
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, ApprovalsCmd(), "trusts", "revoke", "sgr_1"); err != nil {
		t.Errorf("an agent ending a trust: %v", err)
	}
	for _, sub := range ApprovalsCmd().Commands() {
		if sub.Name() == "trusts" {
			for _, c := range sub.Commands() {
				if strings.Contains(c.Name(), "create") || strings.Contains(c.Name(), "add") || strings.Contains(c.Name(), "grant") {
					t.Errorf("there must be no command that creates a trust: %s", c.Name())
				}
			}
		}
	}
}

func TestApprovalsTrustsOrgNeedsAnAdministrator(t *testing.T) {
	a2aGateway(t, routeFns{"GET /api/approvals/session-grants": jsonReply(403, `{"error":"forbidden","message":"Only an administrator lists the organisation's trusts."}`)})
	if _, err := runCommand(t, ApprovalsCmd(), "trusts", "list", "--org"); exitCodeOf(err) != exitDenied || !strings.Contains(err.Error(), "administrator") {
		t.Errorf("exit %d (%v)", exitCodeOf(err), err)
	}
}

package cli

import (
	"net/url"
	"strings"
	"testing"
)

const feedTokenBody = `{"token":"fdt_example","expires_in":3600}`

func TestAuthHeader_PrintsALineOrJSON(t *testing.T) {
	approvalsGateway(t, handlers{"POST /api/cli/feed-token": reply(200, feedTokenBody)})
	out, err := runCommand(t, AuthCmd(), "header")
	if err != nil || out != "Authorization: Bearer fdt_example\n" {
		t.Fatalf("line: %q, %v", out, err)
	}
	out, err = runCommand(t, AuthCmd(), "header", "--json")
	if err != nil || strings.TrimSpace(out) != `{"Authorization":"Bearer fdt_example"}` {
		t.Fatalf("json: %q, %v", out, err)
	}
}

func TestAuthHeader_AGatewayWithoutTheFeatureIsAnError(t *testing.T) {
	approvalsGateway(t, handlers{"POST /api/cli/feed-token": reply(402, `{"error":"plan_required","message":"private feeds need Pro","feature":"plugins.private_feeds","upgrade_url":"https://gw.example/billing"}`)})
	_, err := runCommand(t, AuthCmd(), "header")
	if got := exitCodeOf(err); got != 30 || !strings.Contains(err.Error(), "https://gw.example/billing") {
		t.Fatalf("exit %d (%v)", got, err)
	}
}

func TestGitCredential_AnswersOnlyForTheGateway(t *testing.T) {
	srv, _ := approvalsGateway(t, handlers{"POST /api/cli/feed-token": reply(200, feedTokenBody)})
	u, _ := url.Parse(srv.URL)

	out, err := runWithInput(t, AuthCmd(), "protocol="+u.Scheme+"\nhost="+u.Host+"\npath=m/acme/git/docs.git\n\n", "git-credential", "get")
	if err != nil || out != "username=zuano\npassword=fdt_example\n" {
		t.Fatalf("gateway: %q, %v", out, err)
	}
	for name, in := range map[string]string{
		"other host":  "protocol=" + u.Scheme + "\nhost=github.com\n\n",
		"other proto": "protocol=ssh\nhost=" + u.Host + "\n\n",
		"other path":  "protocol=" + u.Scheme + "\nhost=" + u.Host + "\npath=api/x\n\n",
		"empty":       "",
	} {
		if out, err := runWithInput(t, AuthCmd(), in, "git-credential", "get"); err != nil || out != "" {
			t.Errorf("%s: %q, %v", name, out, err)
		}
	}
	for _, op := range []string{"store", "erase"} {
		if out, err := runWithInput(t, AuthCmd(), "protocol=https\nhost=x\nusername=a\npassword=b\n\n", "git-credential", op); err != nil || out != "" {
			t.Errorf("%s: %q, %v", op, out, err)
		}
	}
	if _, err := runWithInput(t, AuthCmd(), "", "git-credential", "frobnicate"); err == nil {
		t.Error("an unknown operation was accepted")
	}
}

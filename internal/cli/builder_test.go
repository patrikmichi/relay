package cli

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
)

// runWithStdin runs a command with the given standard input.
func runWithStdin(t *testing.T, cmd *cobra.Command, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), err
}

// Built at run time so no secret-shaped literal sits in the source.
func fakeSecret(name string) string { return name + "-" + strings.Repeat("q7", 8) }

func jsonFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const acJSON = `{"id":"ac_1","projectId":"","service":"github","name":"Our app","type":"custom","scheme":"oauth2","credentialSource":"config","scopes":["repo","read:org"],"redirectHost":"platform","shareWithProjects":false,"isDefault":true,"enabled":true,"secretFields":[{"key":"client_id","set":true,"updatedAt":"2026-10-16","hint":"Iv1."},{"key":"client_secret","set":false}]}`

func TestAuthConfigsReadCommands(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/v1/auth-configs":                       jsonReply(200, `{"items":[`+acJSON+`,{"id":"ac_2","projectId":"prj_9","service":"slack","name":"Slack","type":"managed","scheme":"oauth2","enabled":false}],"nextCursor":"n2"}`),
		"GET /api/v1/auth-configs/ac_1":                  jsonReply(200, `{"authConfig":`+acJSON+`}`),
		"GET /api/v1/services/github/auth-config-fields": jsonReply(200, `{"scheme":"oauth2","fields":[{"key":"client_id","label":"Client ID","required":true,"secret":false,"kind":"string"},{"key":"client_secret","label":"Client secret","required":true,"secret":true,"kind":"string"}],"scopes":{"available":["repo","read:org"]},"redirectUri":"https://app.example/api/v1/connect/callback","managedAvailable":true}`),
	})
	out, err := runCommand(t, AuthConfigsCmd(), "list", "--service", "github", "--search", "app", "--project", "prj_9", "--show-disabled", "--limit", "50", "--cursor", "n1")
	if err != nil || !strings.Contains(out, "ac_1") || !strings.Contains(out, "custom/oauth2") || !strings.Contains(out, "prj_9") || !strings.Contains(out, "off") || !strings.Contains(out, "More: --cursor n2") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if out, err = runCommand(t, AuthConfigsCmd(), "list", "--json"); err != nil || !strings.Contains(out, `"items"`) {
		t.Errorf("list --json: %v", err)
	}
	out, err = runCommand(t, AuthConfigsCmd(), "show", "ac_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ac_1  Our app (github)", "type custom, scheme oauth2", "scopes: repo read:org", "secret client_id: set 2026-10-16 (Iv1....)", "secret client_secret: not set"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, AuthConfigsCmd(), "fields", "github", "--scheme", "oauth2", "--required-only")
	if err != nil || !strings.Contains(out, "client_secret") || !strings.Contains(out, "required secret") || !strings.Contains(out, "Register this redirect URI") || !strings.Contains(out, "managed app available: true") {
		t.Errorf("fields: %v\n%s", err, out)
	}
	_ = gw
	for _, bad := range [][]string{{"show", "x"}, {"show", "ac_1/x"}, {"fields", "a/b"}} {
		if _, err := runCommand(t, AuthConfigsCmd(), bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestAuthConfigsCreateSendsSecretsAndNeverEchoesThem(t *testing.T) {
	gw := a2aGateway(t, routeFns{"POST /api/v1/auth-configs": jsonReply(201, `{"authConfig":`+acJSON+`}`)})
	secret := fakeSecret("client")
	creds := `{"client_id":"abc","client_secret":"` + secret + `"}`
	out, err := runCommand(t, AuthConfigsCmd(), "create", "--service", "github", "--name", "Our app", "--type", "custom", "--scheme", "oauth2", "--scopes", "repo,read:org", "--credential-source", "config", "--redirect-host", "platform", "--project", "prj_9", "--share-with-projects", "--default", "--from-file", jsonFile(t, creds))
	if err != nil || strings.Contains(out, secret) || !strings.Contains(out, "Created ac_1") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	b := gw.body("POST /api/v1/auth-configs", 0)
	sec, _ := b["secrets"].(map[string]any)
	if b["service"] != "github" || b["projectId"] != "prj_9" || b["shareWithProjects"] != true || b["isDefault"] != true || sec["client_secret"] != secret {
		t.Errorf("body = %v", b)
	}
	// Standard input needs its flag.
	out, err = runWithStdin(t, AuthConfigsCmd(), creds, "create", "--service", "github", "--name", "n2", "--type", "custom", "--scheme", "api_key", "--secrets-stdin")
	if err != nil || strings.Contains(out, secret) {
		t.Errorf("stdin: %v\n%s", err, out)
	}
	if sec, _ := gw.body("POST /api/v1/auth-configs", 1)["secrets"].(map[string]any); sec["client_secret"] != secret {
		t.Error("the credentials on standard input were not sent")
	}
	// A managed config takes none, and a custom one with connect-time fields may take none.
	if _, err := runCommand(t, AuthConfigsCmd(), "create", "--service", "slack", "--name", "m", "--type", "managed", "--scheme", "oauth2"); err != nil {
		t.Errorf("managed: %v", err)
	}
	if _, ok := gw.body("POST /api/v1/auth-configs", 2)["secrets"]; ok {
		t.Error("a managed config sent secrets")
	}
	for name, args := range map[string][]string{
		"missing flags": {"create", "--service", "x"},
		"bad type":      {"create", "--service", "x", "--name", "n", "--type", "both", "--scheme", "oauth2"},
		"bad json":      {"create", "--service", "x", "--name", "n", "--type", "custom", "--scheme", "oauth2", "--from-file", jsonFile(t, "[1]")},
		"missing file":  {"create", "--service", "x", "--name", "n", "--type", "custom", "--scheme", "oauth2", "--from-file", filepath.Join(t.TempDir(), "none")},
	} {
		if _, err := runCommand(t, AuthConfigsCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := runWithStdin(t, AuthConfigsCmd(), "", "create", "--service", "x", "--name", "n", "--type", "custom", "--scheme", "oauth2", "--secrets-stdin"); err == nil {
		t.Error("empty standard input")
	}
}

func TestAuthConfigsSecretsAreWriteOnlyAndRefusedForAgents(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"PUT /api/v1/auth-configs/ac_1/secrets":         jsonReply(200, `{}`),
		"POST /api/v1/auth-configs/ac_1/secrets:rotate": jsonReply(200, `{}`),
		"PATCH /api/v1/auth-configs/ac_1":               jsonReply(200, `{}`),
		"POST /api/v1/auth-configs/ac_1:disable":        jsonReply(200, `{}`),
		"POST /api/v1/auth-configs/ac_1:enable":         jsonReply(200, `{}`),
		"DELETE /api/v1/auth-configs/ac_1":              jsonReply(200, `{}`),
	})
	secret := fakeSecret("new")
	out, err := runWithStdin(t, AuthConfigsCmd(), `{"client_secret":"`+secret+`"}`, "secrets", "set", "ac_1")
	if err != nil || strings.Contains(out, secret) || !strings.Contains(out, "Updated client_secret on ac_1 (values not shown)") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if f, _ := gw.body("PUT /api/v1/auth-configs/ac_1/secrets", 0)["fields"].(map[string]any); f["client_secret"] != secret {
		t.Error("the value was not sent")
	}
	if _, err := runWithStdin(t, AuthConfigsCmd(), `{"client_secret":"`+secret+`"}`, "secrets", "rotate", "ac_1", "--grace", "48h"); err != nil {
		t.Fatal(err)
	}
	if b := gw.body("POST /api/v1/auth-configs/ac_1/secrets:rotate", 0); b["graceSeconds"] != float64(48*3600) {
		t.Errorf("rotate body = %v", b)
	}
	for _, grace := range []string{"1s", "200h", "soon"} {
		if _, err := runWithStdin(t, AuthConfigsCmd(), `{"a":"b"}`, "secrets", "rotate", "ac_1", "--grace", grace); err == nil {
			t.Errorf("grace %q accepted", grace)
		}
	}
	if _, err := runCommand(t, AuthConfigsCmd(), "secrets", "set", "ac_1"); err == nil {
		t.Error("secrets with nothing piped and no file must not read the terminal")
	}

	if out, err := runCommand(t, AuthConfigsCmd(), "update", "ac_1", "--name", "Renamed", "--scopes", "repo", "--default=false", "--share-with-projects", "--redirect-host", "whitelabel"); err != nil || !strings.Contains(out, "Updated ac_1") {
		t.Errorf("update: %v", err)
	}
	if b := gw.body("PATCH /api/v1/auth-configs/ac_1", 0); b["name"] != "Renamed" || b["isDefault"] != false || b["shareWithProjects"] != true || b["redirectHost"] != "whitelabel" {
		t.Errorf("update body = %v", b)
	}
	if _, err := runCommand(t, AuthConfigsCmd(), "update", "ac_1"); err == nil {
		t.Error("update with nothing to change")
	}
	if _, err := runCommand(t, AuthConfigsCmd(), "disable", "ac_1", "--suspend-accounts"); err != nil || gw.body("POST /api/v1/auth-configs/ac_1:disable", 0)["suspendAccounts"] != true {
		t.Errorf("disable: %v", err)
	}
	if out, err := runCommand(t, AuthConfigsCmd(), "enable", "ac_1"); err != nil || !strings.Contains(out, "Enabled ac_1") {
		t.Errorf("enable: %v %q", err, out)
	}
	if _, err := runCommand(t, AuthConfigsCmd(), "delete", "ac_1"); err == nil {
		t.Error("delete without --yes")
	}
	if out, err := runCommand(t, AuthConfigsCmd(), "delete", "ac_1", "--yes"); err != nil || !strings.Contains(out, "Deleted ac_1") {
		t.Errorf("delete: %v", err)
	}

	t.Setenv("ZUANO_AGENT", "1")
	for _, args := range [][]string{
		{"create", "--service", "x", "--name", "n", "--type", "managed", "--scheme", "oauth2"},
		{"secrets", "set", "ac_1"},
		{"secrets", "rotate", "ac_1"},
	} {
		if _, err := runWithStdin(t, AuthConfigsCmd(), `{"a":"b"}`, args...); exitCodeOf(err) != exitDenied {
			t.Errorf("%v from an agent: exit %d (%v)", args, exitCodeOf(err), err)
		}
	}
	if gw.count("PUT /api/v1/auth-configs/ac_1/secrets") != 1 {
		t.Error("an agent's secret write reached the gateway")
	}
}

func TestBuilderRefusalsKeepTheirClass(t *testing.T) {
	gw := a2aGateway(t, routeFns{})
	status, body := 409, `{"error":"auth_config_required","message":"No auth config for gmail."}`
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{409, `{"error":"auth_config_required","message":"No auth config for gmail."}`, 1},
		{422, `{"error":"policy_widening","message":"The session asks for destructive."}`, 1},
		{400, `{"error":"callback_not_allowed","message":"Not on the allowlist."}`, 1},
		{403, `{"error":"approval_unavailable","message":"This key cannot decide approvals."}`, exitDenied},
		{402, `{"error":"plan_required","feature":"builder.sessions","message":"Needs Pro."}`, client.ExitPlanRequired},
		{404, `{"error":"not_found"}`, 1},
	} {
		status, body = tc.status, tc.body
		_, err := runCommand(t, AuthConfigsCmd(), "show", "ac_1")
		if exitCodeOf(err) != tc.want {
			t.Errorf("%d %s: exit %d (%v), want %d", tc.status, tc.body, exitCodeOf(err), err, tc.want)
		}
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_, _ = io.WriteString(w, `{"status":"pending_approval","approval_id":"apr_3","service":"gateway","tool":"auth_config_update","risk":"write","expires_at":"2026-10-16T13:00:00Z"}`)
	})
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, AuthConfigsCmd(), "update", "ac_1", "--name", "x"); exitCodeOf(err) != exitPending {
		t.Errorf("a held write: exit %d", exitCodeOf(err))
	}
}

func TestAccounts(t *testing.T) {
	secret := fakeSecret("tok")
	gw := a2aGateway(t, routeFns{
		"GET /api/v1/connected-accounts":                    jsonReply(200, `{"items":[{"id":"ca_1","service":"gmail","alias":"work","status":"active","ownerKind":"end_user","userId":"user_1","authConfigId":"ac_1","isPrimary":true},{"id":"ca_2","service":"gmail","status":"needs_reconnect","ownerKind":"org","authConfigId":"ac_1"}],"nextCursor":"c2"}`),
		"GET /api/v1/connected-accounts/ca_1":               jsonReply(200, `{"account":{"id":"ca_1","service":"gmail","alias":"work","status":"active","ownerKind":"end_user","userId":"user_1","authConfigId":"ac_1","displayName":"jane@example.com","isPrimary":true}}`),
		"PATCH /api/v1/connected-accounts/ca_1":             jsonReply(200, `{}`),
		"PATCH /api/v1/connected-accounts/ca_1/credentials": jsonReply(200, `{}`),
		"DELETE /api/v1/connected-accounts/ca_1":            jsonReply(200, `{}`),
		"POST /api/v1/connected-accounts:import":            jsonReply(201, `{"account":{"id":"ca_9","service":"gmail","status":"active"}}`),
		"POST /api/v1/connected-accounts:importBatch":       jsonReply(202, `{"jobId":"job_5"}`),
	})
	out, err := runCommand(t, AccountsCmd(), "list", "--user", "user_1", "--service", "gmail", "--auth-config", "ac_1", "--status", "active", "--alias", "work", "--cursor", "c1")
	if err != nil || !strings.Contains(out, "ca_1  gmail    work") || !strings.Contains(out, "end_user:user_1") || !strings.Contains(out, "primary") || !strings.Contains(out, "More: --cursor c2") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = runCommand(t, AccountsCmd(), "show", "ca_1")
	if err != nil || !strings.Contains(out, "jane@example.com (gmail)") || !strings.Contains(out, "alias work") {
		t.Errorf("show: %v\n%s", err, out)
	}
	if _, err := runCommand(t, AccountsCmd(), "update", "ca_1", "--alias", "personal", "--primary"); err != nil {
		t.Fatal(err)
	}
	if b := gw.body("PATCH /api/v1/connected-accounts/ca_1", 0); b["alias"] != "personal" || b["isPrimary"] != true {
		t.Errorf("update body = %v", b)
	}
	if _, err := runCommand(t, AccountsCmd(), "update", "ca_1"); err == nil {
		t.Error("update with nothing to change")
	}
	if _, err := runCommand(t, AccountsCmd(), "delete", "ca_1"); err == nil {
		t.Error("delete without --yes")
	}
	if out, err := runCommand(t, AccountsCmd(), "delete", "ca_1", "--yes"); err != nil || !strings.Contains(out, "Disconnected ca_1") {
		t.Errorf("delete: %v", err)
	}

	creds := `{"token":"` + secret + `"}`
	out, err = runWithStdin(t, AccountsCmd(), creds, "credentials", "ca_1")
	if err != nil || strings.Contains(out, secret) || !strings.Contains(out, "Updated token on ca_1") {
		t.Errorf("credentials: %v\n%s", err, out)
	}
	out, err = runCommand(t, AccountsCmd(), "import", "--user", "user_1", "--service", "gmail", "--auth-config", "ac_1", "--alias", "imported", "--from-file", jsonFile(t, `{"refreshToken":"`+secret+`"}`))
	if err != nil || strings.Contains(out, secret) || !strings.Contains(out, "Imported ca_9 (gmail, active)") {
		t.Errorf("import: %v\n%s", err, out)
	}
	if b := gw.body("POST /api/v1/connected-accounts:import", 0); b["userId"] != "user_1" || b["alias"] != "imported" || b["credentials"].(map[string]any)["refreshToken"] != secret {
		t.Errorf("import body = %v", b)
	}
	if _, err := runCommand(t, AccountsCmd(), "import", "--user", "u", "--from-file", jsonFile(t, `{}`)); err == nil {
		t.Error("import without a service and config")
	}

	rows := `{"userId":"u1","service":"gmail","authConfigId":"ac_1","credentials":{"token":"` + secret + `"}}` + "\n\n" + `{"userId":"u2","service":"gmail","authConfigId":"ac_1","credentials":{"token":"` + secret + `"}}` + "\n"
	out, err = runCommand(t, AccountsCmd(), "import-batch", jsonFile(t, rows))
	if err != nil || !strings.Contains(out, "Started import job job_5 for 2 row(s)") || strings.Contains(out, secret) {
		t.Errorf("batch: %v\n%s", err, out)
	}
	for name, content := range map[string]string{"empty": "", "no credentials": `{"userId":"u","service":"s","authConfigId":"a"}`, "not json": "x"} {
		if _, err := runCommand(t, AccountsCmd(), "import-batch", jsonFile(t, content)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := runCommand(t, AccountsCmd(), "import-batch", filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing file")
	}

	t.Setenv("ZUANO_AGENT", "1")
	for _, args := range [][]string{{"credentials", "ca_1"}, {"import", "--user", "u", "--service", "s", "--auth-config", "ac_1"}, {"import-batch", jsonFile(t, rows)}} {
		if _, err := runWithStdin(t, AccountsCmd(), `{"a":"b"}`, args...); exitCodeOf(err) != exitDenied {
			t.Errorf("%v from an agent: exit %d", args, exitCodeOf(err))
		}
	}
}

func TestLinks(t *testing.T) {
	old := linksPollInterval
	linksPollInterval = time.Millisecond
	t.Cleanup(func() { linksPollInterval = old })
	status := "initiated"
	polls := 0
	gw := a2aGateway(t, routeFns{
		"POST /api/v1/connect-links": jsonReply(201, `{"id":"lnk_1","url":"https://connect.example/link/abc","expiresAt":"2026-10-16T13:00:00Z"}`),
		"GET /api/v1/connect-links/lnk_1": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			polls++
			s := status
			if status == "wait" && polls >= 3 {
				s = "completed"
			} else if status == "wait" {
				s = "initiated"
			}
			jsonReply(200, `{"status":"`+s+`","connectedAccountId":"ca_7","error":"consent denied"}`)(nil, w, r)
		},
	})
	out, err := runCommand(t, LinksCmd(), "create", "--user", "u1", "--service", "gmail", "--auth-config", "ac_1", "--alias", "work", "--callback-url", "https://app.example/done", "--scopes", "a,b", "--expires-in", "30m")
	if err != nil || !strings.Contains(out, "Link lnk_1, expires 2026-10-16T13:00:00Z") || !strings.Contains(out, "https://connect.example/link/abc") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if b := gw.body("POST /api/v1/connect-links", 0); b["userId"] != "u1" || b["authConfigId"] != "ac_1" || b["expiresIn"] != 1800.0 || b["callbackUrl"] != "https://app.example/done" || len(b["scopes"].([]any)) != 2 {
		t.Errorf("body = %v", b)
	}
	for _, bad := range []string{"30s", "2h", "soon"} {
		if _, err := runCommand(t, LinksCmd(), "create", "--user", "u", "--service", "s", "--expires-in", bad); err == nil {
			t.Errorf("expires %q accepted", bad)
		}
	}
	if _, err := runCommand(t, LinksCmd(), "create", "--user", "u"); err == nil {
		t.Error("create without a service")
	}
	if out, err := runCommand(t, LinksCmd(), "show", "lnk_1"); err != nil || !strings.Contains(out, "Link lnk_1: initiated") {
		t.Errorf("show: %v %q", err, out)
	}
	status = "wait"
	if out, err := runCommand(t, LinksCmd(), "show", "lnk_1", "--wait", "5s"); err != nil || !strings.Contains(out, "completed, account ca_7") {
		t.Errorf("wait: %v %q", err, out)
	}
	status = "initiated"
	if _, err := runCommand(t, LinksCmd(), "show", "lnk_1", "--wait", "20ms"); exitCodeOf(err) != exitWaitTimeout {
		t.Errorf("a link nobody used: exit %d", exitCodeOf(err))
	}
	for _, s := range []string{"failed", "expired"} {
		status = s
		if out, err := runCommand(t, LinksCmd(), "show", "lnk_1"); err == nil || !strings.Contains(out, "consent denied") {
			t.Errorf("%s: %v %q", s, err, out)
		}
	}
	if out, err := runCommand(t, LinksCmd(), "show", "lnk_1", "--json"); err == nil || !strings.Contains(out, `"status"`) {
		t.Errorf("--json: %v", err)
	}
	if _, err := runCommand(t, LinksCmd(), "show", "bad"); err == nil {
		t.Error("a bad id")
	}
}

func TestBuilderSessions(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"POST /api/v1/sessions":                      jsonReply(201, `{"session":{"id":"ses_1","userId":"user_1","identity":"service","configVersion":1,"mcp":{"url":"https://mcp.example/api/v1/sessions/ses_1/mcp"}}}`),
		"GET /api/v1/sessions":                       jsonReply(200, `{"items":[{"id":"ses_1","userId":"user_1","identity":"service","configVersion":3,"lastUsedAt":"2026-10-16"}],"nextCursor":"n"}`),
		"GET /api/v1/sessions/ses_1":                 jsonReply(200, `{"session":{"id":"ses_1","userId":"user_1","identity":"delegated_user","configVersion":3,"createdAt":"2026-10-01","mcp":{"url":"https://mcp.example/x"}}}`),
		"PATCH /api/v1/sessions/ses_1":               jsonReply(200, `{}`),
		"DELETE /api/v1/sessions/ses_1":              jsonReply(200, `{}`),
		"GET /api/v1/sessions/ses_1/config-history":  jsonReply(200, `{"items":[{"version":2,"at":"t","by":"key","summary":"risk changed"}]}`),
		"GET /api/v1/sessions/ses_1/toolkits":        jsonReply(200, `{"items":[{"service":"gmail","connected":true,"accounts":["ca_1"]},{"service":"github","connected":false}]}`),
		"POST /api/v1/sessions/ses_1/tokens":         jsonReply(201, `{"token":"`+fakeSecret("sess")+`","tokenId":"tok_1","expiresAt":"2026-10-16T14:00:00Z","mcp":{"url":"https://mcp.example/x"}}`),
		"DELETE /api/v1/sessions/ses_1/tokens/tok_1": jsonReply(200, `{}`),
	})
	policy := jsonFile(t, `{"search":{"recipes":true},"risk":{"disable":["outward"]}}`)
	out, err := runCommand(t, BuilderCmd(), "sessions", "create", "--user", "user_1", "--config-file", policy, "--toolkits", "gmail,github", "--risk-disable", "destructive", "--require-approval", "write", "--auth-config", "gmail=ac_1", "--account", "github=ca_2", "--identity", "service", "--preset", "direct", "--idle-days", "14", "--memory", "--sandbox=false", "--metadata", "conversation=c9")
	if err != nil || !strings.Contains(out, "Created ses_1 for user_1") || !strings.Contains(out, "https://mcp.example/api/v1/sessions/ses_1/mcp") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	b := gw.body("POST /api/v1/sessions", 0)
	risk, _ := b["risk"].(map[string]any)
	tk, _ := b["toolkits"].(map[string]any)
	acc, _ := b["connectedAccounts"].(map[string]any)
	if b["userId"] != "user_1" || b["identity"] != "service" || b["preset"] != "direct" || len(tk["enable"].([]any)) != 2 ||
		risk["disable"].([]any)[0] != "destructive" || risk["requireApproval"].([]any)[0] != "write" || acc["github"].([]any)[0] != "ca_2" ||
		b["authConfigs"].(map[string]any)["gmail"] != "ac_1" || b["ttl"].(map[string]any)["idleDays"] != 14.0 || b["memory"].(map[string]any)["enable"] != true ||
		b["sandbox"].(map[string]any)["enable"] != false || b["search"] == nil || b["metadata"].(map[string]any)["conversation"] != "c9" {
		t.Errorf("body = %v", b)
	}
	if _, err := runCommand(t, BuilderCmd(), "sessions", "create", "--user", "u", "--disable-toolkits", "slack", "--config-id", "scf_1"); err != nil {
		t.Errorf("disable-toolkits: %v", err)
	}
	if tk := gw.body("POST /api/v1/sessions", 1)["toolkits"].(map[string]any); tk["disable"] == nil || gw.body("POST /api/v1/sessions", 1)["configId"] != "scf_1" {
		t.Errorf("second body = %v", gw.body("POST /api/v1/sessions", 1))
	}
	for name, args := range map[string][]string{
		"no user":      {"sessions", "create"},
		"bad identity": {"sessions", "create", "--user", "u", "--identity", "root"},
		"bad preset":   {"sessions", "create", "--user", "u", "--preset", "all"},
		"bad pair":     {"sessions", "create", "--user", "u", "--account", "gmail"},
		"bad metadata": {"sessions", "create", "--user", "u", "--metadata", "novalue"},
		"missing file": {"sessions", "create", "--user", "u", "--config-file", filepath.Join(t.TempDir(), "none")},
		"bad file":     {"sessions", "create", "--user", "u", "--config-file", jsonFile(t, "[1]")},
	} {
		if _, err := runCommand(t, BuilderCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	if out, err := runCommand(t, BuilderCmd(), "sessions", "list", "--user", "user_1", "--limit", "10", "--cursor", "c"); err != nil || !strings.Contains(out, "ses_1  user_1  service   3") || !strings.Contains(out, "More: --cursor n") {
		t.Errorf("list: %v\n%s", err, out)
	}
	if out, err := runCommand(t, BuilderCmd(), "sessions", "show", "ses_1"); err != nil || !strings.Contains(out, "identity delegated_user, config version 3") {
		t.Errorf("show: %v\n%s", err, out)
	}
	if _, err := runCommand(t, BuilderCmd(), "sessions", "update", "ses_1", "--toolkits", "gmail"); err == nil {
		t.Error("update without --expected-version")
	}
	if _, err := runCommand(t, BuilderCmd(), "sessions", "update", "ses_1", "--expected-version", "3", "--identity", "service"); err == nil {
		t.Error("changing the identity")
	}
	if _, err := runCommand(t, BuilderCmd(), "sessions", "update", "ses_1", "--expected-version", "3", "--toolkits", "gmail"); err != nil {
		t.Fatal(err)
	}
	if b := gw.body("PATCH /api/v1/sessions/ses_1", 0); b["expectedConfigVersion"] != 3.0 {
		t.Errorf("update body = %v", b)
	}
	if out, err := runCommand(t, BuilderCmd(), "sessions", "history", "ses_1"); err != nil || !strings.Contains(out, "risk changed") {
		t.Errorf("history: %v\n%s", err, out)
	}
	if out, err := runCommand(t, BuilderCmd(), "sessions", "toolkits", "ses_1"); err != nil || !strings.Contains(out, "gmail") || !strings.Contains(out, "not connected") {
		t.Errorf("toolkits: %v\n%s", err, out)
	}
	if _, err := runCommand(t, BuilderCmd(), "sessions", "delete", "ses_1"); err == nil {
		t.Error("delete without --yes")
	}
	if out, err := runCommand(t, BuilderCmd(), "sessions", "delete", "ses_1", "--yes"); err != nil || !strings.Contains(out, "Deleted ses_1") {
		t.Errorf("delete: %v", err)
	}
	if out, err := runCommand(t, BuilderCmd(), "sessions", "revoke-token", "ses_1", "tok_1"); err != nil || !strings.Contains(out, "Revoked tok_1") {
		t.Errorf("revoke-token: %v", err)
	}
	if _, err := runCommand(t, BuilderCmd(), "sessions", "revoke-token", "ses_1", "a/b"); err == nil {
		t.Error("a path in the token id")
	}
}

func TestBuilderSessionTokenIsPrintedOnceOnStdout(t *testing.T) {
	token := fakeSecret("sess")
	gw := a2aGateway(t, routeFns{"POST /api/v1/sessions/ses_1/tokens": jsonReply(201, `{"token":"`+token+`","tokenId":"tok_1","expiresAt":"2026-10-16T14:00:00Z","mcp":{"url":"https://mcp.example/x"}}`)})
	var stdout, stderr bytes.Buffer
	cmd := BuilderCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"sessions", "token", "ses_1", "--expires-in", "30m"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != token || strings.Contains(stderr.String(), token) || !strings.Contains(stderr.String(), "Shown once; revoke it with `zuano builder sessions revoke-token ses_1 tok_1`") {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if b := gw.body("POST /api/v1/sessions/ses_1/tokens", 0); b["expiresIn"] != 1800.0 {
		t.Errorf("body = %v", b)
	}
	if _, err := runCommand(t, BuilderCmd(), "sessions", "token", "ses_1", "--expires-in", "48h"); err == nil {
		t.Error("a token longer than a day")
	}
	if out, err := runCommand(t, BuilderCmd(), "sessions", "token", "ses_1", "--json"); err != nil || !strings.Contains(out, "tokenId") {
		t.Errorf("--json: %v", err)
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, BuilderCmd(), "sessions", "token", "ses_1"); exitCodeOf(err) != exitDenied {
		t.Errorf("an agent minting a token: exit %d", exitCodeOf(err))
	}
}

func TestSessionConfigs(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/v1/session-configs":          jsonReply(200, `{"items":[{"id":"scf_1","name":"Support","version":4}]}`),
		"GET /api/v1/session-configs/scf_1":    jsonReply(200, `{"id":"scf_1","name":"Support","version":4,"policy":{"risk":{"disable":["outward"]}}}`),
		"POST /api/v1/session-configs":         jsonReply(201, `{"id":"scf_2","version":1}`),
		"PATCH /api/v1/session-configs/scf_1":  jsonReply(200, `{"id":"scf_1","version":5}`),
		"DELETE /api/v1/session-configs/scf_1": jsonReply(200, `{}`),
	})
	if out, err := runCommand(t, BuilderCmd(), "session-configs", "list"); err != nil || !strings.Contains(out, "scf_1  Support  4") {
		t.Errorf("list: %v\n%s", err, out)
	}
	if out, err := runCommand(t, BuilderCmd(), "session-configs", "show", "scf_1"); err != nil || !strings.Contains(out, `"policy"`) {
		t.Errorf("show: %v\n%s", err, out)
	}
	if out, err := runCommand(t, BuilderCmd(), "session-configs", "create", "--name", "Support", "--toolkits", "gmail", "--risk-disable", "outward"); err != nil || !strings.Contains(out, "Saved scf_2 as version 1") {
		t.Errorf("create: %v\n%s", err, out)
	}
	b := gw.body("POST /api/v1/session-configs", 0)
	if b["name"] != "Support" || b["policy"].(map[string]any)["risk"] == nil {
		t.Errorf("create body = %v", b)
	}
	if out, err := runCommand(t, BuilderCmd(), "session-configs", "update", "scf_1", "--propagate", "--risk-disable", "outward"); err != nil || !strings.Contains(out, "version 5") || gw.body("PATCH /api/v1/session-configs/scf_1", 0)["propagate"] != true {
		t.Errorf("update: %v\n%s", err, out)
	}
	if _, err := runCommand(t, BuilderCmd(), "session-configs", "create"); err == nil {
		t.Error("create without a name")
	}
	if _, err := runCommand(t, BuilderCmd(), "session-configs", "delete", "scf_1"); err == nil {
		t.Error("delete without --yes")
	}
	if out, err := runCommand(t, BuilderCmd(), "session-configs", "delete", "scf_1", "-y"); err != nil || !strings.Contains(out, "Deleted scf_1") {
		t.Errorf("delete: %v", err)
	}
}

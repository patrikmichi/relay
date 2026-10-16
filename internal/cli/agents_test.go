package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/config"
)

const agentJSON = `{"id":"agt_1","name":"Claude on laptop","kind":"mcp_client","owner":"ann@example.com","status":"active","lastActivity":"2026-10-15","exposure":"high"}`

func TestAgentsList(t *testing.T) {
	var q string
	packGateway(t, routeMap{"GET /api/agents": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"agents":[`+agentJSON+`,{"id":"agt_2","name":"CI key","kind":"api_key_agent","status":"suspended"}]}`)
	}})
	out, err := runCommand(t, AgentsCmd(), "list", "--kind", "mcp_client", "--status", "active", "--owner", "ann@example.com", "--label", "env=ci", "--stale", "--exposure", "high", "--can", "stripe.create_refund", "--risk", "write", "--mode", "allowed")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agt_1", "Claude on laptop", "mcp_client", "ann@example.com", "high", "agt_2", "suspended"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"kind=mcp_client", "label=env%3Dci", "stale=true", "can=stripe.create_refund", "mode=allowed", "risk=write", "exposure=high", "owner=ann%40example.com", "status=active"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %q", q, want)
		}
	}
	if out, err := runCommand(t, AgentsCmd(), "list", "--json"); err != nil || !strings.Contains(out, "agt_2") {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCommand(t, AgentsCmd(), "list", "--label", "novalue"); err == nil {
		t.Error("a label without = was accepted")
	}
}

func TestAgentsList_Empty(t *testing.T) {
	packGateway(t, routeMap{"GET /api/agents": jsonReply(200, `{"agents":[]}`)})
	if out, err := runCommand(t, AgentsCmd(), "list"); err != nil || !strings.Contains(out, "No identities match.") {
		t.Errorf("%q %v", out, err)
	}
}

func TestAgentsShowAndActivity(t *testing.T) {
	var q string
	packGateway(t, routeMap{
		"GET /api/agents/agt_1": jsonReply(200, `{"agent":{"id":"agt_1","name":"Claude","kind":"mcp_client","status":"active","owner":"ann@example.com","exposure":"high",
		 "credentials":[{"kind":"oauth family","createdAt":"2026-09-01","expiresAt":"2026-12-01"}],"device":{"hostLabel":"laptop","os":"linux"},
		 "access":[{"service":"stripe","risk":"write","mode":"approval","source":"org_matrix"}],"exposureFactors":["can write to stripe","no owner review"]}}`),
		"GET /api/agents/agt_1/activity": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			q = r.URL.RawQuery
			_, _ = io.WriteString(w, `{"activity":{"calls":[{"service":"stripe","count":12}],"approvals":3,"refusals":1}}`)
		},
	})
	out, err := runCommand(t, AgentsCmd(), "show", "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agt_1 Claude (mcp_client, active)", "owner: ann@example.com", "device: laptop (linux)", "credential oauth family, created 2026-09-01, expires 2026-12-01", "can reach stripe", "decided by org_matrix", "why: can write to stripe; no owner review"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, AgentsCmd(), "activity", "agt_1", "--from", "2026-10-01", "--to", "2026-10-15")
	if err != nil || !strings.Contains(out, "stripe") || !strings.Contains(out, "12 calls") || !strings.Contains(out, "3 approvals, 1 refusals") || q != "from=2026-10-01&to=2026-10-15" {
		t.Errorf("activity: %q %q %v", out, q, err)
	}
	for _, args := range [][]string{{"show", "agt_1", "--json"}, {"activity", "agt_1", "--json"}} {
		if out, err := runCommand(t, AgentsCmd(), args...); err != nil || !strings.Contains(out, "{") {
			t.Errorf("%v: %q %v", args, out, err)
		}
	}
}

func TestAgentsSuspendResumeRevoke(t *testing.T) {
	ps := packGateway(t, routeMap{
		"POST /api/agents/agt_1/suspend": jsonReply(200, `{"ok":true}`),
		"POST /api/agents/agt_1/resume":  jsonReply(200, `{"ok":true}`),
		"POST /api/agents/agt_1/revoke":  jsonReply(200, `{"ok":true}`),
	})
	if out, err := runCommand(t, AgentsCmd(), "suspend", "agt_1", "--reason", "leaked key"); err != nil || !strings.Contains(out, "agt_1: suspend done.") {
		t.Errorf("suspend: %q %v", out, err)
	}
	if b := ps.body("POST /api/agents/agt_1/suspend", 0); b["reason"] != "leaked key" {
		t.Errorf("suspend body = %v", b)
	}
	if _, err := runCommand(t, AgentsCmd(), "resume", "agt_1"); err != nil {
		t.Errorf("resume: %v", err)
	}
	if b := ps.body("POST /api/agents/agt_1/resume", 0); b["reason"] != nil {
		t.Errorf("resume body = %v", b)
	}
	if _, err := runCommand(t, AgentsCmd(), "revoke", "agt_1", "--reason", "gone"); err == nil {
		t.Error("revoked without confirmation")
	}
	if out, err := runCommand(t, AgentsCmd(), "revoke", "agt_1", "--reason", "gone", "--yes", "--json"); err != nil || !strings.Contains(out, `"ok":true`) {
		t.Errorf("revoke: %q %v", out, err)
	}
	if _, err := runCommand(t, AgentsCmd(), "suspend", "agt_1"); err == nil || !strings.Contains(err.Error(), "--reason is required") {
		t.Errorf("no reason: %v", err)
	}
	before := ps.count("POST /api/agents/agt_1/suspend")
	t.Setenv("ZUANO_AGENT", "1")
	for verb, args := range map[string][]string{"suspend": {"--reason", "x"}, "resume": nil, "revoke": {"--reason", "x", "--yes"}} {
		if _, err := runCommand(t, AgentsCmd(), append([]string{verb, "agt_1"}, args...)...); exitCodeOf(err) != 10 {
			t.Errorf("%s under an agent: %v", verb, err)
		}
	}
	if ps.count("POST /api/agents/agt_1/suspend") != before {
		t.Error("an agent's suspend reached the gateway")
	}
}

func TestAgentsStepUpAndSuspendedIdentity(t *testing.T) {
	packGateway(t, routeMap{
		"POST /api/agents/agt_1/resume": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_id":"su_1","step_up_url":"http://`+r.Host+`/stepup/su_1"}`)
		},
		"GET /api/agents": jsonReply(403, `{"error":"identity_suspended","message":"this agent is suspended"}`),
	})
	if _, err := runCommand(t, AgentsCmd(), "resume", "agt_1"); exitCodeOf(err) != 14 {
		t.Errorf("step-up: %v", err)
	}
	if _, err := runCommand(t, AgentsCmd(), "list"); exitCodeOf(err) != 33 || !strings.Contains(err.Error(), "this agent is suspended") {
		t.Errorf("suspended: exit %d, %v", exitCodeOf(err), err)
	}
}

func TestAgentsDevicesAndShadowMCP(t *testing.T) {
	packGateway(t, routeMap{
		"GET /api/agents/devices":    jsonReply(200, `{"devices":[{"hostLabel":"laptop","os":"linux","cliVersion":"1.2.3","owner":"ann@example.com","lastReportAt":"2026-10-15","agents":[{"id":"claude","gatewayMcp":true},{"id":"cursor","gatewayMcp":false}]}]}`),
		"GET /api/agents/shadow-mcp": jsonReply(200, `{"servers":[{"name":"files","transport":"stdio","command":"npx","devices":3},{"name":"web","transport":"http","host":"mcp.example.com","devices":1}]}`),
	})
	out, err := runCommand(t, AgentsCmd(), "devices")
	if err != nil || !strings.Contains(out, "laptop") || !strings.Contains(out, "claude*, cursor") || !strings.Contains(out, "* has the gateway's MCP server") {
		t.Errorf("devices: %q %v", out, err)
	}
	out, err = runCommand(t, AgentsCmd(), "shadow-mcp")
	if err != nil || !strings.Contains(out, "files") || !strings.Contains(out, "npx") || !strings.Contains(out, "mcp.example.com") {
		t.Errorf("shadow: %q %v", out, err)
	}
	for _, v := range []string{"devices", "shadow-mcp"} {
		if out, err := runCommand(t, AgentsCmd(), v, "--json"); err != nil || !strings.Contains(out, "{") {
			t.Errorf("%s json: %q %v", v, out, err)
		}
	}
}

func TestAgentsShadowMCP_None(t *testing.T) {
	packGateway(t, routeMap{"GET /api/agents/shadow-mcp": jsonReply(200, `{"servers":[]}`)})
	if out, err := runCommand(t, AgentsCmd(), "shadow-mcp"); err != nil || !strings.Contains(out, "No unmanaged MCP servers reported.") {
		t.Errorf("%q %v", out, err)
	}
}

func TestReviews(t *testing.T) {
	ps := packGateway(t, routeMap{
		"GET /api/access-reviews":       jsonReply(200, `{"reviews":[{"id":"rev_1","name":"Q4","status":"open","dueAt":"2026-11-01","progress":{"decided":2,"total":5}}]}`),
		"GET /api/access-reviews/rev_1": jsonReply(200, `{"review":{"id":"rev_1","name":"Q4","status":"open","progress":{"decided":2,"total":5}},"items":[{"identityId":"agt_1","name":"Claude","reviewer":"ann@example.com","decision":"keep"},{"identityId":"agt_2","name":"CI"}]}`),
		"GET /api/access-reviews/rev_1/export": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "identity,decision\nagt_1,keep\n")
		},
	})
	if out, err := runCommand(t, ReviewsCmd(), "list"); err != nil || !strings.Contains(out, "rev_1") || !strings.Contains(out, "2 of 5") {
		t.Errorf("list: %q %v", out, err)
	}
	out, err := runCommand(t, ReviewsCmd(), "show", "rev_1")
	if err != nil || !strings.Contains(out, "2 of 5 decided") || !strings.Contains(out, "agt_1") || !strings.Contains(out, "keep") {
		t.Errorf("show: %q %v", out, err)
	}
	if out, err := runCommand(t, ReviewsCmd(), "export", "rev_1", "--format", "csv"); err != nil || out != "identity,decision\nagt_1,keep\n" {
		t.Errorf("export: %q %v", out, err)
	}
	file := filepath.Join(t.TempDir(), "r.csv")
	if out, err := runCommand(t, ReviewsCmd(), "export", "rev_1", "--out", file); err != nil || !strings.Contains(out, "Wrote") {
		t.Errorf("export file: %q %v", out, err)
	}
	if raw, _ := os.ReadFile(file); !strings.Contains(string(raw), "agt_1") {
		t.Errorf("file = %q", raw)
	}
	if _, err := runCommand(t, ReviewsCmd(), "export", "rev_1", "--format", "xml"); err == nil {
		t.Error("bad format")
	}
	for _, args := range [][]string{{"list", "--json"}, {"show", "rev_1", "--json"}} {
		if out, err := runCommand(t, ReviewsCmd(), args...); err != nil || !strings.Contains(out, "{") {
			t.Errorf("%v: %q %v", args, out, err)
		}
	}
	out, err = runCommand(t, ReviewsCmd(), "decide", "rev_1")
	if err != nil || !strings.Contains(out, ps.URL+"/reviews?campaign=rev_1") {
		t.Errorf("decide: %q %v", out, err)
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, ReviewsCmd(), "decide", "rev_1"); exitCodeOf(err) != 14 {
		t.Errorf("agent decide: %v", err)
	}
}

func TestWhoamiShowsTheIdentity(t *testing.T) {
	packGateway(t, routeMap{"GET /api/cli/whoami": jsonReply(200, `{"email":"ann@example.com","identityId":"agt_9","identityStatus":"active","issuedAt":"2026-10-01"}`)})
	out, err := runCommand(t, WhoamiCmd())
	if err != nil || !strings.Contains(out, "Identity: agt_9 (active)") {
		t.Errorf("%q %v", out, err)
	}
}

func fakeAgentHome(t *testing.T) agentsync.Env {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"mcpServers":{
	  "other":{"type":"http","url":"https://other.example/mcp","headers":{"Authorization":"Bearer SECRETVALUE"}},
	  "files":{"type":"stdio","command":"/usr/bin/npx","args":["-y","files-server","--token","ARGSECRET"],"env":{"API_KEY":"ENVSECRET"}},
	  "web":{"type":"http","url":"https://mcp.example.com/sse?token=QUERYSECRET"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return agentsync.Env{Home: home, Project: home}
}

func TestDeviceReportCarriesNamesAndHostsNeverArgumentsEnvOrHeaders(t *testing.T) {
	env := fakeAgentHome(t)
	rep, err := buildDeviceReport(env, "https://gw.example", "my-laptop")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	for _, leak := range []string{"SECRETVALUE", "ARGSECRET", "ENVSECRET", "QUERYSECRET", "Authorization", "/usr/bin", "files-server"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%q in the report: %s", leak, raw)
		}
	}
	if len(rep.Agents) != 1 || rep.Agents[0].ID != "claude" || rep.Agents[0].GatewayMCP || len(rep.Agents[0].MCPServers) != 2 {
		t.Fatalf("agents = %+v", rep.Agents)
	}
	byName := map[string]deviceMCP{}
	for _, m := range rep.Agents[0].MCPServers {
		byName[m.Name] = m
	}
	if byName["files"].Transport != "stdio" || byName["files"].Command != "npx" || byName["web"].Host != "mcp.example.com" || byName["web"].Transport != "http" {
		t.Errorf("servers = %+v", byName)
	}
	if rep.HostLabel != "my-laptop" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(rep.InstallID) || rep.OS == "" || rep.CLIVersion == "" {
		t.Errorf("report = %+v", rep)
	}
}

func TestReportDeviceSendsWithAPersonalSessionOnly(t *testing.T) {
	ps := packGatewayAt(t, routeMap{"POST /api/agents/devices/report": jsonReply(200, `{}`)}, false)
	fakeAgentHome2(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	label := ""
	reportDevice(cmd, "", &label)
	if ps.count("POST /api/agents/devices/report") != 1 {
		t.Fatal("no report was sent")
	}
	b := ps.body("POST /api/agents/devices/report", 0)
	if b["hostLabel"] != "" || b["installId"] == "" {
		t.Errorf("body = %v", b)
	}
	in, _ := config.LoadInstall()
	if !in.HostLabelSet || in.LastReportAt == "" || in.LastReportSent != 1 {
		t.Errorf("install = %+v", in)
	}
	if hostLabel(in) != "" {
		t.Errorf("an empty label chosen on purpose became %q", hostLabel(in))
	}
	if hostLabel(config.Install{}) == "" {
		t.Log("no host name on this machine")
	}

	t.Setenv("ZUANO_AGENT", "1")
	reportDevice(cmd, "", nil)
	SetOffline(true)
	reportDevice(cmd, "", nil)
	SetOffline(false)
	if ps.count("POST /api/agents/devices/report") != 1 {
		t.Error("an agent session or --offline still reported")
	}
}

func fakeAgentHome2(t *testing.T) {
	t.Helper()
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReportDeviceWarnsWhenTheGatewayRefuses(t *testing.T) {
	ps := packGateway(t, routeMap{"POST /api/agents/devices/report": jsonReply(500, `{"error":"boom"}`)})
	fakeAgentHome2(t)
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	reportDevice(cmd, ps.URL, nil)
	if !strings.Contains(out.String(), "the device report was not sent") {
		t.Errorf("stderr = %q", out.String())
	}
	if in, _ := config.LoadInstall(); in.LastReportAt != "" {
		t.Error("a failed report was recorded")
	}
}

func TestSyncWritesTheInstallHeaderIntoTheGatewayEntry(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	if _, err := runCommand(t, SyncCmd(), "--to", "claude,codex", "--kind", "mcp", "--no-packs", "--yes"); err != nil {
		t.Fatal(err)
	}
	id, _ := config.InstallID()
	claude := readTestFile(t, filepath.Join(e.home, ".claude.json"))
	codex := readTestFile(t, filepath.Join(e.home, ".codex", "config.toml"))
	if !strings.Contains(claude, `"X-Zuano-Install": "`+id+`"`) && !strings.Contains(claude, `"X-Zuano-Install":"`+id+`"`) {
		t.Errorf("claude config lacks the header:\n%s", claude)
	}
	if !strings.Contains(codex, `http_headers = { X-Zuano-Install = "`+id+`" }`) && !strings.Contains(codex, `http_headers = { "X-Zuano-Install" = "`+id+`" }`) {
		t.Errorf("codex config lacks the header:\n%s", codex)
	}
	if strings.Contains(claude, "Authorization") {
		t.Error("the header carried a credential")
	}
}

func TestDeviceReportSeesTheGatewayEntryAfterASync(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp", "--no-packs", "--yes"); err != nil {
		t.Fatal(err)
	}
	env := agentsync.Env{Home: e.home, Project: e.home}
	rep, err := buildDeviceReport(env, e.gw.URL, "h")
	if err != nil {
		t.Fatal(err)
	}
	var claude *deviceAgent
	for i := range rep.Agents {
		if rep.Agents[i].ID == "claude" {
			claude = &rep.Agents[i]
		}
	}
	if claude == nil || !claude.GatewayMCP {
		t.Errorf("agents = %+v", rep.Agents)
	}
	if hasGatewayEntry(env, "claude", nil) {
		t.Error("no gateway means no entry")
	}
}

func TestDoctorDeviceRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var d string
	doctorDevice(func(name string, ok bool, detail string) { d = detail })
	if !strings.Contains(d, "install id ") || !strings.Contains(d, "never reported") {
		t.Errorf("fresh: %q", d)
	}
	_ = config.RecordReport(2)
	doctorDevice(func(name string, ok bool, detail string) { d = detail })
	if !strings.Contains(d, "last device report 20") {
		t.Errorf("reported: %q", d)
	}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".config", "zuano", "install.json.d"), 0o700); err != nil {
		t.Fatal(err)
	}
}

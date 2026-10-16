package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/client"
)

// connectorGateway is a gateway double covering the endpoints connectors and
// connect use. sessionAPI toggles whether /api/connections* accepts bearer
// callers; real gateways may restrict them to browser sessions.
type connectorGateway struct {
	*httptest.Server
	mu         sync.Mutex
	sessionAPI bool
	linked     map[string]bool
	oauthState map[string]string
	saved      []map[string]any
	probes     int
	linkAfter  int

	// statusEndpoint makes GET /api/connections?service=<slug> answer
	// {connected}; statusCode forces that route to fail with the given status.
	statusEndpoint bool
	statusLinked   map[string]bool
	statusCode     int
	statusCalls    int
}

func newConnectorGateway(t *testing.T) *connectorGateway {
	t.Helper()
	g := &connectorGateway{sessionAPI: true, linked: map[string]bool{}, oauthState: map[string]string{}, statusLinked: map[string]bool{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	withAPIKeyGateway(t, g.Server)
	return g
}

func (g *connectorGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch r.URL.Path {
	case "/api/integrations":
		reply(200, map[string]any{"ok": true, "serviceCount": 5, "toolCount": 5, "services": []map[string]any{
			{"id": "clockify", "name": "Clockify", "authType": "apikey", "category": "Time tracking", "tags": []string{"timesheets"}, "description": "Time entries", "accessible": true, "toolCount": 1, "tools": []map[string]any{{"name": "list_time_entries", "description": "List entries"}}},
			{"id": "notion", "name": "Notion", "authType": "apikey", "category": "Work management", "summary": "Pages and databases", "accessible": true, "toolCount": 1, "tools": []map[string]any{{"name": "search_pages"}}},
			{"id": "google-workspace", "name": "Google Workspace", "authType": "oauth", "category": "Productivity", "accessible": true, "toolCount": 1, "tools": []map[string]any{{"name": "list_files"}}},
			{"id": "echo", "name": "Echo", "authType": "none", "category": "Utilities", "accessible": true, "toolCount": 1, "tools": []map[string]any{{"name": "echo"}}},
			{"id": "github", "name": "GitHub", "authType": "apikey", "category": "Code", "accessible": false, "toolCount": 1, "tools": []map[string]any{{"name": "list_repos"}}},
		}})
	case "/api/cli/services/authorize":
		g.probes++
		svc := r.URL.Query().Get("service")
		if r.URL.Query().Get("scope") == "" {
			reply(400, map[string]string{"error": "invalid_request"})
			return
		}
		linked := g.linked[svc] || (g.linkAfter > 0 && g.probes >= g.linkAfter && svc == "notion")
		reply(200, map[string]any{"service": svc, "accountLinked": linked, "authorizationPath": "/settings/connections"})
	case "/api/connections":
		if svc := r.URL.Query().Get("service"); svc != "" && g.statusCode != 0 {
			g.statusCalls++
			reply(g.statusCode, map[string]string{"error": "no status endpoint"})
			return
		} else if svc != "" && g.statusEndpoint {
			g.statusCalls++
			reply(200, map[string]bool{"connected": g.statusLinked[svc]})
			return
		}
		if !g.sessionAPI {
			reply(401, map[string]string{"error": "Unauthorized"})
			return
		}
		reply(200, map[string]any{"connections": []map[string]any{
			{"serviceId": "clockify", "authType": "apikey", "connected": g.linked["clockify"], "orgAllowed": true, "fields": []map[string]any{
				{"key": "CLOCKIFY_API_KEY", "label": "API key", "secret": true},
				{"key": "WORKSPACE", "label": "Workspace", "secret": false, "optional": true},
			}},
			{"serviceId": "notion", "authType": "apikey", "connected": g.linked["notion"], "orgAllowed": true,
				"fields": []map[string]any{{"key": "NOTION_TOKEN", "secret": true}},
				"oauth":  map[string]string{"state": "needs_connect", "connectUrl": "/api/connectors/oauth/start?service=notion"}},
		}})
	case "/api/connections/keys":
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		_ = json.Unmarshal(body, &doc)
		g.saved = append(g.saved, doc)
		g.linked[doc["service"].(string)] = true
		reply(200, map[string]bool{"saved": true})
	case "/api/connectors/oauth/status":
		state := g.oauthState[r.URL.Query().Get("service")]
		if state == "" {
			state = "needs_connect"
		}
		reply(200, map[string]string{"service": r.URL.Query().Get("service"), "state": state})
	default:
		http.NotFound(w, r)
	}
}

func stubBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	prevOpen, prevPoll := openBrowserURL, connectPollInterval
	openBrowserURL = func(u string) error { opened = append(opened, u); return nil }
	connectPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { openBrowserURL, connectPollInterval = prevOpen, prevPoll })
	return &opened
}

func TestFetchServiceLinks_OneMissingServiceDoesNotDisableTheStatusEndpoint(t *testing.T) {
	var statusCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/connections" {
			http.NotFound(w, r) // the authorize fallback
			return
		}
		statusCalls.Add(1)
		if r.URL.Query().Get("service") == "gone" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"connected":true}`))
	}))
	defer srv.Close()

	services := []DiscoveryService{{ID: "gone", Accessible: true, AuthType: "apikey"}}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		services = append(services, DiscoveryService{ID: id, Accessible: true, AuthType: "oauth"})
	}
	links := fetchServiceLinks(context.Background(), client.New(srv.URL, "tok"), services)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !links[id].Known || !links[id].AccountLinked {
			t.Errorf("%s = %+v, want connected from the status endpoint", id, links[id])
		}
	}
	if n := statusCalls.Load(); n != 6 {
		t.Errorf("status endpoint called %d times, want once per service (6)", n)
	}
}

func TestConnectorsListSearchAndCategory(t *testing.T) {
	newConnectorGateway(t)

	out, err := runCommand(t, ConnectorsCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SLUG", "clockify", "Time tracking", "API key", "OAuth", "github", "5 connectors"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}

	out, err = runCommand(t, ConnectorsCmd(), "search", "timesheets")
	if err != nil || !strings.Contains(out, "clockify") || strings.Contains(out, "notion") {
		t.Errorf("tag search: %v\n%s", err, out)
	}
	out, _ = runCommand(t, ConnectorsCmd(), "search", "list", "entries")
	if !strings.Contains(out, "clockify") || strings.Contains(out, "github") {
		t.Errorf("tool search:\n%s", out)
	}
	out, _ = runCommand(t, ConnectorsCmd(), "--category", "work management")
	if !strings.Contains(out, "notion") || strings.Contains(out, "clockify") {
		t.Errorf("category filter:\n%s", out)
	}
	out, _ = runCommand(t, ConnectorsCmd(), "search", "nonexistent-thing")
	if !strings.Contains(out, "no connectors match") {
		t.Errorf("empty search:\n%s", out)
	}
	out, _ = runCommand(t, ConnectorsCmd(), "--categories")
	if !strings.Contains(out, "Utilities") || !strings.Contains(out, "Work management") {
		t.Errorf("categories:\n%s", out)
	}
}

func TestConnectorsJSONAndStatus(t *testing.T) {
	g := newConnectorGateway(t)
	g.linked["clockify"] = true

	out, err := runCommand(t, ConnectorsCmd(), "--json", "--status", "--category", "Time tracking")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct{ ID, Status string }
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].ID != "clockify" || rows[0].Status != "connected" {
		t.Fatalf("rows = %+v err %v\n%s", rows, err, out)
	}

	out, _ = runCommand(t, ConnectorsCmd(), "--status")
	for _, want := range []string{"STATUS", "connected", "not connected", "no access", "-"} {
		if !strings.Contains(out, want) {
			t.Errorf("status table missing %q:\n%s", want, out)
		}
	}
}

func TestServicesShowsConnectionStatus(t *testing.T) {
	g := newConnectorGateway(t)
	g.linked["clockify"] = true
	out, err := runCommand(t, ServicesCmd())
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	for svc, want := range map[string]string{"clockify": "connected", "notion": "not connected", "github": "(no access)", "echo": "tools"} {
		if !strings.HasSuffix(strings.TrimSpace(lines[svc]), want) {
			t.Errorf("%s line = %q, want suffix %q", svc, lines[svc], want)
		}
	}
	if strings.Contains(lines["echo"], "connected") {
		t.Errorf("service without credentials shows a status: %q", lines["echo"])
	}

	probesBefore := g.probes
	out, _ = runCommand(t, ServicesCmd(), "--no-status")
	if g.probes != probesBefore || strings.Contains(out, "connected") {
		t.Errorf("--no-status still probed or printed status:\n%s", out)
	}
}

func TestConnectStoresAPIKeyFromFlagsWithoutEchoingIt(t *testing.T) {
	g := newConnectorGateway(t)
	secret := syntheticKey("clockify", "value")
	out, err := runCommand(t, ConnectCmd(), "clockify", "--field", "CLOCKIFY_API_KEY="+secret, "--label", "Work")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("secret echoed:\n%s", out)
	}
	if len(g.saved) != 1 {
		t.Fatalf("saved = %v", g.saved)
	}
	fields := g.saved[0]["fields"].(map[string]any)
	if g.saved[0]["service"] != "clockify" || fields["CLOCKIFY_API_KEY"] != secret || g.saved[0]["label"] != "Work" {
		t.Errorf("saved body = %v", g.saved[0])
	}
	if _, ok := fields["WORKSPACE"]; ok {
		t.Error("an empty optional field was sent")
	}

	again, _ := runCommand(t, ConnectCmd(), "clockify")
	if !strings.Contains(again, "already connected") {
		t.Errorf("second connect:\n%s", again)
	}
}

func TestConnectReadsKeyFromEnvAndRequiresAValueWithoutATerminal(t *testing.T) {
	g := newConnectorGateway(t)
	if _, err := runCommand(t, ConnectCmd(), "clockify"); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("RELAY_CONNECT_CLOCKIFY_API_KEY", syntheticKey("env"))
	if _, err := runCommand(t, ConnectCmd(), "clockify"); err != nil {
		t.Fatal(err)
	}
	if len(g.saved) != 1 {
		t.Fatalf("saved = %v", g.saved)
	}
}

func TestConnectRejectsBadInput(t *testing.T) {
	newConnectorGateway(t)
	for want, args := range map[string][]string{
		"no connector":    {"ghost"},
		"no access":       {"github"},
		"invalid --field": {"clockify", "--field", "oops"},
	} {
		_, err := runCommand(t, ConnectCmd(), args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", args, err, want)
		}
	}
	out, err := runCommand(t, ConnectCmd(), "echo")
	if err != nil || !strings.Contains(out, "needs no credentials") {
		t.Errorf("echo: %v\n%s", err, out)
	}
}

func TestConnectOAuthOpensBrowserAndWaitsForConnection(t *testing.T) {
	g := newConnectorGateway(t)
	opened := stubBrowser(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		g.mu.Lock()
		g.oauthState["notion"] = "connected"
		g.mu.Unlock()
	}()
	out, err := runCommand(t, ConnectCmd(), "notion", "--wait", "5s")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if len(*opened) != 1 || !strings.HasSuffix((*opened)[0], "/api/connectors/oauth/start?service=notion") {
		t.Errorf("opened = %v", *opened)
	}
	if !strings.Contains(out, "notion is connected") {
		t.Errorf("output:\n%s", out)
	}
}

func TestConnectOAuthReportsMissingApp(t *testing.T) {
	g := newConnectorGateway(t)
	stubBrowser(t)
	g.oauthState["notion"] = "needs_app"
	_, err := runCommand(t, ConnectCmd(), "notion", "--wait", "5s")
	if err == nil || !strings.Contains(err.Error(), "no OAuth app registered") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectFallsBackToTheConnectionPageForSessionOnlyGateways(t *testing.T) {
	g := newConnectorGateway(t)
	g.sessionAPI = false
	g.linkAfter = 6
	opened := stubBrowser(t)

	out, err := runCommand(t, ConnectCmd(), "notion", "--wait", "5s")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if len(*opened) != 1 || !strings.HasSuffix((*opened)[0], "/settings/connections") {
		t.Errorf("opened = %v", *opened)
	}

	g.linkAfter = 0
	g.probes = 0
	out, err = runCommand(t, ConnectCmd(), "notion", "--oauth", "--no-browser", "--wait", "30ms", "--reconnect")
	if err == nil || !strings.Contains(err.Error(), "not connected yet") {
		t.Errorf("timeout err = %v", err)
	}
	if !strings.Contains(out, "/api/connectors/oauth/start?service=notion") {
		t.Errorf("--no-browser should print the OAuth URL:\n%s", out)
	}
}

func TestConnectPollsTheConnectionStatusEndpointForDeclarativeOAuth(t *testing.T) {
	g := newConnectorGateway(t)
	g.sessionAPI = false
	g.statusEndpoint = true
	opened := stubBrowser(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		g.mu.Lock()
		g.statusLinked["notion"] = true
		g.mu.Unlock()
	}()

	out, err := runCommand(t, ConnectCmd(), "notion", "--oauth", "--wait", "5s")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if len(*opened) != 1 || !strings.HasSuffix((*opened)[0], "/api/connectors/oauth/start?service=notion") {
		t.Errorf("opened = %v", *opened)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.probes != 0 || g.statusCalls < 2 {
		t.Errorf("authorize probes = %d, status calls = %d; the status endpoint should have decided", g.probes, g.statusCalls)
	}
}

func TestConnectFallsBackToAuthorizeProbeWhenStatusEndpointIsMissing(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		g := newConnectorGateway(t)
		g.sessionAPI = false
		g.statusCode = code
		g.linkAfter = 4
		stubBrowser(t)
		if out, err := runCommand(t, ConnectCmd(), "notion", "--wait", "5s"); err != nil {
			t.Fatalf("status %d: %v\n%s", code, err, out)
		}
		if g.probes < 4 {
			t.Errorf("status %d: authorize probes = %d, the fallback never ran", code, g.probes)
		}
	}
}

func TestConnectReportsAlreadyConnectedFromStatusEndpoint(t *testing.T) {
	g := newConnectorGateway(t)
	g.statusEndpoint = true
	g.statusLinked["notion"] = true
	out, err := runCommand(t, ConnectCmd(), "notion")
	if err != nil || !strings.Contains(out, "already connected") {
		t.Fatalf("%v\n%s", err, out)
	}
	if g.probes != 0 {
		t.Errorf("authorize probes = %d", g.probes)
	}
}

func TestServicesStatusPrefersTheConnectionStatusEndpoint(t *testing.T) {
	g := newConnectorGateway(t)
	g.statusEndpoint = true
	g.statusLinked["notion"] = true
	out, err := runCommand(t, ServicesCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "notion":
			if !strings.HasSuffix(l, "connected") || strings.HasSuffix(l, "not connected") {
				t.Errorf("notion = %q", l)
			}
		case "clockify":
			if !strings.HasSuffix(l, "not connected") {
				t.Errorf("clockify = %q", l)
			}
		}
	}
	if g.probes != 0 {
		t.Errorf("authorize probes = %d, want 0", g.probes)
	}

	// A gateway without the endpoint is probed through authorize instead,
	// and the failing endpoint is not retried for every service.
	g2 := newConnectorGateway(t)
	g2.statusCode = http.StatusNotFound
	g2.linked["clockify"] = true
	out, _ = runCommand(t, ServicesCmd())
	if !strings.Contains(out, "connected") || g2.probes == 0 {
		t.Errorf("fallback not used:\n%s", out)
	}
}

func TestConnectGoogleWorkspaceUsesItsAuthorizeEndpoint(t *testing.T) {
	g := newConnectorGateway(t)
	g.sessionAPI = false
	opened := stubBrowser(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		g.mu.Lock()
		g.linked["google-workspace"] = true
		g.mu.Unlock()
	}()
	if _, err := runCommand(t, ConnectCmd(), "google-workspace", "--wait", "5s"); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 1 || !strings.HasSuffix((*opened)[0], "/api/connections/google/authorize") {
		t.Errorf("opened = %v", *opened)
	}
}

func TestConnectorsAndConnectFailClosedOffline(t *testing.T) {
	withNoGateway(t)
	for _, cmd := range []struct {
		name string
		args []string
	}{{"connectors", nil}, {"connect", []string{"clockify"}}} {
		var err error
		if cmd.name == "connectors" {
			_, err = runCommand(t, ConnectorsCmd(), cmd.args...)
		} else {
			_, err = runCommand(t, ConnectCmd(), cmd.args...)
		}
		if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
			t.Errorf("%s: err = %v", cmd.name, err)
		}
	}
}

func TestFilterServicesRanksIDMatchesFirst(t *testing.T) {
	services := []DiscoveryService{
		{ID: "slack", Name: "Slack", Description: "chat for teams"},
		{ID: "chat-bridge", Name: "Chat Bridge"},
		{ID: "discord", Name: "Discord", Tags: []string{"chat"}},
	}
	got := filterServices(services, "chat", "")
	if len(got) != 3 || got[0].ID != "chat-bridge" || got[1].ID != "discord" || got[2].ID != "slack" {
		t.Fatalf("ranking = %+v", got)
	}
}

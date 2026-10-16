package cli

import (
	"net/http"
	"strings"
	"testing"
)

const networkList = `{"networks":[{"id":"net_1","name":"Datacentre 1","slug":"dc1","status":"online","agents_online":1,"bindings":2,"last_seen_at":"2026-10-16T10:00:00Z"}]}`

func networksGateway(t *testing.T, extra map[string]func(*packServer, http.ResponseWriter, *http.Request)) *packServer {
	t.Helper()
	routes := map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"GET /api/admin/networks": jsonReply(200, networkList),
	}
	for k, v := range extra {
		routes[k] = v
	}
	syncHome(t)
	return packGateway(t, routes)
}

func TestNetworksReadCommands(t *testing.T) {
	gw := networksGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"GET /api/admin/networks/net_1":           jsonReply(200, `{"network":{"id":"net_1","status":"online","data_class":"internal"}}`),
		"GET /api/admin/networks/net_1/agents":    jsonReply(200, `{"agents":[{"id":"nag_1","name":"erp-a","status":"active","connection":"online","version":"1.4.2","platform":"linux/amd64","last_seen_at":"now","key_age_days":3}]}`),
		"GET /api/admin/networks/net_1/allowlist": jsonReply(200, `{"drift":true,"targets":[{"name":"erp","kind":"http","methods":["GET"],"path_globs":["/invoices/**"],"agents":["erp-a"],"bound_to":["erp-private"]},{"name":"hr","kind":"mcp_stdio","tool_globs":["employees_*"],"agents":["erp-a"]}],"unserved_bindings":["crm"]}`),
		"GET /api/admin/networks/net_1/logs":      jsonReply(200, `{"logs":[{"ts":"t","agent":"erp-a","target":"erp","method":"GET","status":200,"outcome":"ok","duration_ms":12,"audit_ref":"aud_1"}]}`),
	})
	gw.lenient = true
	out, err := runCommand(t, NetworksCmd(), "list")
	if err != nil || !strings.Contains(out, "Datacentre 1") || !strings.Contains(out, "online") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err = runCommand(t, NetworksCmd(), "show", "dc1"); err != nil || !strings.Contains(out, "data_class: internal") {
		t.Errorf("show: %q %v", out, err)
	}
	if out, err = runCommand(t, NetworksCmd(), "show", "net_1", "--json"); err != nil || !strings.Contains(out, `"network"`) {
		t.Errorf("show --json: %q %v", out, err)
	}
	if _, err = runCommand(t, NetworksCmd(), "show", "nope"); err == nil || !strings.Contains(err.Error(), "no network named") {
		t.Errorf("unknown network: %v", err)
	}
	if out, err = runCommand(t, NetworksCmd(), "agents", "Datacentre 1"); err != nil || !strings.Contains(out, "nag_1") || !strings.Contains(out, "linux/amd64") {
		t.Errorf("agents: %q %v", out, err)
	}
	out, err = runCommand(t, NetworksCmd(), "allowlist", "dc1")
	if err != nil || !strings.Contains(out, "erp (http)") || !strings.Contains(out, "/invoices/**") || !strings.Contains(out, "employees_*") || !strings.Contains(out, "different allowlists") || !strings.Contains(out, "crm is bound") {
		t.Errorf("allowlist: %q %v", out, err)
	}
	if out, err = runCommand(t, NetworksCmd(), "allowlist", "dc1", "--json"); err != nil || !strings.Contains(out, `"drift":true`) {
		t.Errorf("allowlist --json: %q %v", out, err)
	}
	out, err = runCommand(t, NetworksCmd(), "logs", "dc1", "--agent", "erp-a", "--target", "erp", "--outcome", "ok", "--since", "24h")
	if err != nil || !strings.Contains(out, "aud_1") {
		t.Errorf("logs: %q %v", out, err)
	}
	if _, err = runCommand(t, NetworksCmd(), "logs", "dc1", "--since", "soon"); err == nil {
		t.Error("a bad --since was accepted")
	}
	if out, err = runCommand(t, NetworksCmd(), "logs", "dc1", "--json"); err != nil || !strings.Contains(out, `"logs"`) {
		t.Errorf("logs --json: %q %v", out, err)
	}
}

func TestNetworksEmptyViews(t *testing.T) {
	networksGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"GET /api/admin/networks/net_1/agents":    jsonReply(200, `{"agents":[]}`),
		"GET /api/admin/networks/net_1/allowlist": jsonReply(200, `{"targets":[]}`),
		"GET /api/admin/networks/net_1/logs":      jsonReply(200, `{"logs":[]}`),
	})
	for args, want := range map[string]string{"agents dc1": "No agents", "allowlist dc1": "No agent advertises", "logs dc1": "No log lines"} {
		out, err := runCommand(t, NetworksCmd(), strings.Fields(args)...)
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("%s: %q %v", args, out, err)
		}
	}
}

func TestNetworksWriteCommands(t *testing.T) {
	gw := networksGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"POST /api/admin/networks":                            jsonReply(201, `{"network":{"id":"net_2","name":"HQ"}}`),
		"DELETE /api/admin/networks/net_1":                    jsonReply(200, `{}`),
		"POST /api/admin/networks/net_1/agents/nag_1/approve": jsonReply(200, `{}`),
		"POST /api/admin/networks/net_1/agents/nag_1/rotate":  jsonReply(200, `{}`),
		"POST /api/admin/networks/net_1/agents/nag_1/revoke":  jsonReply(200, `{}`),
		"POST /api/admin/networks/net_1/bindings":             jsonReply(201, `{}`),
		"DELETE /api/admin/networks/net_1/bindings":           jsonReply(200, `{}`),
	})
	out, err := runCommand(t, NetworksCmd(), "create", "HQ", "--description", "head office", "--data-class", "confidential")
	if err != nil || !strings.Contains(out, "Created HQ") {
		t.Fatalf("create: %q %v", out, err)
	}
	if b := gw.body("POST /api/admin/networks", 0); b["name"] != "HQ" || b["description"] != "head office" || b["data_class"] != "confidential" {
		t.Errorf("create body = %v", b)
	}
	if out, err = runCommand(t, NetworksCmd(), "create", "HQ", "--json"); err != nil || !strings.Contains(out, "net_2") {
		t.Errorf("create --json: %q %v", out, err)
	}
	if _, err = runCommand(t, NetworksCmd(), "delete", "dc1"); err == nil {
		t.Error("delete without a terminal or --yes was accepted")
	}
	if out, err = runCommand(t, NetworksCmd(), "delete", "dc1", "--yes"); err != nil || !strings.Contains(out, "Deleted") {
		t.Errorf("delete: %q %v", out, err)
	}
	for action, want := range map[string]string{"approve": "Approved", "rotate": "Rotation requested", "revoke": "Revoked"} {
		args := []string{action, "dc1", "nag_1"}
		if action == "revoke" {
			args = append(args, "--yes")
		}
		if out, err = runCommand(t, NetworksCmd(), args...); err != nil || !strings.Contains(out, want) {
			t.Errorf("%s: %q %v", action, out, err)
		}
	}
	if _, err = runCommand(t, NetworksCmd(), "revoke", "dc1", "nag_1"); err == nil {
		t.Error("revoke without --yes and without a terminal was accepted")
	}

	if out, err = runCommand(t, NetworksCmd(), "bind", "erp-private", "--network", "dc1", "--target", "erp", "--credentials", "agent"); err != nil || !strings.Contains(out, "Bound erp-private") {
		t.Errorf("bind: %q %v", out, err)
	}
	if b := gw.body("POST /api/admin/networks/net_1/bindings", 0); b["service"] != "erp-private" || b["target"] != "erp" || b["credentials_mode"] != "agent" {
		t.Errorf("bind body = %v", b)
	}
	if _, err = runCommand(t, NetworksCmd(), "bind", "mcp:hr", "--network", "dc1", "--target", "hr"); err != nil {
		t.Fatal(err)
	}
	if b := gw.body("POST /api/admin/networks/net_1/bindings", 1); b["mcp_server"] != "hr" || b["credentials_mode"] != "gateway" {
		t.Errorf("mcp bind body = %v", b)
	}
	if out, err = runCommand(t, NetworksCmd(), "unbind", "mcp:hr", "--network", "dc1"); err != nil || !strings.Contains(out, "Unbound mcp:hr") {
		t.Errorf("unbind: %q %v", out, err)
	}
	for _, args := range [][]string{
		{"bind", "erp", "--target", "erp"},
		{"bind", "erp", "--network", "dc1"},
		{"bind", "erp", "--network", "dc1", "--target", "erp", "--credentials", "magic"},
		{"bind", "mcp:", "--network", "dc1", "--target", "x"},
		{"unbind", "erp"},
	} {
		if _, err := runCommand(t, NetworksCmd(), args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestNetworksTokenCreate(t *testing.T) {
	token := "nxt_" + "ABCDEFGH23456"
	gw := networksGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"POST /api/admin/networks/net_1/enrollment-tokens": jsonReply(201, `{"token":"`+token+`","expires_at":"2026-10-16T11:00:00Z","snippets":[{"title":"Linux","text":"sudo zuano-connector enroll --token `+token+`\n"}]}`),
	})
	out, err := runCommand(t, NetworksCmd(), "token", "create", "dc1")
	if err != nil || !strings.Contains(out, token) || !strings.Contains(out, "shown once") || !strings.Contains(out, "Linux") {
		t.Fatalf("token create: %q %v", out, err)
	}
	if b := gw.body("POST /api/admin/networks/net_1/enrollment-tokens", 0); b["mode"] != "single" || b["uses"] != float64(1) || b["ttl_seconds"] != float64(3600) || b["require_approval"] != nil {
		t.Errorf("single body = %v", b)
	}
	if _, err = runCommand(t, NetworksCmd(), "token", "create", "dc1", "--uses", "3", "--ttl", "2h", "--require-approval"); err != nil {
		t.Fatal(err)
	}
	if b := gw.body("POST /api/admin/networks/net_1/enrollment-tokens", 1); b["mode"] != "multi" || b["uses"] != float64(3) || b["ttl_seconds"] != float64(7200) || b["require_approval"] != true {
		t.Errorf("multi body = %v", b)
	}
	if out, err = runCommand(t, NetworksCmd(), "token", "create", "dc1", "--json"); err != nil || !strings.Contains(out, token) {
		t.Errorf("--json: %q %v", out, err)
	}
	for _, args := range [][]string{{"--uses", "0"}, {"--uses", "11"}, {"--ttl", "48h"}, {"--ttl", "0s"}} {
		if _, err := runCommand(t, NetworksCmd(), append([]string{"token", "create", "dc1"}, args...)...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(networkList))
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"token":"leaked"}`))
	})
	if _, err := runCommand(t, NetworksCmd(), "token", "create", "dc1"); err == nil {
		t.Error("a response without an nxt_ token was accepted")
	}
}

func TestNetworksTestCommandExitStatuses(t *testing.T) {
	results := `{"results":[]}`
	networksGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"POST /api/admin/networks/net_1/test": func(s *packServer, w http.ResponseWriter, r *http.Request) { jsonReply(200, results)(s, w, r) },
	})
	_, err := runCommand(t, NetworksCmd(), "test", "dc1")
	if exitCodeOf(err) != 34 || !strings.Contains(err.Error(), "offline") {
		t.Errorf("no agent online: exit %d, %v", exitCodeOf(err), err)
	}
	results = `{"results":[{"agent":"erp-a","ok":true,"rtt_ms":42},{"agent":"erp-b","ok":false,"error":"timeout"}]}`
	out, err := runCommand(t, NetworksCmd(), "test", "dc1")
	if err != nil || !strings.Contains(out, "erp-a: ok, 42 ms") || !strings.Contains(out, "erp-b: failed: timeout") {
		t.Errorf("mixed: %q %v", out, err)
	}
	results = `{"results":[{"agent":"erp-b","ok":false,"error":"timeout"}]}`
	if _, err = runCommand(t, NetworksCmd(), "test", "dc1"); exitCodeOf(err) != 1 {
		t.Errorf("all failed: exit %d, %v", exitCodeOf(err), err)
	}
	if out, err = runCommand(t, NetworksCmd(), "test", "dc1", "--json"); exitCodeOf(err) != 1 || !strings.Contains(out, "results") {
		t.Errorf("--json: %q %v", out, err)
	}
}

func TestNetworksMutationsAreRefusedForAgents(t *testing.T) {
	networksGateway(t, nil)
	t.Setenv("ZUANO_AGENT", "1")
	for _, args := range [][]string{
		{"create", "x"}, {"delete", "dc1", "--yes"}, {"token", "create", "dc1"}, {"approve", "dc1", "a"}, {"rotate", "dc1", "a"},
		{"revoke", "dc1", "a", "--yes"}, {"bind", "x", "--network", "dc1", "--target", "t"}, {"unbind", "x", "--network", "dc1"},
	} {
		if _, err := runCommand(t, NetworksCmd(), args...); exitCodeOf(err) != exitDenied {
			t.Errorf("%v by an agent: exit %d, %v", args, exitCodeOf(err), err)
		}
	}
}

func TestOfflineNetworkRefusalExitsThirtyFour(t *testing.T) {
	for _, reason := range []string{"network_offline", "egress_unavailable"} {
		if err := policyRefusal([]byte(`{"reason":"`+reason+`","message":"DC1 is offline"}`), nil); exitCodeOf(err) != 34 {
			t.Errorf("%s: %v", reason, err)
		}
	}
}

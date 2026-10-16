package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
)

func TestDoctorFlagsServersTheOrganisationRequiresRouted(t *testing.T) {
	home := syncHome(t)
	keyring.MockInit()
	writeTestFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"files": {"command": "npx", "args": ["-y", "@acme/files"]}}}`)
	var body string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fw/local/requirements" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	if err := config.SetGatewayURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	c := client.New(srv.URL, "tok")
	rows := map[string]string{}
	oks := map[string]bool{}
	run := func() {
		rows, oks = map[string]string{}, map[string]bool{}
		doctorFirewallRequired(context.Background(), c, srv.URL, func(n string, ok bool, d string) { rows[n], oks[n] = d, ok })
	}

	body = `{"requiredRouting": false}`
	if run(); len(rows) != 0 {
		t.Errorf("a row appeared although routing is not required: %v", rows)
	}
	status = http.StatusNotFound
	if run(); len(rows) != 0 {
		t.Errorf("a gateway without the endpoint produced a row: %v", rows)
	}

	status, body = http.StatusOK, `{"requiredRouting": true}`
	run()
	if oks["firewall-required"] || !strings.Contains(rows["firewall-required"], "claude: files") || !strings.Contains(rows["firewall-required"], "firewall import-local <name>") {
		t.Errorf("unrouted server: ok=%v %q", oks["firewall-required"], rows["firewall-required"])
	}

	// Routing the servers makes the row pass.
	if out, err := runCommand(t, FirewallCmd(), "import-local", "files", "--from", "claude", "--to", "claude", "--yes"); err != nil {
		t.Fatalf("import-local: %v\n%s", err, out)
	}
	if run(); !oks["firewall-required"] || !strings.Contains(rows["firewall-required"], "every MCP server is") {
		t.Errorf("routed: ok=%v %q", oks["firewall-required"], rows["firewall-required"])
	}
}

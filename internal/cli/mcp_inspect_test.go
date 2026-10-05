package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMcpInspectCredentialSafeProjection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "test-token")
	descriptor := map[string]any{"source": "url", "transport": "http", "endpoint": "https://example.com/mcp?token=synthetic-secret", "authRef": "private-reference", "authType": "api_key", "authScope": "user", "declaredTools": []string{"one"}, "credentials": "synthetic-secret"}
	content, _ := json.Marshal(descriptor)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/catalog/res_test" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": map[string]any{"id": "res_test", "type": "mcp_server", "name": "Test", "credential": "synthetic-secret"}, "currentVersion": map[string]any{"semver": "1.2.3", "manifestJson": map[string]any{"content": string(content)}}})
	}))
	defer srv.Close()
	cmd := McpInspectCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_test", "--gateway-url", srv.URL, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic-secret", "private-reference", "example.com"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("disclosed %s", secret)
		}
	}
	var result mcpInspection
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Transport != "http" || result.AuthType != "api_key" || !result.HasAuthReference || result.DeclaredToolCount != 1 {
		t.Fatalf("unexpected metadata: %+v", result)
	}
}

func TestMcpInspectRejectsInvalidOrDeniedResources(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"denied", "synthetic-secret", 404},
		{"wrongtype", `{"resource":{"id":"res_test","type":"skill"}}`, 200},
		{"wrongid", `{"resource":{"id":"res_other","type":"mcp_server"}}`, 200},
		{"missing", "{}", 200},
		{"badjson", "synthetic-secret", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GATEWAY_API_KEY", "test-token")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			cmd := McpInspectCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"res_test", "--gateway-url", srv.URL, "--json"})
			err := cmd.Execute()
			if err == nil || strings.Contains(out.String()+err.Error(), "synthetic-secret") {
				t.Fatalf("unsafe result: %v %s", err, out.String())
			}
		})
	}
}

func TestMcpDescriptorContract(t *testing.T) {
	valid := mcpDescriptor{Transport: "http", Endpoint: "https://example.com/mcp", Source: "url", AuthType: "api_key", AuthRef: "catalog-service", AuthScope: "user"}
	if err := validateMcpDescriptor(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*mcpDescriptor){
		"source":      func(d *mcpDescriptor) { d.Source = "bad" },
		"scope":       func(d *mcpDescriptor) { d.AuthScope = "bad" },
		"auth":        func(d *mcpDescriptor) { d.AuthType = "bad" },
		"missing_ref": func(d *mcpDescriptor) { d.AuthRef = "" },
		"oauth_ref":   func(d *mcpDescriptor) { d.AuthType = "oauth" },
		"raw_secret":  func(d *mcpDescriptor) { d.AuthRef = "sk-secret" },
		"credentials": func(d *mcpDescriptor) { d.Endpoint = "https://user:secret@example.com" },
		"scheme":      func(d *mcpDescriptor) { d.Endpoint = "file:///tmp/file" },
		"command":     func(d *mcpDescriptor) { d.Command = "node server.js" },
	} {
		t.Run(name, func(t *testing.T) {
			d := valid
			mutate(&d)
			if err := validateMcpDescriptor(d); err == nil {
				t.Fatal("invalid descriptor accepted")
			}
		})
	}
	repo := mcpDescriptor{Transport: "http", Source: "repo"}
	if err := validateMcpDescriptor(repo); err != nil {
		t.Fatalf("repo endpoint is resolved after hosting: %v", err)
	}
}

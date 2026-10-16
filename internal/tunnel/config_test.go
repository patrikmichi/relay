package tunnel

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const sampleConfig = `
version: 1
network: net_01JB
name: erp-dc1-a
state_dir: /var/lib/zuano-connector
broker:
  ca_file: /etc/ssl/corp-proxy-ca.pem
  proxy: http://proxy.corp:3128
max_concurrency: 16
shutdown_grace: 30s
log:
  format: json
  upload: errors
health:
  listen: 127.0.0.1:9464
update:
  auto: false
  channel: stable
  window: "Sat 02:00-04:00 Europe/Prague"
targets:
  - name: erp
    kind: http
    url: https://erp.internal.example:8443/api/v2
    cidrs: [10.20.0.0/16]
    methods: [get, POST, PATCH]
    paths: ["/invoices/**", "/customers/**"]
    timeout: 60s
    tls: { ca_file: /etc/ssl/internal-ca.pem, server_name: erp.internal.example }
    credentials:
      header: Authorization
      value: "Bearer ${file:/run/secrets/erp-token}"
    rate: { per_second: 20, burst: 40 }
  - name: hr-mcp
    kind: mcp_stdio
    command: /opt/hr-mcp/bin/hr-mcp
    args: ["--readonly"]
    env: { HR_API_TOKEN: "${file:/run/secrets/hr-token}" }
    mcp: { tools: ["employees_*", "leave_*"] }
  - name: crm-mcp
    kind: mcp_http
    url: http://crm-mcp.internal:3000/mcp
    cidrs: [10.30.4.0/24]
    mcp: { methods: [initialize, tools/list, tools/call, ping] }
`

func TestParseConfigAcceptsTheDocumentedExample(t *testing.T) {
	cfg, err := ParseConfig([]byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network != "net_01JB" || len(cfg.Targets) != 3 || cfg.MaxConcurrency != 16 || time.Duration(cfg.ShutdownGrace) != 30*time.Second {
		t.Errorf("config = %+v", cfg)
	}
	erp, _ := cfg.Target("erp")
	if erp.Methods[0] != "GET" || time.Duration(erp.Timeout) != time.Minute || erp.Rate.Burst != 40 {
		t.Errorf("erp = %+v", erp)
	}
	hr, _ := cfg.Target("hr-mcp")
	if len(hr.MCP.Methods) != 4 || time.Duration(hr.IdleTimeout) != 15*time.Minute {
		t.Errorf("mcp defaults = %+v", hr)
	}
	if _, ok := cfg.Target("nope"); ok {
		t.Error("an unknown target was found")
	}
}

func TestAllowlistAdvertisesNamesNotLocations(t *testing.T) {
	cfg, err := ParseConfig([]byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	al := cfg.Allowlist()
	raw, _ := json.Marshal(al)
	for _, secret := range []string{"erp.internal", "10.20.0.0", "/run/secrets", "Bearer", "hr-mcp/bin", "proxy.corp"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the allowlist leaks %q: %s", secret, raw)
		}
	}
	if len(al.Targets) != 3 || al.Targets[0].Name != "crm-mcp" || len(al.Hash) != 16 {
		t.Errorf("allowlist = %s", raw)
	}
	cfg2, _ := ParseConfig([]byte(strings.Replace(sampleConfig, "/customers/**", "/orders/**", 1)))
	if cfg2.Allowlist().Hash == al.Hash {
		t.Error("a changed allowlist kept its hash")
	}
	cfg3, _ := ParseConfig([]byte(strings.Replace(sampleConfig, "erp.internal.example:8443", "other.example:9", 1)))
	if cfg3.Allowlist().Hash != al.Hash {
		t.Error("a changed URL changed the hash: the hash is of what is advertised")
	}
}

func TestParseConfigRefusals(t *testing.T) {
	min := func(target string) string {
		return "version: 1\nnetwork: net_1\ntargets:\n" + target
	}
	for name, doc := range map[string]string{
		"unknown top key":    "version: 1\nnetwork: net_1\ntargets: []\nsurprise: 1\n",
		"unknown target key": min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    hosts: [x]\n"),
		"wrong version":      "version: 2\nnetwork: net_1\ntargets: []\n",
		"missing network":    "version: 1\ntargets: []\n",
		"bad network":        "version: 1\nnetwork: erp\ntargets: []\n",
		"bad kind":           min("  - name: a\n    kind: ftp\n"),
		"bad name":           min("  - name: Bad Name\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n"),
		"duplicate":          min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n  - name: a\n    kind: http\n    url: https://b.example\n    methods: [GET]\n    paths: [\"/**\"]\n"),
		"no url":             min("  - name: a\n    kind: http\n    methods: [GET]\n    paths: [\"/**\"]\n"),
		"url with login":     min("  - name: a\n    kind: http\n    url: https://u:p@a.example\n    methods: [GET]\n    paths: [\"/**\"]\n"),
		"url with query":     min("  - name: a\n    kind: http\n    url: https://a.example/x?y=1\n    methods: [GET]\n    paths: [\"/**\"]\n"),
		"ftp scheme":         min("  - name: a\n    kind: http\n    url: ftp://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n"),
		"no methods":         min("  - name: a\n    kind: http\n    url: https://a.example\n    paths: [\"/**\"]\n"),
		"no paths":           min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n"),
		"bad method":         min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [FETCH]\n    paths: [\"/**\"]\n"),
		"bad path":           min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [invoices]\n"),
		"bad cidr":           min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    cidrs: [10.0.0.0/99]\n"),
		"stdio no command":   min("  - name: a\n    kind: mcp_stdio\n"),
		"stdio with url":     min("  - name: a\n    kind: mcp_stdio\n    command: x\n    url: https://a.example\n"),
		"http with command":  min("  - name: a\n    kind: mcp_http\n    url: https://a.example\n    command: x\n"),
		"skip verify alone":  min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    tls: { insecure_skip_verify: true }\n"),
		"half client cert":   min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    tls: { client_cert: /c }\n"),
		"two credentials":    min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    credentials: { bearer: x, basic: { username: u, password: p } }\n"),
		"half header cred":   min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    credentials: { header: X }\n"),
		"bad rate":           min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    rate: { per_second: 0, burst: 1 }\n"),
		"bad duration":       min("  - name: a\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    timeout: soon\n"),
		"bad log format":     "version: 1\nnetwork: net_1\ntargets: []\nlog: { format: xml }\n",
		"bad window":         "version: 1\nnetwork: net_1\ntargets: []\nupdate: { window: whenever }\n",
		"bad health":         "version: 1\nnetwork: net_1\ntargets: []\nhealth: { listen: nonsense }\n",
		"bad broker":         "version: 1\nnetwork: net_1\ntargets: []\nbroker: { url: ftp://b }\n",
		"not yaml":           "version: [",
		"non text key":       "1: a\nversion: 1\n",
	} {
		_, err := ParseConfig([]byte(doc))
		var ce *ErrConfig
		if !errors.As(err, &ce) || len(ce.Problems) == 0 || !strings.Contains(ce.Error(), "not valid") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A loopback broker is allowed for the test harness.
	if _, err := ParseConfig([]byte("version: 1\nnetwork: net_1\ntargets: []\nbroker: { url: 'http://127.0.0.1:8787' }\n")); err != nil {
		t.Errorf("loopback broker: %v", err)
	}
	if _, err := ParseConfig([]byte("version: 1\nnetwork: net_1\ntargets: []\n")); err != nil {
		t.Errorf("minimal config: %v", err)
	}
}

func TestLoadConfigRefusesGroupWritableFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nnetwork: net_1\ntargets: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.path != path {
		t.Fatalf("a 0644 file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o664); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "writable by group or other") {
			t.Errorf("a group-writable file: %v", err)
		}
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file loaded")
	}
	t.Setenv("ZUANO_CONNECTOR_CONFIG", "/x/y.yaml")
	if DefaultConfigPath() != "/x/y.yaml" {
		t.Error("ZUANO_CONNECTOR_CONFIG is ignored")
	}
	t.Setenv("ZUANO_CONNECTOR_CONFIG", "")
	if !strings.HasSuffix(filepath.ToSlash(DefaultConfigPath()), "config.yaml") {
		t.Error("default path")
	}
	t.Setenv("ZUANO_CONNECTOR_STATE_DIR", "/s")
	if DefaultStateDir() != "/s" {
		t.Error("ZUANO_CONNECTOR_STATE_DIR is ignored")
	}
	t.Setenv("ZUANO_CONNECTOR_STATE_DIR", "")
	if DefaultStateDir() == "" || BinaryName != "zuano-connector" {
		t.Error("state dir or binary name")
	}
	if len(ConfigSchema()) == 0 {
		t.Error("no schema")
	}
}

func TestResolveSecret(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONNECTOR_TEST_ENV", "fromenv")
	got, err := ResolveSecret("Bearer ${file:" + file + "} and ${env:CONNECTOR_TEST_ENV}")
	if err != nil || got != "Bearer s3cret and fromenv" {
		t.Fatalf("got %q, %v", got, err)
	}
	// A rotated file is picked up on the next call.
	_ = os.WriteFile(file, []byte("rotated"), 0o600)
	if got, _ := ResolveSecret("${file:" + file + "}"); got != "rotated" {
		t.Errorf("rotated = %q", got)
	}
	if _, err := ResolveSecret("${file:" + filepath.Join(dir, "missing") + "}"); err == nil {
		t.Error("a missing file resolved")
	}
	if _, err := ResolveSecret("${env:CONNECTOR_TEST_UNSET_VALUE}"); err == nil {
		t.Error("an unset variable resolved")
	}
	if got, err := ResolveSecret("plain"); got != "plain" || err != nil {
		t.Errorf("plain = %q %v", got, err)
	}
}

func TestUpdateWindow(t *testing.T) {
	w, err := ParseWindow("Sat 02:00-04:00 Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	prague, _ := time.LoadLocation("Europe/Prague")
	in := time.Date(2026, 10, 17, 3, 0, 0, 0, prague) // a Saturday
	if !w.Contains(in) || w.Contains(in.Add(2*time.Hour)) || w.Contains(in.AddDate(0, 0, 1)) {
		t.Error("window membership")
	}
	night, err := ParseWindow("22:00-02:00")
	if err != nil {
		t.Fatal(err)
	}
	if !night.Contains(time.Date(2026, 10, 17, 23, 0, 0, 0, time.UTC)) || !night.Contains(time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)) || night.Contains(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)) {
		t.Error("a window across midnight")
	}
	for _, bad := range []string{"", "Sat", "02:00", "25:00-26:00", "Sat 02:00-04:00 Nowhere/City", "a b c d"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

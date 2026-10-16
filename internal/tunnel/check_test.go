package tunnel

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkFindings(t *testing.T, yamlTargets string, res Resolver) []Finding {
	t.Helper()
	cfg, err := ParseConfig([]byte("version: 1\nnetwork: net_1\ntargets:\n" + yamlTargets))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		res = net.DefaultResolver
	}
	return CheckTargets(context.Background(), cfg, res)
}

func hasFinding(fs []Finding, level, contains string) bool {
	for _, f := range fs {
		if f.Level == level && strings.Contains(f.Msg, contains) {
			return true
		}
	}
	return false
}

func TestCheckTargets(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tlsSrv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw}), 0o600)
	target := func(name, url, extra string) string {
		return "  - name: " + name + "\n    kind: http\n    url: " + url + "\n    methods: [GET]\n    paths: [\"/**\"]\n    allow_loopback: true\n" + extra
	}

	if fs := checkFindings(t, target("ok", tlsSrv.URL, "    tls: { ca_file: "+ca+" }\n"), nil); HasErrors(fs) {
		t.Errorf("a reachable TLS target: %v", fs)
	}
	if fs := checkFindings(t, target("noca", tlsSrv.URL, ""), nil); !hasFinding(fs, "error", "TLS handshake") {
		t.Errorf("untrusted certificate: %v", fs)
	}
	if fs := checkFindings(t, target("skip", tlsSrv.URL, "    allow_insecure: true\n    tls: { insecure_skip_verify: true }\n"), nil); HasErrors(fs) || !hasFinding(fs, "warn", "not checked") {
		t.Errorf("insecure opt-in: %v", fs)
	}
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	if fs := checkFindings(t, target("plain", plain.URL, ""), nil); HasErrors(fs) || !hasFinding(fs, "warn", "without TLS") {
		t.Errorf("plain http: %v", fs)
	}
	if fs := checkFindings(t, target("lo", plain.URL, "    cidrs: [10.0.0.0/8]\n"), nil); !hasFinding(fs, "error", "outside the target's cidrs") {
		t.Errorf("outside cidrs: %v", fs)
	}
	if fs := checkFindings(t, "  - name: meta\n    kind: http\n    url: http://169.254.169.254\n    methods: [GET]\n    paths: [\"/**\"]\n", nil); !hasFinding(fs, "error", "cloud metadata") {
		t.Errorf("metadata: %v", fs)
	}
	if fs := checkFindings(t, "  - name: nodns\n    kind: http\n    url: https://erp.internal.example\n    methods: [GET]\n    paths: [\"/**\"]\n", &fakeResolver{answers: [][]net.IPAddr{nil}}); !hasFinding(fs, "error", "does not resolve") {
		t.Errorf("dns: %v", fs)
	}
	if fs := checkFindings(t, target("badca", tlsSrv.URL, "    tls: { ca_file: "+filepath.Join(dir, "none.pem")+" }\n"), nil); !hasFinding(fs, "error", "tls.ca_file") {
		t.Errorf("missing ca: %v", fs)
	}
	empty := filepath.Join(dir, "empty.pem")
	_ = os.WriteFile(empty, []byte("x"), 0o600)
	if fs := checkFindings(t, target("emptyca", tlsSrv.URL, "    tls: { ca_file: "+empty+" }\n"), nil); !hasFinding(fs, "error", "no certificate") {
		t.Errorf("empty ca: %v", fs)
	}
	if fs := checkFindings(t, target("cert", tlsSrv.URL, "    tls: { ca_file: "+ca+", client_cert: "+filepath.Join(dir, "a")+", client_key: "+filepath.Join(dir, "b")+" }\n"), nil); !hasFinding(fs, "error", "client certificate") {
		t.Errorf("client cert: %v", fs)
	}

	secret := filepath.Join(dir, "token")
	_ = os.WriteFile(secret, []byte("x"), 0o600)
	creds := "    credentials: { header: Authorization, value: \"Bearer ${file:" + filepath.Join(dir, "missing") + "}\" }\n"
	if fs := checkFindings(t, target("c1", plain.URL, creds), nil); !hasFinding(fs, "error", "credentials") {
		t.Errorf("missing credential file: %v", fs)
	}
	if fs := checkFindings(t, target("c2", plain.URL, "    credentials: { bearer: \"${env:CHECK_TEST_UNSET}\" }\n"), nil); !hasFinding(fs, "error", "CHECK_TEST_UNSET") {
		t.Errorf("unset bearer variable: %v", fs)
	}
	if fs := checkFindings(t, target("c3", plain.URL, "    credentials: { basic: { username: u, password: \"${file:"+secret+"}\" } }\n"), nil); HasErrors(fs) {
		t.Errorf("basic credentials: %v", fs)
	}
	if fs := checkFindings(t, target("c4", plain.URL, "    credentials: { basic: { username: u, password: \"${env:CHECK_TEST_UNSET}\" } }\n"), nil); !hasFinding(fs, "error", "credentials") {
		t.Errorf("basic with a missing secret: %v", fs)
	}

	stdio := "  - name: hr\n    kind: mcp_stdio\n    command: /nonexistent/hr-mcp\n    env: { T: \"${file:/nonexistent/t}\" }\n    cwd: /nonexistent/dir\n"
	fs := checkFindings(t, stdio, nil)
	if !hasFinding(fs, "error", "was not found") || !hasFinding(fs, "error", "env T") || !hasFinding(fs, "error", "cwd") {
		t.Errorf("stdio: %v", fs)
	}
	if f := fs[0].String(); !strings.HasPrefix(f, "error: hr: ") {
		t.Errorf("String = %q", f)
	}
	if fs := checkFindings(t, "  - name: sh\n    kind: mcp_stdio\n    command: sh\n", nil); HasErrors(fs) {
		t.Errorf("a command on PATH: %v", fs)
	}
}

func TestRedactedConfigHidesLiteralSecrets(t *testing.T) {
	cfg, err := ParseConfig([]byte(sampleConfig + "  - name: lit\n    kind: http\n    url: https://a.example\n    methods: [GET]\n    paths: [\"/**\"]\n    credentials: { basic: { username: svc, password: hunter2 } }\n  - name: lit2\n    kind: mcp_stdio\n    command: x\n    env: { API_KEY: plain-value }\n  - name: lit3\n    kind: http\n    url: https://b.example\n    methods: [GET]\n    paths: [\"/**\"]\n    credentials: { bearer: raw-bearer-value }\n"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := cfg.PrintConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "plain-value", "raw-bearer-value", "svc"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("the printed config leaks %q", leak)
		}
	}
	if !strings.Contains(string(out), "${file:/run/secrets/erp-token}") || !strings.Contains(string(out), "<literal hidden>") {
		t.Errorf("printed config:\n%s", out)
	}
	if cfg.Targets[3].Credentials.Basic.Password != "hunter2" {
		t.Error("redaction changed the running config")
	}
	if got := UnservedHints(cfg, []string{"erp", "payroll"}); len(got) != 1 || got[0] != "payroll" {
		t.Errorf("hints = %v", got)
	}
	if err := CheckAddress(netip.MustParseAddr("169.254.169.254"), Target{}); err == nil {
		t.Error("metadata address allowed")
	}
	if err := CheckAddress(netip.MustParseAddr("10.1.2.3"), Target{CIDRs: []string{"10.0.0.0/8"}}); err != nil {
		t.Errorf("an address inside the cidrs: %v", err)
	}
	if ErrFindings == nil {
		t.Error("ErrFindings")
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupDoctor(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_URL", "")
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	t.Chdir(t.TempDir())
	previous := Offline()
	SetOffline(false)
	t.Cleanup(func() { SetOffline(previous) })
	return home
}

func runDoctorJSON(t *testing.T) (string, error) {
	t.Helper()
	cmd := DoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--json"})
	err := cmd.Execute()
	var parsed struct {
		Healthy bool
		Checks  []doctorCheck
	}
	if decodeErr := json.Unmarshal(out.Bytes(), &parsed); decodeErr != nil {
		t.Fatalf("invalid JSON: %v: %s", decodeErr, out.String())
	}
	if parsed.Healthy != (err == nil) {
		t.Fatalf("health and exit status disagree: %s %v", out.String(), err)
	}
	return out.String(), err
}

func TestDoctorLocalAndOverrideDiagnostics(t *testing.T) {
	home := setupDoctor(t)
	output, err := runDoctorJSON(t)
	if err != nil || !strings.Contains(output, "local checks only") {
		t.Fatalf("%v: %s", err, output)
	}
	dir := filepath.Join(home, ".config", "relay", "providers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.yml"), []byte("id: ["), 0600); err != nil {
		t.Fatal(err)
	}
	output, err = runDoctorJSON(t)
	if err == nil || !strings.Contains(output, "broken.yml") || !strings.Contains(output, "malformed override") {
		t.Fatalf("%v: %s", err, output)
	}
}

func TestDoctorGatewayProbeAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"authenticated", `{"email":"test@example.com"}`, 200, true},
		{"denied", "synthetic-secret-response", 403, false},
		{"malformed", `{"unexpected":"synthetic-secret-response"}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDoctor(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/cli/whoami" || r.Header.Get("Authorization") != "Bearer synthetic-secret-token" {
					t.Errorf("unexpected request")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			t.Setenv("GATEWAY_URL", srv.URL)
			t.Setenv("GATEWAY_API_KEY", "synthetic-secret-token")
			output, err := runDoctorJSON(t)
			if (err == nil) != tc.ok || strings.Contains(output, "synthetic-secret") {
				t.Fatalf("%v: %s", err, output)
			}
		})
	}
}

func TestDoctorOfflineAndCancellation(t *testing.T) {
	setupDoctor(t)
	t.Setenv("GATEWAY_URL", "http://127.0.0.1:1")
	t.Setenv("GATEWAY_API_KEY", "secret")
	SetOffline(true)
	output, err := runDoctorJSON(t)
	if err != nil || !strings.Contains(output, "offline mode") {
		t.Fatalf("%v: %s", err, output)
	}
	SetOffline(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, check := range doctorChecks(ctx) {
		if check.Name == "gateway" {
			if check.OK {
				t.Fatal("canceled probe succeeded")
			}
			return
		}
	}
	t.Fatal("missing gateway check")
}

func TestDoctorDirectoryPermissions(t *testing.T) {
	dir := t.TempDir()
	if !doctorDirectoryWritable(filepath.Join(dir, "missing", "child")) {
		t.Fatal("writable ancestor rejected")
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0700) }()
	if doctorDirectoryWritable(dir) {
		t.Fatal("read-only dir accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if doctorDirectoryWritable(filepath.Join(file, "child")) {
		t.Fatal("file parent accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if doctorDirectoryWritable(link) {
		t.Fatal("symlink accepted")
	}
}

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

func doctorCheckNamed(t *testing.T, output, name string) doctorCheck {
	t.Helper()
	var parsed struct{ Checks []doctorCheck }
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatal(err)
	}
	for _, c := range parsed.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %s", name, output)
	return doctorCheck{}
}

func TestDoctor_TextOutputMarksFailures(t *testing.T) {
	setupDoctor(t)
	writeCorruptConfig(t)

	out, err := runCommand(t, DoctorCmd())
	if err == nil || err.Error() != "doctor found failing checks" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out, "fail  gateway: gateway configuration is invalid") || !strings.Contains(out, "ok  recovery: 0 transactions need recovery") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestDoctor_UnreadableManifestAndPendingRecovery(t *testing.T) {
	home := setupDoctor(t)
	interruptedJournal(t, t.TempDir(), "a.md")
	if err := os.WriteFile(filepath.Join(home, ".config", "relay", "agentport-manifest.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runDoctorJSON(t)
	if err == nil {
		t.Fatalf("expected failing checks: %s", output)
	}
	if c := doctorCheckNamed(t, output, "manifest"); c.OK {
		t.Errorf("corrupt manifest reported healthy: %+v", c)
	}
	if c := doctorCheckNamed(t, output, "recovery"); c.OK || c.Detail != "1 transactions need recovery" {
		t.Errorf("pending journal not reported: %+v", c)
	}
}

func TestDoctor_ReportsTamperedInstallation(t *testing.T) {
	home := setupDoctor(t)
	writeSkillFiles(t, filepath.Join(home, ".claude", "skills", "plain"), map[string][]byte{"SKILL.md": []byte("---\nname: plain\ndescription: A plain skill.\n---\nDo it.\n")})
	if _, err := runCommand(t, SkillMigrateCmd(), "plain", "--from", "claude", "--to", "codex"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	output, err := runDoctorJSON(t)
	if err != nil || !strings.Contains(output, "1 current installations") || !strings.Contains(output, "hashes match") {
		t.Fatalf("fresh install should be healthy: %v %s", err, output)
	}

	if err := os.WriteFile(filepath.Join(home, ".agents", "skills", "plain", "SKILL.md"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err = runDoctorJSON(t)
	if err == nil || strings.Contains(output, "hashes match") {
		t.Fatalf("tampered install should fail: %v %s", err, output)
	}
}

func TestDoctor_SessionChecks(t *testing.T) {
	whoami := func() *httptest.Server {
		srv := httptest.NewServer(jsonHandler(http.StatusOK, map[string]string{"email": "dev@example.com"}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("not logged in", func(t *testing.T) {
		setupDoctor(t)
		t.Setenv("GATEWAY_URL", whoami().URL)
		output, err := runDoctorJSON(t)
		if c := doctorCheckNamed(t, output, "gateway"); err == nil || c.OK || !strings.Contains(c.Detail, "run relay login") {
			t.Fatalf("%v %+v", err, c)
		}
	})

	t.Run("unreachable gateway", func(t *testing.T) {
		setupDoctor(t)
		srv := whoami()
		srv.Close()
		t.Setenv("GATEWAY_URL", srv.URL)
		t.Setenv("GATEWAY_API_KEY", syntheticKey("doctor"))
		output, _ := runDoctorJSON(t)
		if c := doctorCheckNamed(t, output, "gateway"); c.OK || c.Detail != "gateway request failed" {
			t.Fatalf("%+v", c)
		}
	})

	for name, expiry := range map[string]time.Duration{"valid token": time.Hour, "expired token": -time.Hour} {
		t.Run(name, func(t *testing.T) {
			setupDoctor(t)
			srv := whoami()
			t.Setenv("GATEWAY_URL", srv.URL)
			origin, _ := config.NormalizeGatewayURL(srv.URL)
			email := "doctor@example.com"
			t.Setenv("RELAY_EMAIL", email)
			if err := keychain.WriteToken(origin, email, keychain.TokenData{AccessToken: syntheticKey("access"), ExpiresAt: time.Now().Add(expiry).Unix()}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = keychain.DeleteToken(origin, email) })

			output, err := runDoctorJSON(t)
			token := doctorCheckNamed(t, output, "token")
			if token.OK != (expiry > 0) || (err == nil) != (expiry > 0) {
				t.Fatalf("token check %+v, err %v", token, err)
			}
			if !doctorCheckNamed(t, output, "gateway").OK {
				t.Error("gateway probe should succeed")
			}
		})
	}
}

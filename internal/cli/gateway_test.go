package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// withLoggedInSession sets RELAY_EMAIL and writes a matching token into the
// (mocked, see call_test.go's keyring.MockInit init()) OS keychain, so
// resolveClient succeeds without ever dialing the network — needed to reach
// the Offline()/resolveGatewayURLOrFailClosed branch in commands (logout,
// tokens revoke) that build the client BEFORE checking offline state, since
// resolveClient calls os.Exit(1) on ErrNotLoggedIn and would otherwise kill
// the test binary.
func withLoggedInSession(t *testing.T) (email string) {
	t.Helper()
	email = "offline-test@example.com"
	t.Setenv("RELAY_EMAIL", email)
	if err := keychain.WriteToken(email, keychain.TokenData{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		Email:        email,
	}); err != nil {
		t.Fatalf("keychain.WriteToken: %v", err)
	}
	t.Cleanup(func() { _ = keychain.DeleteToken(email) })
	return email
}

// withNoGateway simulates a public build with no baked-in gateway and no
// env/config override: config.DefaultGatewayURL is emptied for the
// duration of the test (restored on cleanup), $GATEWAY_URL is unset, and
// $HOME points at a fresh temp dir so no ~/.config/relay/config.json is
// picked up.
func withNoGateway(t *testing.T) {
	t.Helper()
	orig := config.DefaultGatewayURL
	config.DefaultGatewayURL = ""
	t.Cleanup(func() { config.DefaultGatewayURL = orig })
	t.Setenv("GATEWAY_URL", "")
	t.Setenv("HOME", t.TempDir())
}

func TestResolveGatewayURLOrFailClosed_EmptyDefault_FailsClosed(t *testing.T) {
	withNoGateway(t)

	_, err := resolveGatewayURLOrFailClosed("")
	if err == nil {
		t.Fatalf("expected an error when no gateway URL can be resolved")
	}
	if !strings.Contains(err.Error(), offlineGuidance) {
		t.Errorf("expected error to contain offlineGuidance, got: %v", err)
	}
}

func TestResolveGatewayURLOrFailClosed_OverrideWins(t *testing.T) {
	withNoGateway(t)

	got, err := resolveGatewayURLOrFailClosed("https://override.example.com")
	if err != nil {
		t.Fatalf("resolveGatewayURLOrFailClosed: %v", err)
	}
	if got != "https://override.example.com" {
		t.Errorf("got %q, want override", got)
	}
}

func TestResolveGatewayURLOrFailClosed_FallsBackToDefaultWhenSet(t *testing.T) {
	withNoGateway(t)
	config.DefaultGatewayURL = "https://compiled-default.example.com"

	got, err := resolveGatewayURLOrFailClosed("")
	if err != nil {
		t.Fatalf("resolveGatewayURLOrFailClosed: %v", err)
	}
	if got != "https://compiled-default.example.com" {
		t.Errorf("got %q, want compiled default", got)
	}
}

// ---- catalog verbs must fail closed with offlineGuidance ----

func TestSkillInstall_CatalogVerb_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	_, err := fetchFromGateway("res_never_reached", skillInstallOpts{})
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestSkillSearch_CatalogVerb_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runSkillSearch(cmd, "", "", false)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestPublish_CatalogVerb_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	_, _, err := resolvePublishDoerWithURL("")
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestSync_CatalogVerb_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	_, err := resolveSyncDoer("")
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestServices_CatalogVerb_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := ServicesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestCall_CatalogVerb_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := CallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, []string{"clockify", "list_time_entries"})
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

// ---- root --offline flag must fail closed regardless of a configured gateway ----

// withOffline simulates the root --offline persistent flag being set (what
// cmd/relay/main.go's PersistentPreRunE does after parsing --offline),
// restoring the previous value on cleanup. A configured, reachable gateway
// (env var) is set up alongside it so these tests prove --offline wins even
// when a gateway URL would otherwise resolve successfully.
func withOffline(t *testing.T) {
	t.Helper()
	t.Setenv("GATEWAY_URL", "https://configured.example.com")
	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })
}

func TestResolveGatewayURLOrFailClosed_OfflineFlag_WinsOverConfiguredGateway(t *testing.T) {
	withOffline(t)

	_, err := resolveGatewayURLOrFailClosed("https://explicit-override.example.com")
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error even with an explicit override, got: %v", err)
	}
}

func TestSkillSearch_OfflineFlag_FailsClosedEvenWithConfiguredGateway(t *testing.T) {
	withOffline(t)

	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runSkillSearch(cmd, "", "", false)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestServices_OfflineFlag_FailsClosedEvenWithConfiguredGateway(t *testing.T) {
	withOffline(t)

	cmd := ServicesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

// ---- the 7 newly-routed gateway commands must fail closed too (Fix 1) ----
//
// login, whoami, authorize, and help-tools are pure gateway commands (no
// local-only half) — both "no gateway configured" and "--offline" must
// produce the offlineGuidance error before any network call. logout and
// tokens revoke have a local-only half (deleting the keychain entry) that
// must keep working; their fail-closed behavior is scoped to the
// server-side revoke and tested separately below.

func TestLogin_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := LoginCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestLogin_OfflineFlag_FailsClosedEvenWithConfiguredGateway(t *testing.T) {
	withOffline(t)

	cmd := LoginCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestWhoami_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := WhoamiCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestWhoami_OfflineFlag_FailsClosedEvenWithConfiguredGateway(t *testing.T) {
	withOffline(t)

	cmd := WhoamiCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestAuthorize_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := AuthorizeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, []string{"google-workspace"})
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestAuthorize_OfflineFlag_FailsClosedEvenWithConfiguredGateway(t *testing.T) {
	withOffline(t)

	cmd := AuthorizeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, []string{"google-workspace"})
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestHelpTools_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := HelpToolsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestHelpTools_OfflineFlag_FailsClosedEvenWithConfiguredGateway(t *testing.T) {
	withOffline(t)

	cmd := HelpToolsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestTokensList_FailsClosedWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	cmd := TokensCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"list"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

func TestTokensList_OfflineFlag_FailsClosedEvenWithConfiguredGateway(t *testing.T) {
	withOffline(t)

	cmd := TokensCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"list"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
}

// ---- logout / tokens revoke: local keychain-clear half must survive
// --offline; only the server-side revoke is fail-closed ----

func TestLogout_OfflineFlag_SkipsServerRevokeButStillClearsLocalSession(t *testing.T) {
	email := withLoggedInSession(t)
	withOffline(t)

	cmd := LogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	stdout := captureStdout(t, func() {
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("LogoutCmd offline: %v", err)
		}
	})
	if !strings.Contains(stdout, "Offline") || !strings.Contains(stdout, email) {
		t.Errorf("expected offline-skip message mentioning %s, got: %q", email, stdout)
	}
	if _, err := keychain.ReadToken(email); err == nil {
		t.Errorf("expected the keychain entry for %s to be deleted", email)
	}
}

func TestLogout_FailsClosedWhenNoGatewayConfigured(t *testing.T) {
	email := withLoggedInSession(t)
	withNoGateway(t)

	cmd := LogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
	// The local keychain entry must survive: this is the "no gateway
	// configured, not --offline" case, which is a plain resolution failure,
	// not the caller explicitly asking to skip the server-side revoke.
	if _, err := keychain.ReadToken(email); err != nil {
		t.Errorf("expected the keychain entry for %s to survive a resolution failure, got: %v", email, err)
	}
}

func TestTokensRevoke_OfflineFlag_SkipsServerRevokeButStillClearsLocalSession(t *testing.T) {
	email := withLoggedInSession(t)
	withOffline(t)

	cmd := TokensCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	stdout := captureStdout(t, func() {
		cmd.SetArgs([]string{"revoke"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("tokens revoke offline: %v", err)
		}
	})
	if !strings.Contains(stdout, "Offline") || !strings.Contains(stdout, email) {
		t.Errorf("expected offline-skip message mentioning %s, got: %q", email, stdout)
	}
	if _, err := keychain.ReadToken(email); err == nil {
		t.Errorf("expected the keychain entry for %s to be deleted", email)
	}
}

func TestTokensRevoke_FailsClosedWhenNoGatewayConfigured(t *testing.T) {
	email := withLoggedInSession(t)
	withNoGateway(t)

	cmd := TokensCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"revoke"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
	if _, err := keychain.ReadToken(email); err != nil {
		t.Errorf("expected the keychain entry for %s to survive a resolution failure, got: %v", email, err)
	}
}

// ---- offline (local-only) verbs must be unaffected ----

func TestSkillInstall_LocalPath_StillSucceedsWhenNoGateway(t *testing.T) {
	withNoGateway(t)

	src := t.TempDir()
	srcDir := filepath.Join(src, "my-skill")
	writeSkillFiles(t, srcDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: my-skill\ndescription: a locally authored skill\n---\n\nbody\n"),
	})

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{srcDir, "--to", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected local install to succeed offline, got: %v\noutput:\n%s", err, out.String())
	}
}

func TestSkillInstall_LocalPath_StillSucceedsWithRootOfflineFlagSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	src := t.TempDir()
	srcDir := filepath.Join(src, "offline-flag-skill")
	writeSkillFiles(t, srcDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: offline-flag-skill\ndescription: local install must ignore --offline\n---\n\nbody\n"),
	})

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{srcDir, "--to", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected local install to ignore the root --offline flag, got: %v\noutput:\n%s", err, out.String())
	}
}

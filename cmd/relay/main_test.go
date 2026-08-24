package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/cli"
)

func TestOfflineFlagPresent(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"absent", []string{"clockify", "list_time_entries"}, false},
		{"bare flag", []string{"--offline", "clockify", "list_time_entries"}, true},
		{"equals true", []string{"--offline=true", "clockify"}, true},
		{"equals false", []string{"--offline=false", "clockify"}, false},
		{"equals zero", []string{"--offline=0", "clockify"}, false},
		{"mixed with other flags", []string{"clockify", "--arg", "x=1", "--offline"}, true},
		{"empty args", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := offlineFlagPresent(c.args); got != c.want {
				t.Errorf("offlineFlagPresent(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestRewriteOfflineUnknownCommandErr_RewritesWhenOfflineAndUnknownCommand(t *testing.T) {
	err := errors.New(`unknown command "clockify" for "relay"`)
	got := rewriteOfflineUnknownCommandErr(err, []string{"--offline", "clockify", "list_time_entries"})
	if got.Error() != cli.OfflineGuidance() {
		t.Fatalf("got %q, want the offline guidance message", got.Error())
	}
}

func TestRewriteOfflineUnknownCommandErr_LeavesNonUnknownCommandErrorsUntouched(t *testing.T) {
	err := errors.New("login failed: some other problem")
	got := rewriteOfflineUnknownCommandErr(err, []string{"--offline"})
	if got != err {
		t.Fatalf("got %v, want the original error untouched", got)
	}
}

func TestRewriteOfflineUnknownCommandErr_LeavesUnknownCommandUntouchedWithoutOfflineFlag(t *testing.T) {
	err := errors.New(`unknown command "clockify" for "relay"`)
	got := rewriteOfflineUnknownCommandErr(err, []string{"clockify", "list_time_entries"})
	if got != err {
		t.Fatalf("got %v, want the original error untouched (no --offline present)", got)
	}
}

func TestRewriteOfflineUnknownCommandErr_NilErrorPassesThrough(t *testing.T) {
	if got := rewriteOfflineUnknownCommandErr(nil, []string{"--offline"}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// TestShouldBuildDynamicCommands exercises the pure decision function
// PersistentPreRunE delegates to — the direct regression test for "--offline
// must skip dynamic command building" (Fix 1).
func TestShouldBuildDynamicCommands(t *testing.T) {
	cases := []struct {
		name    string
		cmdName string
		topName string
		offline bool
		want    bool
	}{
		{"dynamic command online", "clockify", "clockify", false, true},
		{"dynamic command offline", "clockify", "clockify", true, false},
		{"dynamic tool sub-command offline", "list_time_entries", "clockify", true, false},
		{"static top-level command never builds regardless of offline", "login", "login", false, false},
		{"static nested command (tokens list) never builds", "list", "tokens", false, false},
		{"static nested command (tokens list) offline too", "list", "tokens", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldBuildDynamicCommands(c.cmdName, c.topName, c.offline); got != c.want {
				t.Errorf("shouldBuildDynamicCommands(%q, %q, %v) = %v, want %v", c.cmdName, c.topName, c.offline, got, c.want)
			}
		})
	}
}

// TestRootCmd_Offline_SkipsDynamicRegistration_NoNetworkCall is a
// main-level smoke test: executing the full root command tree with
// --offline against an unregistered dynamic command name must never dial
// the gateway. GATEWAY_URL is set to an address nothing listens on
// (127.0.0.1:1 is refused instantly rather than hanging) so that IF
// BuildServiceCommands were mistakenly invoked, the resulting network
// error would surface loudly in the command's output instead of this test
// silently passing for the wrong reason.
func TestRootCmd_Offline_SkipsDynamicRegistration_NoNetworkCall(t *testing.T) {
	t.Setenv("GATEWAY_URL", "http://127.0.0.1:1")
	t.Setenv("HOME", t.TempDir())
	cli.SetOffline(false) // reset package-level state between tests
	t.Cleanup(func() { cli.SetOffline(false) })

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--offline", "clockify", "list_time_entries"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected an error for an unregistered dynamic command, got none. output:\n%s", out.String())
	}
	rewritten := rewriteOfflineUnknownCommandErr(err, []string{"--offline", "clockify", "list_time_entries"}).Error()
	if rewritten != cli.OfflineGuidance() {
		t.Errorf("error = %q, want the offline guidance message", rewritten)
	}
	if strings.Contains(err.Error()+out.String(), "connection refused") || strings.Contains(err.Error()+out.String(), "dial tcp") {
		t.Fatalf("expected zero network calls under --offline, but got a dial error: err=%v output=%s", err, out.String())
	}
}

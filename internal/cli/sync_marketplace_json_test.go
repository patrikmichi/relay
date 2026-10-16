package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runMarketplaceSync(t *testing.T, statusOverrides map[string]int, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	srv := buildMockServer(t, nil, statusOverrides)
	defer srv.Close()
	cmd := syncCmdWithDoer(&testDoer{srv: srv})
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(append([]string{"--marketplace", "--dir", t.TempDir(), "--to", "claude", "--kind", "rules", "--json", "--yes"}, args...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

func TestSyncMarketplaceJSONIsOneDocument(t *testing.T) {
	syncHome(t)
	stdout, stderr, err := runMarketplaceSync(t, nil)
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, stderr)
	}
	var doc struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Agents) == 0 {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if !strings.Contains(stderr, "Synced marketplace") {
		t.Errorf("the marketplace summary should go to stderr with --json:\n%s", stderr)
	}
}

func TestSyncMarketplaceFailureLeavesAgentConfigsUntouched(t *testing.T) {
	home := syncHome(t)
	_, _, err := runMarketplaceSync(t, map[string]int{"/api/marketplace/manifest": http.StatusInternalServerError})
	if err == nil || !strings.Contains(err.Error(), "no agent configuration was changed") {
		t.Fatalf("err = %v, want a failure that says the agent configs were not changed", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".claude", "CLAUDE.md")); statErr == nil {
		t.Error("agent rules were written although the marketplace pull failed")
	}
}

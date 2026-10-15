package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// TestWritePluginBundle_UnownedFileNeverDeleted proves the other half of
// the stale-file contract: a file the user added by hand next to a plugin's owned
// files — never recorded in any prior sync's owned-file set — survives an
// update even though it isn't present in the new bundle either. Only
// files this mechanism previously wrote are removal candidates.
func TestWritePluginBundle_UnownedFileNeverDeleted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	localDir := t.TempDir()
	pluginDir := filepath.Join(localDir, "plugins", "demo")

	if err := writePluginBundle(localDir, "demo", pluginDir, []byte(`{"files":{"keep.md":"one"}}`)); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	userFile := filepath.Join(pluginDir, "my-notes.txt")
	if err := os.WriteFile(userFile, []byte("hand-written notes"), 0o644); err != nil {
		t.Fatalf("seed user file: %v", err)
	}

	if err := writePluginBundle(localDir, "demo", pluginDir, []byte(`{"files":{"keep.md":"two"}}`)); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := os.ReadFile(userFile)
	if err != nil {
		t.Fatalf("user's manually added file should survive an update it was never part of: %v", err)
	}
	if string(got) != "hand-written notes" {
		t.Fatalf("user file content = %q, want unchanged", got)
	}
}

// TestWritePluginBundle_InterruptedApplyIsRecoverableAndRetryable forces
// every file write in a bundle to fail (a read-only plugin directory —
// deterministic regardless of the new-bundle map's iteration order,
// unlike targeting one specific file) and verifies: nothing is left
// half-written, the owned-file record is not advanced to the failed
// bundle's file set, and a retry once the fault clears completes cleanly.
func TestWritePluginBundle_InterruptedApplyIsRecoverableAndRetryable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	localDir := t.TempDir()
	pluginDir := filepath.Join(localDir, "plugins", "demo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(pluginDir, 0o555); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(pluginDir, 0o755) })

	err := writePluginBundle(localDir, "demo", pluginDir, []byte(`{"files":{"a.md":"one","b.md":"two"}}`))
	if err == nil {
		t.Fatal("writePluginBundle should fail against a read-only plugin directory")
	}

	if _, statErr := os.Stat(filepath.Join(pluginDir, "a.md")); !os.IsNotExist(statErr) {
		t.Fatalf("a.md should not exist after a failed write, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(pluginDir, "b.md")); !os.IsNotExist(statErr) {
		t.Fatalf("b.md should not exist after a failed write, stat err = %v", statErr)
	}

	owned, err := loadOwnedFiles(localDir, "demo")
	if err != nil {
		t.Fatalf("loadOwnedFiles: %v", err)
	}
	if len(owned) != 0 {
		t.Fatalf("owned-file record should not advance on a failed write, got %v", owned)
	}

	journals, err := txn.List()
	if err != nil {
		t.Fatalf("txn.List: %v", err)
	}
	if len(journals) != 1 || journals[0].State != txn.StateRestored {
		t.Fatalf("expected exactly one Restored journal, got %#v", journals)
	}

	if err := os.Chmod(pluginDir, 0o755); err != nil {
		t.Fatalf("chmod writable: %v", err)
	}
	if err := writePluginBundle(localDir, "demo", pluginDir, []byte(`{"files":{"a.md":"one","b.md":"two"}}`)); err != nil {
		t.Fatalf("retry should succeed once the fault clears: %v", err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if _, statErr := os.Stat(filepath.Join(pluginDir, name)); statErr != nil {
			t.Fatalf("%s should exist after the successful retry: %v", name, statErr)
		}
	}
}

// TestRunSync_FailureBeforeManifestWritePreservesStalePluginDir proves
// stale-plugin-directory revocation only runs AFTER marketplace.json is
// confirmed written: injecting a failure at the managed-settings fetch —
// the step immediately before both the marketplace.json write and
// revocation — must leave a stale plugin directory in place. Revoking
// first (the prior ordering) would let this exact failure point delete the
// directory while marketplace.json (still the old copy, or never written
// on a first sync) kept referencing it — a dangling reference. Revoking
// last means the only leftover in this failure direction is a harmless
// orphaned directory that nothing references.
func TestRunSync_FailureBeforeManifestWritePreservesStalePluginDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	srv := buildMockServer(t, nil, map[string]int{
		"/api/marketplace/managed-settings": http.StatusInternalServerError,
	})
	defer srv.Close()

	staleDir := filepath.Join(dir, "plugins", "old-revoked-plugin")
	if err := os.MkdirAll(staleDir, 0o700); err != nil {
		t.Fatalf("create stale dir: %v", err)
	}

	if err := runSync(&testDoer{srv: srv}, dir, false); err == nil {
		t.Fatal("sync should fail on the managed-settings 500")
	}

	if _, err := os.Stat(staleDir); err != nil {
		t.Fatalf("stale plugin dir should not be revoked before marketplace.json is written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marketplace.json")); !os.IsNotExist(err) {
		t.Fatalf("marketplace.json should not exist after a failed sync, stat err = %v", err)
	}
}

// TestRunSync_FailedPluginFetchPreservesLastCompleteMarketplaceState runs
// a full successful sync, then a second sync whose manifest changed but
// whose second plugin fetch fails — the on-disk marketplace.json (what
// Claude Code actually reads to know what's registered) must still
// reflect the FIRST, fully-complete sync, not a mix of the old and new
// manifests.
func TestRunSync_FailedPluginFetchPreservesLastCompleteMarketplaceState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	srv1 := buildMockServer(t, nil, nil)
	defer srv1.Close()
	if err := runSync(&testDoer{srv: srv1}, dir, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	firstManifest, err := os.ReadFile(filepath.Join(dir, "marketplace.json"))
	if err != nil {
		t.Fatalf("read marketplace.json after first sync: %v", err)
	}

	updatedManifest := marketplaceManifest{
		Name: testManifest.Name,
		Plugins: []manifestPlugin{
			{Name: "searcher", Source: "./plugins/searcher-001", Version: "9.9.9"},
			{Name: "formatter", Source: "./plugins/fmt-002", Version: "9.9.9"},
		},
	}
	srv2 := buildMockServer(t, updatedManifest, map[string]int{
		"/api/marketplace/plugin/fmt-002/9.9.9": http.StatusUnauthorized,
	})
	defer srv2.Close()

	if err := runSync(&testDoer{srv: srv2}, dir, false); err == nil {
		t.Fatal("second sync should fail on the fmt-002 401")
	}

	secondManifest, err := os.ReadFile(filepath.Join(dir, "marketplace.json"))
	if err != nil {
		t.Fatalf("read marketplace.json after failed second sync: %v", err)
	}
	if string(secondManifest) != string(firstManifest) {
		t.Fatalf("marketplace.json changed despite an incomplete sync:\nbefore: %s\nafter:  %s", firstManifest, secondManifest)
	}
}

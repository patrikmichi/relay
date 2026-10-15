package agentport

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func manifestFile(t *testing.T) string {
	t.Helper()
	p, err := manifestPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManifest_HomeUnusable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if _, err := LoadManifest(); err == nil {
		t.Error("LoadManifest succeeded without a config dir")
	}
	if err := SaveManifest(Manifest{}); err == nil {
		t.Error("SaveManifest succeeded without a config dir")
	}
	if err := RecordEntry(ManifestEntry{Name: "x"}); err == nil {
		t.Error("RecordEntry succeeded without a config dir")
	}
}

func TestLoadManifest_CorruptAndUnreadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := manifestFile(t)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(); err == nil || !strings.Contains(err.Error(), "parse manifest") {
		t.Fatalf("corrupt manifest = %v, want parse error", err)
	}
	if err := RecordEntry(ManifestEntry{Name: "x"}); err == nil {
		t.Fatal("RecordEntry must not overwrite a manifest it cannot parse")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{" {
		t.Fatalf("corrupt manifest was rewritten to %q", raw)
	}
	if _, err := RemoveEntriesFor("x", ProviderClaude, ScopeUser, KindSkill); err == nil {
		t.Fatal("RemoveEntriesFor must surface the parse error")
	}
	if err := RemoveEntry("x"); err == nil {
		t.Fatal("RemoveEntry must surface the parse error")
	}

	skipWithoutPermissionChecks(t)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(); err == nil || !strings.Contains(err.Error(), "read manifest") {
		t.Fatalf("unreadable manifest = %v, want read error", err)
	}
}

func TestLoadManifest_FutureSchemaRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(manifestFile(t), []byte(`{"schemaVersion": 99, "entries": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(); !errors.Is(err, ErrFutureManifestSchema) {
		t.Fatalf("LoadManifest = %v, want ErrFutureManifestSchema", err)
	}
}

func TestSaveManifest_RenameFailureLeavesNoTemp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := manifestFile(t)
	if err := os.MkdirAll(filepath.Join(path, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveManifest(Manifest{}); err == nil || !strings.Contains(err.Error(), "rename manifest") {
		t.Fatalf("SaveManifest = %v, want rename error", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp manifest %s left behind", e.Name())
		}
	}
}

func TestSaveManifest_ReadOnlyConfigDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Dir(manifestFile(t))
	skipWithoutPermissionChecks(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := SaveManifest(Manifest{}); err == nil {
		t.Fatal("SaveManifest succeeded in a read-only directory")
	}
	if err := RecordEntry(ManifestEntry{Name: "x"}); err == nil || !strings.Contains(err.Error(), "create manifest lock") {
		t.Fatalf("RecordEntry = %v, want lock creation error", err)
	}
}

func TestWithManifestLock_StealsStaleLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lock, err := manifestLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStaleTimeout)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := RecordEntry(ManifestEntry{Name: "x"}); err != nil {
		t.Fatalf("RecordEntry with a stale lock: %v", err)
	}
	if time.Since(start) >= lockAcquireTimeout {
		t.Fatal("stale lock was waited on instead of stolen")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock file not released: %v", err)
	}
}

func TestWithManifestLock_WaitsForLiveHolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lock, err := manifestLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(4 * lockRetryInterval)
		_ = os.Remove(lock)
	}()
	if err := RecordEntry(ManifestEntry{Name: "x"}); err != nil {
		t.Fatalf("RecordEntry after holder released: %v", err)
	}
	m, _ := LoadManifest()
	if len(m.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.Entries))
	}
}

func TestRemoveEntry_UnknownIDAndRemoveEntriesForNoMatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := RecordEntry(ManifestEntry{ID: "keep", Name: "x", Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveEntry("missing"); err == nil {
		t.Fatal("RemoveEntry of an unknown id succeeded")
	}
	n, err := RemoveEntriesFor("x", ProviderClaude, ScopeUser, KindAgent)
	if err != nil || n != 0 {
		t.Fatalf("RemoveEntriesFor other kind = %d, %v; want 0, nil", n, err)
	}
	m, _ := LoadManifest()
	if len(m.Entries) != 1 {
		t.Fatalf("no-op removals changed the ledger: %+v", m.Entries)
	}
}

func TestLastEntryOfKind(t *testing.T) {
	m := Manifest{Entries: []ManifestEntry{
		{ID: "1", Kind: KindAgent},
		{ID: "2", Kind: KindSkill},
		{ID: "3", Kind: KindAgent},
		{ID: "4", Kind: KindSkill},
	}}
	if e, ok := LastEntryOfKind(m, KindAgent); !ok || e.ID != "3" {
		t.Fatalf("LastEntryOfKind(agent) = %+v, %v; want entry 3", e, ok)
	}
	if _, ok := LastEntryOfKind(Manifest{Entries: m.Entries[1:2]}, KindAgent); ok {
		t.Fatal("LastEntryOfKind found an agent in a skill-only ledger")
	}
}

// skipWithoutPermissionChecks skips when file modes are not enforced: root
// bypasses them and Windows ignores Unix permission bits.
func skipWithoutPermissionChecks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for this user or OS")
	}
}

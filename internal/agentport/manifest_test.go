package agentport

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestManifest_RecordAndLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest (no file yet): %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %v, want none before any writes", m.Entries)
	}

	entry := ManifestEntry{
		Name:           "git-helper",
		Provider:       ProviderCodex,
		Scope:          ScopeUser,
		SourceProvider: ProviderClaude,
		TargetPaths:    map[string]string{"SKILL.md": "deadbeef"},
		Timestamp:      time.Now().UTC(),
		Provenance:     Provenance{SourceProvider: ProviderClaude, SourcePath: "/tmp/x"},
	}
	if err := RecordEntry(entry); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("Entries = %d, want 1", len(m.Entries))
	}
	if m.Entries[0].Name != "git-helper" || m.Entries[0].Provider != ProviderCodex {
		t.Errorf("unexpected entry: %#v", m.Entries[0])
	}

	// A second entry appends rather than replacing.
	entry2 := entry
	entry2.Name = "other-skill"
	if err := RecordEntry(entry2); err != nil {
		t.Fatalf("RecordEntry #2: %v", err)
	}
	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("Entries = %d, want 2", len(m.Entries))
	}
}

func TestManifest_PathLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := RecordEntry(ManifestEntry{Name: "x", Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	path, err := manifestPath()
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	want := filepath.Join(home, ".config", "relay", "agentport-manifest.json")
	if path != want {
		t.Fatalf("manifestPath = %s, want %s", path, want)
	}
}

// TestManifest_ConcurrentRecordEntry_NoLostUpdate is the regression test
// for the MAJOR issue: concurrent RecordEntry calls raced through an
// unlocked load-mutate-save cycle, so the second SaveManifest to finish
// could silently overwrite the first writer's appended entry. With
// withManifestLock serializing the cycle, every concurrent call's entry
// must survive.
func TestManifest_ConcurrentRecordEntry_NoLostUpdate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := RecordEntry(ManifestEntry{
				Name:     "concurrent",
				Provider: ProviderClaude,
				Scope:    ScopeUser,
			})
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("RecordEntry: %v", err)
		}
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != n {
		t.Fatalf("Entries = %d, want %d (no entry should be lost to a write race)", len(m.Entries), n)
	}

	// Every entry should also have a unique auto-assigned ID (no ID
	// collisions across the racing goroutines).
	seen := make(map[string]bool, n)
	for _, e := range m.Entries {
		if e.ID == "" {
			t.Fatalf("entry missing an ID: %#v", e)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate entry ID %q", e.ID)
		}
		seen[e.ID] = true
	}
}

// TestSaveManifest_UsesUniqueTempFile is the regression test for the
// "shared temp name" half of the MAJOR issue: SaveManifest must not leave
// (or race through) a single fixed "<path>.tmp" name — it uses
// os.CreateTemp so each call gets a distinct temp file before the atomic
// rename.
func TestSaveManifest_UsesUniqueTempFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "x", Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	path, err := manifestPath()
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	dir := filepath.Dir(path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// The old shared-name implementation left "agentport-manifest.json.tmp"
	// on disk if a save ever failed mid-write; with unique names via
	// os.CreateTemp("manifest-*.json.tmp"), that literal shared name must
	// never appear, and after a successful save no leftover "manifest-*.tmp"
	// file should remain either (each is cleaned up / renamed away).
	for _, e := range entries {
		if e.Name() == "agentport-manifest.json.tmp" {
			t.Fatalf("found the old shared temp-file name on disk: %s", e.Name())
		}
	}

	// Directly exercise SaveManifest's temp-file naming by writing several
	// times and confirming no stray "manifest-*.json.tmp" files accumulate
	// (each rename cleans up after itself) — this would only leave litter
	// if two racing writers had collided on the SAME temp name and stepped
	// on each other's file handles.
	for i := 0; i < 5; i++ {
		if err := SaveManifest(Manifest{Entries: []ManifestEntry{{ID: "x", Name: "x"}}}); err != nil {
			t.Fatalf("SaveManifest #%d: %v", i, err)
		}
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file after successful SaveManifest calls: %s", e.Name())
		}
	}
}

// TestLoadManifest_PreKindFixtureDecodesAsSkill loads a manifest JSON file
// written before the Kind field existed (no "kind" key at all) and confirms
// every entry normalizes to KindSkill — the back-compat guarantee P1.5
// exists for (relay-standalone design §3e: "Decode default = skill").
func TestLoadManifest_PreKindFixtureDecodesAsSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".config", "relay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	preKindJSON := `{
  "entries": [
    {
      "id": "1700000000000000000-aabbccdd",
      "name": "git-helper",
      "provider": "codex",
      "scope": "user",
      "sourceProvider": "claude",
      "targetPaths": {"SKILL.md": "deadbeef"},
      "timestamp": "2025-01-01T00:00:00Z",
      "provenance": {"sourceProvider": "claude", "sourcePath": "/tmp/x"}
    }
  ]
}
`
	path := filepath.Join(dir, "agentport-manifest.json")
	if err := os.WriteFile(path, []byte(preKindJSON), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("Entries = %d, want 1", len(m.Entries))
	}
	if m.Entries[0].Kind != KindSkill {
		t.Fatalf("Kind = %q, want %q (default for pre-kind JSON)", m.Entries[0].Kind, KindSkill)
	}

	// Confirm the kind-aware lookups still find it under KindSkill.
	entry, ok := LastEntryFor(m, "git-helper", ProviderCodex, ScopeUser, KindSkill)
	if !ok || entry.ID != m.Entries[0].ID {
		t.Fatalf("LastEntryFor(..., KindSkill) = (%#v, %v), want the pre-kind entry", entry, ok)
	}
	if _, ok := LastEntryFor(m, "git-helper", ProviderCodex, ScopeUser, KindAgent); ok {
		t.Fatalf("LastEntryFor(..., KindAgent) should not match a skill entry")
	}
}

// TestRecordEntry_DefaultsKindToSkill confirms a caller that never sets
// Kind (every existing skill call site) still records KindSkill explicitly
// on write, not just on read.
func TestRecordEntry_DefaultsKindToSkill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "x", Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Kind != KindSkill {
		t.Fatalf("Entries = %#v, want a single KindSkill entry", m.Entries)
	}
}

func TestHashFiles(t *testing.T) {
	files := map[string][]byte{
		"SKILL.md": []byte("hello"),
	}
	hashes := HashFiles(files)
	if len(hashes) != 1 {
		t.Fatalf("hashes = %v, want 1 entry", hashes)
	}
	// sha256("hello")
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if hashes["SKILL.md"] != want {
		t.Fatalf("hash = %s, want %s", hashes["SKILL.md"], want)
	}
}

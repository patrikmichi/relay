package agentport

import "testing"

func TestManifest_RecordEntryAssignsID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "x", Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].ID == "" {
		t.Fatalf("expected an auto-assigned ID, got %#v", m.Entries)
	}
}

func TestManifest_LastEntryAndFindEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, ok := LastEntry(Manifest{}); ok {
		t.Fatalf("LastEntry on empty manifest should report ok=false")
	}

	if err := RecordEntry(ManifestEntry{Name: "first", Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	if err := RecordEntry(ManifestEntry{Name: "second", Provider: ProviderCodex, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	last, ok := LastEntry(m)
	if !ok || last.Name != "second" {
		t.Fatalf("LastEntry = %#v, ok=%v, want Name=second", last, ok)
	}

	got, ok := FindEntry(m, last.ID)
	if !ok || got.Name != "second" {
		t.Fatalf("FindEntry(%q) = %#v, ok=%v", last.ID, got, ok)
	}

	if _, ok := FindEntry(m, "does-not-exist"); ok {
		t.Fatalf("FindEntry(unknown id): ok = true, want false")
	}
}

func TestManifest_LastEntryFor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "git-helper", Provider: ProviderCodex, Scope: ScopeUser, SourceProvider: ProviderClaude}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	// A later entry for the SAME name+provider+scope should be the one
	// LastEntryFor returns (most recent wins).
	if err := RecordEntry(ManifestEntry{Name: "git-helper", Provider: ProviderCodex, Scope: ScopeUser, SourceProvider: ProviderOpencode}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	entry, ok := LastEntryFor(m, "git-helper", ProviderCodex, ScopeUser, KindSkill)
	if !ok {
		t.Fatalf("LastEntryFor: ok = false, want true")
	}
	if entry.SourceProvider != ProviderOpencode {
		t.Fatalf("SourceProvider = %s, want %s (the more recent entry)", entry.SourceProvider, ProviderOpencode)
	}

	if _, ok := LastEntryFor(m, "does-not-exist", ProviderCodex, ScopeUser, KindSkill); ok {
		t.Fatalf("LastEntryFor(unknown name): ok = true, want false")
	}
}

// TestManifest_LastEntryFor_KindDistinguishesEntries confirms a skill entry
// and an agent entry sharing the same name+provider+scope never mask each
// other — LastEntryFor(..., KindAgent) must not return the skill entry, and
// vice versa (relay-standalone design §3e).
func TestManifest_LastEntryFor_KindDistinguishesEntries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "reviewer", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeUser, SourceProvider: ProviderClaude}); err != nil {
		t.Fatalf("RecordEntry (skill): %v", err)
	}
	if err := RecordEntry(ManifestEntry{Name: "reviewer", Kind: KindAgent, Provider: ProviderClaude, Scope: ScopeUser, SourceProvider: ProviderOpencode}); err != nil {
		t.Fatalf("RecordEntry (agent): %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	skillEntry, ok := LastEntryFor(m, "reviewer", ProviderClaude, ScopeUser, KindSkill)
	if !ok || skillEntry.SourceProvider != ProviderClaude {
		t.Fatalf("LastEntryFor(..., KindSkill) = (%#v, %v), want the skill entry", skillEntry, ok)
	}

	agentEntry, ok := LastEntryFor(m, "reviewer", ProviderClaude, ScopeUser, KindAgent)
	if !ok || agentEntry.SourceProvider != ProviderOpencode {
		t.Fatalf("LastEntryFor(..., KindAgent) = (%#v, %v), want the agent entry", agentEntry, ok)
	}
}

// TestManifest_RemoveEntriesFor_KindScoped confirms RemoveEntriesFor only
// removes the matching kind, leaving a same-name/provider/scope entry of
// the other kind untouched.
func TestManifest_RemoveEntriesFor_KindScoped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "reviewer", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry (skill): %v", err)
	}
	if err := RecordEntry(ManifestEntry{Name: "reviewer", Kind: KindAgent, Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry (agent): %v", err)
	}

	removed, err := RemoveEntriesFor("reviewer", ProviderClaude, ScopeUser, KindAgent)
	if err != nil {
		t.Fatalf("RemoveEntriesFor: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the agent entry)", removed)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Kind != KindSkill {
		t.Fatalf("Entries = %#v, want only the surviving skill entry", m.Entries)
	}
}

func TestManifest_RemoveEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "keep", Provider: ProviderClaude, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	if err := RecordEntry(ManifestEntry{Name: "drop", Provider: ProviderCodex, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	dropEntry, ok := LastEntryFor(m, "drop", ProviderCodex, ScopeUser, KindSkill)
	if !ok {
		t.Fatalf("expected to find the 'drop' entry")
	}

	if err := RemoveEntry(dropEntry.ID); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}

	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Name != "keep" {
		t.Fatalf("Entries = %#v, want only 'keep'", m.Entries)
	}

	if err := RemoveEntry("does-not-exist"); err == nil {
		t.Fatalf("RemoveEntry(unknown id): expected an error")
	}
}

func TestManifest_RemoveEntriesFor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := RecordEntry(ManifestEntry{Name: "git-helper", Provider: ProviderCodex, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	if err := RecordEntry(ManifestEntry{Name: "git-helper", Provider: ProviderCodex, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry #2: %v", err)
	}
	if err := RecordEntry(ManifestEntry{Name: "other", Provider: ProviderCodex, Scope: ScopeUser}); err != nil {
		t.Fatalf("RecordEntry (unrelated): %v", err)
	}

	removed, err := RemoveEntriesFor("git-helper", ProviderCodex, ScopeUser, KindSkill)
	if err != nil {
		t.Fatalf("RemoveEntriesFor: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Name != "other" {
		t.Fatalf("Entries = %#v, want only 'other'", m.Entries)
	}

	// A no-match call is a harmless no-op, not an error.
	removed, err = RemoveEntriesFor("nope", ProviderCodex, ScopeUser, KindSkill)
	if err != nil || removed != 0 {
		t.Fatalf("RemoveEntriesFor(no match) = (%d, %v), want (0, nil)", removed, err)
	}
}

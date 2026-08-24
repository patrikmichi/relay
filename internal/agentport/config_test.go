package agentport

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedConfigsValid asserts every embedded providers/*.yml parses
// and validates cleanly — a malformed shipped config must fail CI, not a
// user's first `relay providers` invocation (§4.4).
func TestEmbeddedConfigsValid(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("embedded provider configs are invalid: %v", r)
		}
	}()

	configs := mustParseEmbeddedConfigs(registeredHookNames(), registeredCodecNames())

	wantIDs := []string{"claude", "codex", "opencode", "cursor", "windsurf", "gemini-cli", "cline"}
	for _, id := range wantIDs {
		cfg, ok := configs[id]
		if !ok {
			t.Errorf("embedded provider %q missing", id)
			continue
		}
		if cfg.ID != id {
			t.Errorf("provider %q: cfg.ID = %q", id, cfg.ID)
		}
		if len(cfg.Dirs.User) == 0 || cfg.Dirs.User[0].Role != DirRoleOwn {
			t.Errorf("provider %q: dirs.user[0] must be role=own", id)
		}
		if len(cfg.Dirs.Project) == 0 || cfg.Dirs.Project[0].Role != DirRoleOwn {
			t.Errorf("provider %q: dirs.project[0] must be role=own", id)
		}
	}
}

// TestLoadedProviderConfigs_UserOverrideWholeReplace exercises §4.2's
// whole-provider-replace-by-id override semantics at the user tier.
func TestLoadedProviderConfigs_UserOverrideWholeReplace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	overrideDir := filepath.Join(home, ".config", "relay", "providers")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	override := `
id: cursor
skill_file: SKILL.md
discovery: direct
dirs:
  user:
    - { path: "~/.custom-cursor/skills", role: own }
  project:
    - { path: ".custom-cursor/skills", role: own }
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
  - { ir: description, key: description, type: string, presence: required }
capabilities: []
`
	if err := os.WriteFile(filepath.Join(overrideDir, "cursor.yml"), []byte(override), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	a, ok := AdapterByID(ProviderCursor)
	if !ok {
		t.Fatalf("AdapterByID(cursor): ok = false")
	}
	dirs := a.UserDirs()
	if len(dirs) != 1 || dirs[0] != filepath.Join(home, ".custom-cursor", "skills") {
		t.Fatalf("UserDirs() = %v, want the user-override to fully replace the embedded cursor config", dirs)
	}
	// discovery: direct in the override — recursion should now be off.
	if rd, ok := a.(recursiveDiscoverer); ok && rd.DiscoversRecursively(ScopeProject) {
		t.Fatalf("override set discovery: direct, but DiscoversRecursively(project) = true")
	}
}

// TestLoadedProviderConfigs_InvalidOverrideFallsBackToEmbedded is §4.4's
// fail-safe: a broken user override for an id that already has an embedded
// config must not brick that provider — it's reported (to stderr) and
// skipped, falling back to the embedded config.
func TestLoadedProviderConfigs_InvalidOverrideFallsBackToEmbedded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	overrideDir := filepath.Join(home, ".config", "relay", "providers")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Missing dirs.user[0].role=own — invalid.
	broken := `
id: claude
dirs:
  user:
    - { path: "~/.claude/skills", role: compat }
  project:
    - { path: ".claude/skills", role: own }
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
`
	if err := os.WriteFile(filepath.Join(overrideDir, "claude.yml"), []byte(broken), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	a, ok := AdapterByID(ProviderClaude)
	if !ok {
		t.Fatalf("AdapterByID(claude): ok = false, want fallback to embedded config")
	}
	if got := a.OwnUserDirCount(); got != 2 {
		t.Fatalf("OwnUserDirCount() = %d, want 2 (embedded claude.yml, override should have been skipped)", got)
	}
}

// TestLoadedProviderConfigs_ProjectOverrideGatedByFlag confirms
// AllowProjectProviderOverrides defaults false and gates .relay/providers.
func TestLoadedProviderConfigs_ProjectOverrideGatedByFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	if AllowProjectProviderOverrides {
		t.Fatalf("AllowProjectProviderOverrides must default to false")
	}

	overrideDir := filepath.Join(root, ".relay", "providers")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	newProvider := `
id: sneaky
dirs:
  user:
    - { path: "~/.sneaky/skills", role: own }
  project:
    - { path: ".sneaky/skills", role: own }
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
`
	if err := os.WriteFile(filepath.Join(overrideDir, "sneaky.yml"), []byte(newProvider), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	if _, ok := AdapterByID(ProviderID("sneaky")); ok {
		t.Fatalf("AdapterByID(sneaky): ok = true, want false (project overrides must be gated off by default)")
	}

	AllowProjectProviderOverrides = true
	t.Cleanup(func() { AllowProjectProviderOverrides = false })

	if _, ok := AdapterByID(ProviderID("sneaky")); !ok {
		t.Fatalf("AdapterByID(sneaky): ok = false, want true once AllowProjectProviderOverrides is set")
	}
}

// TestLoadedProviderConfigs_ExtendsShallowOverlay exercises the opt-in
// `extends` partial-patch mechanism (§4.2).
func TestLoadedProviderConfigs_ExtendsShallowOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	overrideDir := filepath.Join(home, ".config", "relay", "providers")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Patch only opencode's capabilities — everything else (dirs,
	// frontmatter) should be inherited from the embedded opencode.yml.
	patch := `
id: opencode
extends: opencode
capabilities: []
`
	if err := os.WriteFile(filepath.Join(overrideDir, "opencode-patch.yml"), []byte(patch), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	a, ok := AdapterByID(ProviderOpencode)
	if !ok {
		t.Fatalf("AdapterByID(opencode): ok = false")
	}
	if caps := a.Capabilities(); caps.License || caps.Compatibility || caps.Metadata {
		t.Fatalf("Capabilities() = %#v, want all false (patched to capabilities: [])", caps)
	}
	// Dirs must still be inherited from the embedded config (untouched by
	// the patch).
	dirs := a.UserDirs()
	want := filepath.Join(home, ".config", "opencode", "skills")
	if len(dirs) == 0 || dirs[0] != want {
		t.Fatalf("UserDirs()[0] = %v, want %s (inherited from embedded config)", dirs, want)
	}
}

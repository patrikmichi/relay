package agentport

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestNewPlatforms_NoGoChange is the "prove add-a-platform-with-no-Go"
// acceptance test (design §5.3): Windsurf, Gemini CLI, and Cline exist ONLY
// as providers/{windsurf,gemini-cli,cline}.yml — no adapter_windsurf.go, no
// switch-statement addition anywhere in this package. AdapterByID/
// AllAdapters resolving them, and a normal Load/Project/Migrate round trip
// working, demonstrates a new platform is a data edit, not a recompile.
func TestNewPlatforms_NoGoChange(t *testing.T) {
	for _, id := range []ProviderID{"windsurf", "gemini-cli", "cline"} {
		t.Run(string(id), func(t *testing.T) {
			a, ok := AdapterByID(id)
			if !ok {
				t.Fatalf("AdapterByID(%s): ok = false", id)
			}
			if a.ID() != id {
				t.Fatalf("ID() = %s, want %s", a.ID(), id)
			}

			home := t.TempDir()
			t.Setenv("HOME", home)
			// Re-fetch after setting HOME so UserDirs() reflects it (dirs
			// are expanded at call time — see configAdapter.UserDirs).
			a, _ = AdapterByID(id)

			dirs := a.UserDirs()
			if len(dirs) == 0 {
				t.Fatalf("UserDirs() = %v, want at least one entry", dirs)
			}
			if a.OwnUserDirCount() != 1 || a.OwnProjectDirCount() != 1 {
				t.Fatalf("Own*DirCount() = (%d, %d), want (1, 1)", a.OwnUserDirCount(), a.OwnProjectDirCount())
			}
		})
	}

	// All 3 must appear in AllAdapters() too.
	found := map[ProviderID]bool{}
	for _, a := range AllAdapters() {
		found[a.ID()] = true
	}
	for _, id := range []ProviderID{"windsurf", "gemini-cli", "cline"} {
		if !found[id] {
			t.Errorf("AllAdapters() missing %s", id)
		}
	}
}

// TestNewPlatforms_RoundTrip is the Load -> Project -> re-Load round trip
// for Windsurf, mirroring the existing adapter_*_test.go pattern for the 4
// originally hard-coded providers.
func TestNewPlatforms_RoundTrip(t *testing.T) {
	a, ok := AdapterByID("windsurf")
	if !ok {
		t.Fatalf("AdapterByID(windsurf): ok = false")
	}

	skillDir := filepath.Join(t.TempDir(), "windsurf-skill")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: windsurf-skill\ndescription: a windsurf skill\nmetadata:\n  author: acme\n---\n\nbody\n"),
	})

	src, err := a.Load(skillDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src.Name != "windsurf-skill" || src.Description != "a windsurf skill" {
		t.Fatalf("Load() = %#v", src)
	}
	if !reflect.DeepEqual(src.Metadata, map[string]string{"author": "acme"}) {
		t.Fatalf("Metadata = %#v", src.Metadata)
	}

	files, loss, err := a.Project(src)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(loss) != 0 {
		t.Fatalf("Loss = %#v, want none (round trip within the same provider)", loss)
	}

	dest := filepath.Join(t.TempDir(), src.Name)
	writeFiles(t, dest, files)
	got, err := a.Load(dest)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	sameCommon(t, got, src)
	if !reflect.DeepEqual(got.Metadata, src.Metadata) {
		t.Errorf("Metadata = %#v, want %#v", got.Metadata, src.Metadata)
	}
}

// TestNewPlatforms_MigrateFromClaude is the acceptance test's `relay skill
// migrate x --from claude --to windsurf`-equivalent: a Claude skill
// migrated to the new, YAML-only Windsurf provider projects a valid
// SKILL.md with a correct lossiness report — Claude-only fields
// (allowed-tools, disable-model-invocation, license) have no Windsurf
// equivalent and must be reported dropped; name/description round-trip.
func TestNewPlatforms_MigrateFromClaude(t *testing.T) {
	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	windsurf, ok := AdapterByID("windsurf")
	if !ok {
		t.Fatalf("AdapterByID(windsurf): ok = false")
	}

	plan, err := Migrate(src, windsurf, ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertLossFields(t, plan.Loss, "AllowedTools", "DisableModelInvocation", "License")

	tmp := filepath.Join(t.TempDir(), src.Name)
	writeFiles(t, tmp, plan.Files)
	got, err := windsurf.Load(tmp)
	if err != nil {
		t.Fatalf("re-Load projected windsurf skill: %v", err)
	}
	if got.Name != src.Name || got.Description != src.Description || got.Body != src.Body {
		t.Errorf("projected skill mismatch: got %#v, want name/description/body of %#v", got, src)
	}
}

// TestNewPlatforms_GeminiCLIAndClineDetect confirms Detect()/dirs work for
// the other 2 new YAML-only providers too (not just Windsurf).
func TestNewPlatforms_GeminiCLIAndClineDetect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, tc := range []struct {
		id      ProviderID
		dirName string
	}{
		{"gemini-cli", ".gemini"},
		{"cline", ".cline"},
	} {
		a, ok := AdapterByID(tc.id)
		if !ok {
			t.Fatalf("AdapterByID(%s): ok = false", tc.id)
		}
		if a.Detect() {
			t.Errorf("%s: Detect() = true before its dir exists", tc.id)
		}
		if err := os.MkdirAll(filepath.Join(home, tc.dirName, "skills"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if !a.Detect() {
			t.Errorf("%s: Detect() = false after its dir exists", tc.id)
		}
	}
}

package agentport

import (
	"os"
	"path/filepath"
	"testing"
)

// This file is a Load -> Project byte-exact round-trip test for ALL 7
// shipped skill providers, using the git-helper fixtures under
// testdata/<provider>/ (the pre-existing 4 fixtures for
// claude/codex/cursor/opencode, plus the 3 fixtures for
// gemini-cli/cline/windsurf, each of which exercises every field its
// providers/<id>.yml declares in "frontmatter").
//
// "Byte-exact round trip" here means: Load(fixture) -> Project() ->
// re-Load the projected bytes (from a scratch dir, never the fixture
// itself) -> Project() again — the SECOND Project() call's SKILL.md bytes
// must be byte-identical to the FIRST's. This is a stronger, provider-
// agnostic guarantee than comparing against a hand-captured golden
// constant (config_adapter_parity_test.go's approach, which only applies
// to the 4 originally hard-coded providers with a pre-refactor
// implementation to capture a golden from): it fails the moment ANY
// provider's frontmatter field mapping changes without the fixture (or
// the mapping) being updated to match, since a same-provider round trip
// must also report zero fidelity loss.
func TestVerifiedFormats_ByteExactRoundTrip(t *testing.T) {
	for _, id := range []string{"claude", "codex", "cursor", "opencode", "gemini-cli", "cline", "windsurf"} {
		t.Run(id, func(t *testing.T) {
			a, ok := AdapterByID(ProviderID(id))
			if !ok {
				t.Fatalf("AdapterByID(%s): ok = false", id)
			}

			fixtureDir := filepath.Join("testdata", id, "git-helper")
			s1, err := a.Load(fixtureDir)
			if err != nil {
				t.Fatalf("Load(fixture): %v", err)
			}
			if s1.Name != "git-helper" {
				t.Fatalf("Name = %q, want git-helper", s1.Name)
			}
			if s1.Description == "" {
				t.Fatalf("Description is empty — fixture frontmatter not loaded")
			}

			files1, loss1, err := a.Project(s1)
			if err != nil {
				t.Fatalf("Project(1): %v", err)
			}
			if len(loss1) != 0 {
				t.Fatalf("Loss(1) = %#v, want none (same-provider round trip)", loss1)
			}
			skillMD1, ok := files1["SKILL.md"]
			if !ok {
				t.Fatalf("Project(1) files = %#v, missing SKILL.md", files1)
			}

			// Re-Load from a SCRATCH copy of the projected output (never
			// the fixture dir itself) and Project a second time.
			scratch := t.TempDir()
			skillDir := filepath.Join(scratch, "git-helper")
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			for rel, data := range files1 {
				dest := filepath.Join(skillDir, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
					t.Fatalf("mkdir for %s: %v", rel, err)
				}
				if err := os.WriteFile(dest, data, 0o644); err != nil {
					t.Fatalf("write %s: %v", rel, err)
				}
			}

			s2, err := a.Load(skillDir)
			if err != nil {
				t.Fatalf("Load(2, re-projected): %v", err)
			}
			files2, loss2, err := a.Project(s2)
			if err != nil {
				t.Fatalf("Project(2): %v", err)
			}
			if len(loss2) != 0 {
				t.Fatalf("Loss(2) = %#v, want none (same-provider round trip)", loss2)
			}
			skillMD2, ok := files2["SKILL.md"]
			if !ok {
				t.Fatalf("Project(2) files = %#v, missing SKILL.md", files2)
			}

			if string(skillMD1) != string(skillMD2) {
				t.Fatalf("Project() is not idempotent for %s:\n--- first ---\n%s\n--- second ---\n%s", id, skillMD1, skillMD2)
			}
		})
	}
}

// TestVerifiedFormats_NewFixturesExerciseEveryDeclaredField confirms the
// fixtures for gemini-cli/cline/windsurf populate every field their
// providers/<id>.yml declares in "frontmatter" (today: name, description,
// metadata for all three) and carry at least one resource file, so the
// round-trip test above is actually exercising the full declared schema,
// not just the two universally-required fields.
func TestVerifiedFormats_NewFixturesExerciseEveryDeclaredField(t *testing.T) {
	for _, id := range []string{"gemini-cli", "cline", "windsurf"} {
		t.Run(id, func(t *testing.T) {
			a, ok := AdapterByID(ProviderID(id))
			if !ok {
				t.Fatalf("AdapterByID(%s): ok = false", id)
			}
			s, err := a.Load(filepath.Join("testdata", id, "git-helper"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if s.Name == "" || s.Description == "" {
				t.Fatalf("name/description not loaded: %#v", s)
			}
			if len(s.Metadata) == 0 {
				t.Fatalf("metadata not loaded: %#v", s)
			}
			if len(s.Resources) == 0 {
				t.Fatalf("expected at least one resource file (scripts/run.sh) to be loaded")
			}
			if _, ok := s.Resources["scripts/run.sh"]; !ok {
				t.Fatalf("Resources = %#v, want a scripts/run.sh entry", s.Resources)
			}
		})
	}
}

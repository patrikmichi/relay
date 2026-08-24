package agentport

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// lossFields returns the sorted set of field names present in a loss
// report, for order-independent membership assertions.
func lossFields(loss []LossItem) []string {
	out := make([]string, 0, len(loss))
	for _, l := range loss {
		out = append(out, l.Field)
	}
	sort.Strings(out)
	return out
}

func assertLossFields(t *testing.T, loss []LossItem, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := lossFields(loss)
	if len(got) != len(want) {
		t.Fatalf("loss fields = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("loss fields = %v, want %v", got, want)
		}
	}
	for _, l := range loss {
		if l.Kind != LossDropped {
			t.Errorf("field %s: Kind = %s, want %s", l.Field, l.Kind, LossDropped)
		}
	}
}

func TestMigrate_ClaudeToCodex_Fidelity(t *testing.T) {
	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Claude-only fields (allowed-tools, disable-model-invocation, license)
	// have no Codex equivalent.
	assertLossFields(t, plan.Loss, "AllowedTools", "DisableModelInvocation", "License")

	// Common fields must still project through correctly.
	tmp := filepath.Join(t.TempDir(), src.Name)
	writeFiles(t, tmp, plan.Files)
	got, err := NewCodexAdapter().Load(tmp)
	if err != nil {
		t.Fatalf("re-Load projected codex skill: %v", err)
	}
	sameCommon(t, got, src)
}

func TestMigrate_CursorToClaude_Fidelity(t *testing.T) {
	src, err := NewCursorAdapter().Load(filepath.Join("testdata", "cursor", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	plan, err := Migrate(src, NewClaudeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Cursor-only fields (paths, metadata) have no Claude Code equivalent.
	// disable-model-invocation IS shared between Cursor and Claude, so it
	// must NOT show up as dropped.
	assertLossFields(t, plan.Loss, "Paths", "Metadata")

	tmp := filepath.Join(t.TempDir(), src.Name)
	writeFiles(t, tmp, plan.Files)
	got, err := NewClaudeAdapter().Load(tmp)
	if err != nil {
		t.Fatalf("re-Load projected claude skill: %v", err)
	}
	sameCommon(t, got, src)
	if got.DisableModelInvocation == nil || *got.DisableModelInvocation != *src.DisableModelInvocation {
		t.Errorf("DisableModelInvocation should be preserved across cursor->claude: got %v, want %v",
			got.DisableModelInvocation, src.DisableModelInvocation)
	}
}

func TestMigrate_CodexToOpencode_Fidelity(t *testing.T) {
	src, err := NewCodexAdapter().Load(filepath.Join("testdata", "codex", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	plan, err := Migrate(src, NewOpencodeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Codex-only fields (openai.yaml sidecar: interface, policy, tools)
	// have no opencode equivalent.
	assertLossFields(t, plan.Loss, "CodexInterface", "AllowImplicitInvocation", "CodexTools")

	tmp := filepath.Join(t.TempDir(), src.Name)
	writeFiles(t, tmp, plan.Files)
	got, err := NewOpencodeAdapter().Load(tmp)
	if err != nil {
		t.Fatalf("re-Load projected opencode skill: %v", err)
	}
	sameCommon(t, got, src)
}

func TestMigrate_NoLossWhenTargetSupportsEverything(t *testing.T) {
	// A minimal skill with no extension fields set should migrate anywhere
	// with zero loss.
	src := &Skill{Name: "plain", Description: "a plain skill", Body: "body\n"}

	for _, target := range AllAdapters() {
		plan, err := Migrate(src, target, ScopeUser)
		if err != nil {
			t.Fatalf("Migrate to %s: %v", target.ID(), err)
		}
		if len(plan.Loss) != 0 {
			t.Errorf("Migrate to %s: Loss = %#v, want none", target.ID(), plan.Loss)
		}
		if plan.HasDropped() {
			t.Errorf("Migrate to %s: HasDropped() = true, want false", target.ID())
		}
	}
}

func TestEngine_WriteRecordsManifestAndFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	wantDir := filepath.Join(tmpHome, ".agents", "skills", "git-helper")
	if plan.TargetPaths != wantDir {
		t.Fatalf("TargetPaths = %s, want %s", plan.TargetPaths, wantDir)
	}

	if err := Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(wantDir, "SKILL.md")); err != nil {
		t.Fatalf("expected SKILL.md written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "scripts", "run.sh")); err != nil {
		t.Fatalf("expected resource file written: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("Entries = %d, want 1", len(m.Entries))
	}
	entry := m.Entries[0]
	if entry.Name != "git-helper" || entry.Provider != ProviderCodex || entry.SourceProvider != ProviderClaude {
		t.Fatalf("unexpected manifest entry: %#v", entry)
	}
	if len(entry.TargetPaths) != len(plan.Files) {
		t.Fatalf("TargetPaths hash count = %d, want %d", len(entry.TargetPaths), len(plan.Files))
	}
}

func TestMigrate_ProjectScopeUsesCwdRelativeDir(t *testing.T) {
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	src := &Skill{Name: "plain", Description: "d", Body: "b\n"}
	plan, err := Migrate(src, NewClaudeAdapter(), ScopeProject)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Use the physical cwd (as filepath.Abs/os.Getwd would resolve it, not
	// the possibly-symlinked path t.TempDir() returned — e.g. /tmp is a
	// symlink to /private/tmp on macOS) for the comparison.
	physicalCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	wantDir := filepath.Join(physicalCwd, ".claude", "skills", "plain")
	if plan.TargetPaths != wantDir {
		t.Fatalf("TargetPaths = %s, want %s", plan.TargetPaths, wantDir)
	}
}

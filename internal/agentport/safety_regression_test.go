package agentport

// Regression tests for the write and rollback engines' safety guarantees:
// destination symlinks, cross-project rollback, overwriting existing
// content, partial multi-target writes, and installs over a corrupt ledger.
// Do not weaken these assertions to obtain a green baseline.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestWriteRejectsDestinationSymlink covers the skill write
// engine against a symlinked destination: the
// destination file is itself a symlink to a file outside the artifact
// directory. Safe behavior: Write must refuse rather than follow it.
func TestWriteRejectsDestinationSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("SAFE SENTINEL\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if err := os.MkdirAll(plan.TargetPaths, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(sentinel, filepath.Join(plan.TargetPaths, "SKILL.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := Write(plan); err == nil {
		t.Fatal("expected Write to refuse a symlinked destination file")
	} else if !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("err = %v, want ErrDestinationSymlink", err)
	}

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "SAFE SENTINEL\n" {
		t.Fatalf("write mutated the symlink target outside the artifact directory: %q", data)
	}
}

// TestWriteAgentRejectsDestinationSymlink is the agent write engine
// analogue of TestWriteRejectsDestinationSymlink.
func TestWriteAgentRejectsDestinationSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := &Agent{Name: "reviewer", Description: "d", Body: "b\n"}
	plan, err := MigrateAgent(src, NewClaudeAgentAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}

	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("SAFE SENTINEL\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if err := os.MkdirAll(plan.TargetPaths, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	var rel string
	for r := range plan.Files {
		rel = r
		break
	}
	if err := os.Symlink(sentinel, filepath.Join(plan.TargetPaths, rel)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := WriteAgent(plan); err == nil {
		t.Fatal("expected WriteAgent to refuse a symlinked destination file")
	} else if !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("err = %v, want ErrDestinationSymlink", err)
	}

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "SAFE SENTINEL\n" {
		t.Fatalf("write mutated the symlink target outside the artifact directory: %q", data)
	}
}

// TestRollbackRejectsSymlinkTarget covers the Rollback side of the symlink
// check: an owned file is replaced with a symlink after installation (an
// attacker, or a corrupted reinstall). Safe behavior: Rollback must refuse to
// delete/read through it rather than acting on whatever it points to.
func TestRollbackRejectsSymlinkTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry after Write")
	}

	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("SAFE SENTINEL\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	skillMdPath := filepath.Join(plan.TargetPaths, "SKILL.md")
	if err := os.Remove(skillMdPath); err != nil {
		t.Fatalf("remove owned file: %v", err)
	}
	if err := os.Symlink(sentinel, skillMdPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := Rollback(entry, false); err == nil {
		t.Fatal("expected Rollback to refuse deleting through a symlink")
	} else if !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("err = %v, want ErrDestinationSymlink", err)
	}

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "SAFE SENTINEL\n" {
		t.Fatalf("rollback followed the symlink and touched the outside sentinel: %q", data)
	}

	m2, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m2.Entries) != 1 {
		t.Fatalf("expected the manifest entry to remain after a refused rollback, got %#v", m2.Entries)
	}
}

// TestRollbackRejectsDifferentProject covers rolling back
// from the wrong project: install into project A, copy identical
// content into project B, then roll back A's entry from B. Safe behavior:
// Rollback must refuse rather than deleting B's file and dropping A's
// ledger entry.
func TestRollbackRejectsDifferentProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	projectA := t.TempDir()
	if err := os.Chdir(projectA); err != nil {
		t.Fatalf("Chdir A: %v", err)
	}

	src := &Skill{Name: "demo", Description: "d", Body: "b\n"}
	plan, err := Migrate(src, NewClaudeAdapter(), ScopeProject)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry after Write")
	}
	if entry.ProjectRoot == "" {
		t.Fatalf("expected a project-scope entry to record ProjectRoot")
	}

	aFile := filepath.Join(plan.TargetPaths, "SKILL.md")
	original, err := os.ReadFile(aFile)
	if err != nil {
		t.Fatalf("read A's file: %v", err)
	}

	projectB := t.TempDir()
	if err := os.Chdir(projectB); err != nil {
		t.Fatalf("Chdir B: %v", err)
	}
	bFile := filepath.Join(projectB, ".claude", "skills", "demo", "SKILL.md")
	writeFiles(t, filepath.Dir(bFile), map[string][]byte{"SKILL.md": original})

	if err := Rollback(entry, false); err == nil {
		t.Fatal("expected Rollback to refuse a rollback initiated from a different project")
	} else if !errors.Is(err, ErrAmbiguousProjectRollback) {
		t.Fatalf("err = %v, want ErrAmbiguousProjectRollback", err)
	}

	if _, err := os.Stat(bFile); err != nil {
		t.Fatalf("expected project B's file to remain untouched: %v", err)
	}
	if _, err := os.Stat(aFile); err != nil {
		t.Fatalf("expected project A's original installation to remain untouched: %v", err)
	}
	m2, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m2.Entries) != 1 {
		t.Fatalf("expected A's ledger entry to remain after a refused cross-project rollback, got %#v", m2.Entries)
	}
}

// TestRollbackRejectsLegacyProjectEntry: a project-scope manifest
// entry recorded before ProjectRoot existed decodes with it empty and must
// be rejected rather than trusted against whatever the current CWD is.
func TestRollbackRejectsLegacyProjectEntry(t *testing.T) {
	entry := ManifestEntry{ID: "legacy-1", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeProject, Name: "demo"}
	if err := Rollback(entry, false); err == nil {
		t.Fatal("expected Rollback to refuse a legacy project-scope entry with no recorded ProjectRoot")
	} else if !errors.Is(err, ErrAmbiguousProjectRollback) {
		t.Fatalf("err = %v, want ErrAmbiguousProjectRollback", err)
	}
}

// TestWriteRejectsConflictingExistingContent covers overwriting
// existing content: pre-existing content at the
// destination differs from what's about to be written. Safe behavior:
// Write must refuse rather than silently overwriting it (the prior
// behavior meant a subsequent rollback deleted the file entirely, with no
// way to recover the original bytes).
func TestWriteRejectsConflictingExistingContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	writeFiles(t, plan.TargetPaths, map[string][]byte{"SKILL.md": []byte("ORIGINAL USER CONTENT\n")})

	if err := Write(plan); err == nil {
		t.Fatal("expected Write to refuse to overwrite conflicting existing content")
	} else if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("err = %v, want ErrWriteConflict", err)
	}

	data, err := os.ReadFile(filepath.Join(plan.TargetPaths, "SKILL.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "ORIGINAL USER CONTENT\n" {
		t.Fatalf("original content was overwritten: %q", data)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("expected no manifest entry recorded for a refused write, got %#v", m.Entries)
	}
}

// TestWriteAgentRejectsConflictingExistingContent is the agent write
// engine analogue of TestWriteRejectsConflictingExistingContent.
func TestWriteAgentRejectsConflictingExistingContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := &Agent{Name: "reviewer", Description: "d", Body: "b\n"}
	plan, err := MigrateAgent(src, NewClaudeAgentAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}
	conflicting := make(map[string][]byte, len(plan.Files))
	for rel := range plan.Files {
		conflicting[rel] = []byte("ORIGINAL USER CONTENT\n")
	}
	writeFiles(t, plan.TargetPaths, conflicting)

	if err := WriteAgent(plan); err == nil {
		t.Fatal("expected WriteAgent to refuse to overwrite conflicting existing content")
	} else if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("err = %v, want ErrWriteConflict", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("expected no manifest entry recorded for a refused write, got %#v", m.Entries)
	}
}

// TestWriteFailsClosedWhenLedgerCorrupt covers an install over
// a corrupt ledger: the manifest ledger is corrupt
// before an install runs. Safe behavior: Write must check the ledger
// BEFORE touching the filesystem, so a corrupt ledger leaves no changed
// files behind (the prior behavior wrote the files first and only failed
// afterward on RecordEntry, leaving mutated files with no ledger record).
func TestWriteFailsClosedWhenLedgerCorrupt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	path, err := manifestPath()
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	if err := Write(plan); err == nil {
		t.Fatal("expected Write to fail closed when the ledger is corrupt")
	} else if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("err = %v, want ErrLedgerUnavailable", err)
	}

	if _, statErr := os.Stat(plan.TargetPaths); !os.IsNotExist(statErr) {
		t.Fatalf("files were written to disk before the ledger check failed (target %s), stat err = %v", plan.TargetPaths, statErr)
	}
}

// TestWriteAgentFailsClosedWhenLedgerCorrupt is the agent write engine
// analogue of TestWriteFailsClosedWhenLedgerCorrupt.
func TestWriteAgentFailsClosedWhenLedgerCorrupt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := &Agent{Name: "reviewer", Description: "d", Body: "b\n"}
	plan, err := MigrateAgent(src, NewClaudeAgentAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}

	path, err := manifestPath()
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	if err := WriteAgent(plan); err == nil {
		t.Fatal("expected WriteAgent to fail closed when the ledger is corrupt")
	} else if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("err = %v, want ErrLedgerUnavailable", err)
	}

	if _, statErr := os.Stat(filepath.Join(plan.TargetPaths, "reviewer.md")); !os.IsNotExist(statErr) {
		t.Fatalf("the agent file was written to disk before the ledger check failed, stat err = %v", statErr)
	}
}

// TestPreflightWriteValidatesAllTargetsBeforeAnyWrite covers partial writes at
// the engine level: a multi-
// target migration (e.g. `relay skill migrate --to codex --to opencode`)
// must not write an earlier target when a later target fails preflight.
// PreflightWrite is exported precisely so a multi-target caller can
// validate every plan first — see preflight.go.
func TestPreflightWriteValidatesAllTargetsBeforeAnyWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	firstTarget, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate (codex): %v", err)
	}
	secondTarget, err := Migrate(src, NewOpencodeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate (opencode): %v", err)
	}
	conflicting := make(map[string][]byte, len(secondTarget.Files))
	for rel := range secondTarget.Files {
		conflicting[rel] = []byte("ORIGINAL USER CONTENT\n")
	}
	writeFiles(t, secondTarget.TargetPaths, conflicting)

	var preflightErr error
	for _, plan := range []*Plan{firstTarget, secondTarget} {
		if err := PreflightWrite(plan); err != nil {
			preflightErr = err
			break
		}
	}
	if preflightErr == nil {
		t.Fatal("expected PreflightWrite to catch the second target's conflict")
	}
	if !errors.Is(preflightErr, ErrWriteConflict) {
		t.Fatalf("err = %v, want ErrWriteConflict", preflightErr)
	}

	if _, statErr := os.Stat(filepath.Join(firstTarget.TargetPaths, "SKILL.md")); !os.IsNotExist(statErr) {
		t.Fatalf("the first target was written even though a later target failed preflight, stat err = %v", statErr)
	}
}

// Regression tests for security-relevant agent fields:
// agent_config_adapter.go only decoded configured frontmatter keys, so
// Claude's disallowedTools/permissionMode/ hooks vanished before loss was
// ever computed, and denied-tool loss onto claude reported only as ordinary
// degraded fidelity.

// reviewerWithClaudeSecurityFields is a Claude agent with disallowedTools,
// permissionMode, and hooks — none of which agents/claude.yml maps into the
// Agent IR.
const reviewerWithClaudeSecurityFields = `---
name: reviewer
description: Review only
disallowedTools: [Bash, Write]
permissionMode: plan
hooks: {}
---
Review only.
`

// TestClaudeSecurityFieldsSurfaceAsLoss: migrating a
// Claude agent carrying disallowedTools/permissionMode/hooks to opencode
// must never report "no loss" — those fields have no Agent-IR mapping and
// must appear in the loss report as Security-flagged LossDropped items, not
// silently disappear before the check ever sees them.
func TestClaudeSecurityFieldsSurfaceAsLoss(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte(reviewerWithClaudeSecurityFields),
	})

	claude := NewClaudeAgentAdapter()
	ag, err := claude.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	opencode := NewOpencodeAgentAdapter()
	_, loss, err := opencode.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	if len(loss) == 0 {
		t.Fatal("claude -> opencode reported no loss despite disallowedTools/permissionMode/hooks having no opencode equivalent")
	}
	if !HasSecurityLoss(loss) {
		t.Fatalf("expected at least one Security-flagged loss item, got %#v", loss)
	}

	wantFields := map[string]bool{"disallowedTools": false, "permissionMode": false, "hooks": false}
	for _, l := range loss {
		if _, tracked := wantFields[l.Field]; tracked {
			wantFields[l.Field] = true
			if l.Kind != LossDropped {
				t.Errorf("field %s reported as %s, want %s", l.Field, l.Kind, LossDropped)
			}
			if !l.Security {
				t.Errorf("field %s not flagged Security — an unrepresentable restriction must never look like ordinary fidelity loss", l.Field)
			}
		}
	}
	for field, seen := range wantFields {
		if !seen {
			t.Errorf("expected a loss item naming %q, got %#v", field, loss)
		}
	}
}

// TestProjectDoesNotSilentlyDropUnknownNonSecurityField is a control:
// an unrecognized field NOT in agentSecurityFrontmatterKeys (e.g. a made-up
// "color" field) must not be captured — the fix targets the reviewed
// security-relevant keys only, not every unknown field.
func TestProjectDoesNotSilentlyDropUnknownNonSecurityField(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\nname: reviewer\ndescription: d\ncolor: blue\n---\n\nbody\n"),
	})
	claude := NewClaudeAgentAdapter()
	ag, err := claude.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ag.UnmappedSecurityFields) != 0 {
		t.Fatalf("expected color (not a reviewed security field) to be ignored, got %#v", ag.UnmappedSecurityFields)
	}
}

// Regression tests for unmapped non-security frontmatter fields silently
// disappearing, resource filtering, executable-mode loss, and symlinked
// skill roots.

// TestAgentUnmappedNonSecurityFieldRoundTripsSameProvider covers the
// non-security case: an unrecognized, non-security frontmatter field
// must survive a same-provider (claude -> claude) round trip instead of
// vanishing the moment Load parses only its configured field set.
func TestAgentUnmappedNonSecurityFieldRoundTripsSameProvider(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\nname: reviewer\ndescription: d\ncustom-field: keep-me\n---\n\nbody\n"),
	})
	claude := NewClaudeAgentAdapter()
	ag, err := claude.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ag.UnmappedFields) != 1 || ag.UnmappedFields[0].Key != "custom-field" {
		t.Fatalf("expected custom-field captured as an UnmappedField, got %#v", ag.UnmappedFields)
	}

	files, loss, err := claude.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for _, l := range loss {
		if l.Field == "custom-field" {
			t.Fatalf("same-provider round trip must not report custom-field as loss, got %#v", loss)
		}
	}
	if !strings.Contains(string(files["reviewer.md"]), "custom-field: keep-me") {
		t.Fatalf("same-provider round trip must re-emit custom-field, got:\n%s", files["reviewer.md"])
	}
}

// TestAgentUnmappedNonSecurityFieldReportedAsLossCrossProvider is the
// cross-provider control: projecting onto a DIFFERENT provider must report
// the unmapped field as loss rather than either silently dropping it or
// inventing a foreign-schema equivalent for it.
func TestAgentUnmappedNonSecurityFieldReportedAsLossCrossProvider(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\nname: reviewer\ndescription: d\ncustom-field: keep-me\n---\n\nbody\n"),
	})
	claude := NewClaudeAgentAdapter()
	ag, err := claude.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	opencode := NewOpencodeAgentAdapter()
	files, loss, err := opencode.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if strings.Contains(string(files["reviewer.md"]), "custom-field") {
		t.Fatalf("cross-provider projection must never invent a foreign-schema mapping for an unmapped field, got:\n%s", files["reviewer.md"])
	}
	var found bool
	for _, l := range loss {
		if l.Field == "custom-field" {
			found = true
			if l.Kind != LossDropped {
				t.Errorf("custom-field reported as %s, want %s", l.Kind, LossDropped)
			}
		}
	}
	if !found {
		t.Fatalf("expected custom-field named in the cross-provider loss report, got %#v", loss)
	}
}

// TestMigrationPreservesFileOutsideRecommendedDirectory:
// a stray top-level helper file (not in scripts/references/assets) survived
// LoadGenericSkill's unscoped walk but disappeared on the SECOND load — once
// installed into a real provider's directory and re-loaded through the
// provider-config-scoped loadResources call, before this fix.
func TestMigrationPreservesFileOutsideRecommendedDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	skillDir := filepath.Join(home, ".claude", "skills", "pkg-tool")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: pkg-tool\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "helper.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("write helper.py: %v", err)
	}

	src, err := NewClaudeAdapter().Load(skillDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := src.Resources["helper.py"]; !ok {
		t.Fatalf("helper.py must load even though it's not under scripts/references/assets, got %#v", src.Resources)
	}

	files, loss, err := NewCodexAdapter().Project(src)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if _, ok := files["helper.py"]; !ok {
		t.Fatalf("helper.py must survive migration to another provider, got files %#v", files)
	}
	for _, l := range loss {
		if l.Field == "Resources" && strings.Contains(l.Note, "helper.py") {
			t.Fatalf("helper.py must not be reported as loss when it round-trips, got %#v", loss)
		}
	}
}

// TestExcludedControlMetadataReportedAsLoss confirms a real exclusion
// (generated/VCS state) is never silent: it must show up as a named loss
// item rather than the migration reporting clean "no loss".
func TestExcludedControlMetadataReportedAsLoss(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	skillDir := filepath.Join(home, ".claude", "skills", "pkg-tool")
	if err := os.MkdirAll(filepath.Join(skillDir, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: pkg-tool\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "node_modules", "pkg", "index.js"), []byte("module.exports={}"), 0o644); err != nil {
		t.Fatalf("write index.js: %v", err)
	}

	src, err := NewClaudeAdapter().Load(skillDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(src.ExcludedResources) != 1 {
		t.Fatalf("expected node_modules excluded and reported, got %#v", src.ExcludedResources)
	}

	_, loss, err := NewCodexAdapter().Project(src)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	var found bool
	for _, l := range loss {
		if l.Field == "Resources" && l.Kind == LossDropped {
			found = true
		}
	}
	if !found {
		t.Fatalf("excluded control metadata must appear as a LossDropped item, got %#v", loss)
	}
}

// TestExecutableBitSurvivesMigration: a 0755 resource
// script must still be 0755 on disk after Load -> Migrate -> Write, not
// silently collapsed to 0644 (which would break `./scripts/run.sh`).
func TestExecutableBitSurvivesMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	skillDir := filepath.Join(home, ".claude", "skills", "runner")
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: runner\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	scriptPath := filepath.Join(skillDir, "scripts", "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write scripts/run.sh: %v", err)
	}

	src, err := NewClaudeAdapter().Load(skillDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	written := filepath.Join(plan.TargetPaths, "scripts", "run.sh")
	fi, err := os.Stat(written)
	if err != nil {
		t.Fatalf("stat %s: %v", written, err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("written scripts/run.sh mode = %v, want 0755 — executable bit lost", fi.Mode().Perm())
	}
}

// TestSymlinkedSkillRootResourcesLoad: an explicitly
// selected skill root that is itself a symlink must still have its
// resources discovered on Load, not silently migrate with the script
// missing (filepath.WalkDir does not descend into a symlinked root on its
// own).
func TestSymlinkedSkillRootResourcesLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	realDir := filepath.Join(home, "dotfiles", "pkg-tool")
	if err := os.MkdirAll(filepath.Join(realDir, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "SKILL.md"), []byte("---\nname: pkg-tool\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write scripts/run.sh: %v", err)
	}

	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir skills dir: %v", err)
	}
	linkPath := filepath.Join(skillsDir, "pkg-tool")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Skipf("symlink not supported on this platform/filesystem: %v", err)
	}

	src, err := NewClaudeAdapter().Load(linkPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := src.Resources["scripts/run.sh"]; !ok {
		t.Fatalf("symlinked skill root's resources must still load, got %#v", src.Resources)
	}
}

// TestSymlinkedSkillDiscoveredByList covers the `skill list` side:
// a symlinked skill directory was previously invisible to List (DirEntry.
// IsDir() doesn't follow a symlink) even though named resolution accepted
// it — an inconsistency between listing and migration for the identical
// skill.
func TestSymlinkedSkillDiscoveredByList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	realDir := filepath.Join(home, "dotfiles", "pkg-tool")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "SKILL.md"), []byte("---\nname: pkg-tool\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir skills dir: %v", err)
	}
	linkPath := filepath.Join(skillsDir, "pkg-tool")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Skipf("symlink not supported on this platform/filesystem: %v", err)
	}

	refs, err := List(NewClaudeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, r := range refs {
		if r.Name == "pkg-tool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("symlinked skill directory must be discovered by List, got %#v", refs)
	}
}

// TestDeniedToolsLossIsFlaggedSecurity: migrating an
// opencode agent with an explicitly-denied tool onto claude (an
// allowlist-only shape with no boolean-denial equivalent) must flag that
// loss as Security — restricted-agent semantics, not cosmetic reshaping —
// so it can never be waved through as ordinary degraded fidelity.
func TestDeniedToolsLossIsFlaggedSecurity(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\ndescription: Reviews code.\ntools:\n  read: true\n  write: false\n---\n\nReview the diff.\n"),
	})

	oc := NewOpencodeAgentAdapter()
	ag, err := oc.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	claude := NewClaudeAgentAdapter()
	_, loss, err := claude.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	if !HasSecurityLoss(loss) {
		t.Fatalf("expected the denied-tool loss to be Security-flagged, got %#v", loss)
	}
	var found bool
	for _, l := range loss {
		if l.Field == "Tools" && l.Kind == LossDropped {
			found = true
			if !l.Security {
				t.Errorf("Tools LossDropped item not flagged Security: %#v", l)
			}
		}
	}
	if !found {
		t.Fatalf("expected a Tools LossDropped item, got %#v", loss)
	}
}

// TestToolIdentityTranslatedNotCasePreserved: a Claude list such as
// `Read, Grep, Bash` must not become case-preserved opencode keys. A
// case-preserved key matches none of opencode's real (lowercase) permission
// keys, so the restriction is not merely mis-cased — it is inert. Project()
// must translate tool identity into the target's vocabulary.
func TestToolIdentityTranslatedNotCasePreserved(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\nname: reviewer\ndescription: d\ntools: Read, Grep, Bash\n---\n\nbody\n"),
	})
	claude := NewClaudeAgentAdapter()
	ag, err := claude.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	opencode := NewOpencodeAgentAdapter()
	files, _, err := opencode.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	content := string(files["reviewer.md"])
	for _, want := range []string{"read: true", "grep: true", "bash: true"} {
		if !strings.Contains(content, want) {
			t.Errorf("expected translated lowercase key %q in projected opencode content, got:\n%s", want, content)
		}
	}
	for _, unwanted := range []string{"Read: true", "Grep: true", "Bash: true"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("case-preserved key %q must not survive translation, got:\n%s", unwanted, content)
		}
	}
}

// TestClaudeAllowlistDefaultDenyPreservedOnOpencode: merely writing `true`
// for named tools does not establish that all other tools are forbidden. Claude's
// tools: CSV is a strict allowlist (presence implies "only these"), but
// opencode's {tool: bool} map defaults an unlisted key to allowed —
// projecting the allowlist without an explicit "*": false entry would
// silently turn a restricted agent into an unrestricted one.
func TestClaudeAllowlistDefaultDenyPreservedOnOpencode(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\nname: reviewer\ndescription: d\ntools: Read\n---\n\nbody\n"),
	})
	claude := NewClaudeAgentAdapter()
	ag, err := claude.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	opencode := NewOpencodeAgentAdapter()
	files, _, err := opencode.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	content := string(files["reviewer.md"])
	if !strings.Contains(content, "'*': false") {
		t.Fatalf("expected an explicit default-deny wildcard preserving the claude allowlist's restriction, got:\n%s", content)
	}

	// Control: an agent with NO tools restriction at all must not gain a
	// synthesized wildcard — there is nothing to preserve.
	writeFiles(t, dir, map[string][]byte{
		"unrestricted.md": []byte("---\nname: unrestricted\ndescription: d\n---\n\nbody\n"),
	})
	ag2, err := claude.Load(filepath.Join(dir, "unrestricted.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	files2, _, err := opencode.Project(ag2)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if strings.Contains(string(files2["unrestricted.md"]), "*") {
		t.Fatalf("an agent with no tools restriction must not gain a synthesized wildcard, got:\n%s", files2["unrestricted.md"])
	}
}

// TestOpencodeWildcardDenyRoundTripsToClaudeWithoutFalseLoss confirms
// the complementary direction: an opencode agent whose ONLY denial is the
// "*" wildcard (i.e. exactly Claude's own allowlist semantics, expressed
// in opencode's shape) migrates to claude with NO Tools loss reported —
// Claude's allowlist already implies "everything else denied" for free, so
// this is not an unrepresentable restriction. A genuine SPECIFIC per-tool
// denial (deny just this one tool, default-allow the rest) has no
// allowlist-only equivalent and must still be flagged.
func TestOpencodeWildcardDenyRoundTripsToClaudeWithoutFalseLoss(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\ndescription: d\ntools:\n  read: true\n  bash: true\n  '*': false\n---\n\nbody\n"),
	})
	oc := NewOpencodeAgentAdapter()
	ag, err := oc.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	claude := NewClaudeAgentAdapter()
	_, loss, err := claude.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if HasSecurityLoss(loss) {
		t.Fatalf("a wildcard-only denial is exactly claude's own allowlist semantics — expected no security loss, got %#v", loss)
	}

	// Control: a SPECIFIC per-tool denial (no wildcard) still has no
	// allowlist-only equivalent and must still be flagged.
	writeFiles(t, dir, map[string][]byte{
		"specific.md": []byte("---\ndescription: d\ntools:\n  read: true\n  write: false\n---\n\nbody\n"),
	})
	ag2, err := oc.Load(filepath.Join(dir, "specific.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, loss2, err := claude.Project(ag2)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !HasSecurityLoss(loss2) {
		t.Fatalf("a specific per-tool denial has no allowlist-only equivalent and must be flagged, got %#v", loss2)
	}
}

// Regression tests for nested Cursor skills: a skill nested under Cursor's
// recursive project scope (.cursor/skills/group/demo/SKILL.md) appeared in
// `skill list` but `skill scan demo --from cursor --scope project` (and, by
// the same code path, migrate/diff/uninstall) reported not found — List used
// a recursive walk but ResolveSkillPath only checked the non-recursive
// <dir>/<name>/SKILL.md shape.

func TestNestedCursorSkillListedAndResolvableByName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	writeFiles(t, filepath.Join(dir, ".cursor", "skills", "group", "demo"), map[string][]byte{
		"SKILL.md": []byte("---\nname: demo\ndescription: d\n---\n\nbody\n"),
	})

	cursor := NewCursorAdapter()
	refs, err := List(cursor, ScopeProject)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var listed bool
	for _, r := range refs {
		if r.Name == "demo" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("expected the nested skill to appear in List(), got %#v", refs)
	}

	got, err := ResolveSkillPath(cursor, ScopeProject, "demo")
	if err != nil {
		t.Fatalf("expected the nested skill listed above to also resolve by name, got: %v", err)
	}
	want := filepath.Join(dir, ".cursor", "skills", "group", "demo")
	if got != want {
		t.Fatalf("ResolveSkillPath = %q, want %q", got, want)
	}
}

// TestAmbiguousNestedNameIsExplicitError confirms the flip side of the
// unification: two nested entries sharing the same name within a single
// recursive directory tree are an explicit, deterministic error from BOTH
// List and ResolveSkillPath — never a silent first-match pick that would
// let `skill list` and a subsequent `skill scan`/`migrate` disagree about
// which one "demo" actually refers to.
func TestAmbiguousNestedNameIsExplicitError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	writeFiles(t, filepath.Join(dir, ".cursor", "skills", "group-a", "demo"), map[string][]byte{
		"SKILL.md": []byte("---\nname: demo\ndescription: a\n---\n\nbody\n"),
	})
	writeFiles(t, filepath.Join(dir, ".cursor", "skills", "group-b", "demo"), map[string][]byte{
		"SKILL.md": []byte("---\nname: demo\ndescription: b\n---\n\nbody\n"),
	})

	cursor := NewCursorAdapter()
	if _, err := List(cursor, ScopeProject); err == nil {
		t.Fatal("expected List to report the duplicate name as an explicit error, got nil")
	}
	if _, err := ResolveSkillPath(cursor, ScopeProject, "demo"); err == nil {
		t.Fatal("expected ResolveSkillPath to report the duplicate name as an explicit error, got nil")
	}
}

// Regression tests for per-kind IR field validation: config.go's validate()
// accepted the UNION of Skill IR and Agent IR field names, so a skill
// provider config declaring an agent-only field (e.g. "tools", "temperature")
// was accepted at validate() time and only failed later, confusingly, at
// Load/Project with "unknown canonical IR field" — the config's own schema
// check never caught the kind mismatch it was supposedly there to catch.

func TestSkillConfigRejectsAgentOnlyIRField(t *testing.T) {
	raw := []byte(`
id: bad-skill
dirs:
  user: [{ path: "~/.bad-skill/skills", role: own }]
  project: [{ path: ".bad-skill/skills", role: own }]
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
  - { ir: tools, key: tools, type: string-or-list, presence: optional }
`)
	_, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames(), KindSkill)
	if err == nil {
		t.Fatal("expected a skill config declaring the agent-only \"tools\" IR field to be rejected at validate() time")
	}
	if !strings.Contains(err.Error(), "tools") {
		t.Errorf("expected the error to name the offending field, got: %v", err)
	}
}

func TestAgentConfigRejectsSkillOnlyIRField(t *testing.T) {
	raw := []byte(`
id: bad-agent
layout: flat
dirs:
  user: [{ path: "~/.bad-agent/agents", role: own }]
  project: [{ path: ".bad-agent/agents", role: own }]
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
  - { ir: allowed_tools, key: allowed-tools, type: string-or-list, presence: optional }
`)
	_, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames(), KindAgent)
	if err == nil {
		t.Fatal("expected an agent config declaring the skill-only \"allowed_tools\" IR field to be rejected at validate() time")
	}
}

// TestSkillFileAndSidecarPathRejectPathEscape confirms skill_file and
// sidecar.path get the SAME containment check dirs.*.path already had —
// both are joined onto a resolved skill directory downstream
// (config_adapter.go's Load/Project, hooks.go's codexOpenAICodec).
func TestSkillFileAndSidecarPathRejectPathEscape(t *testing.T) {
	base := `
id: escape-test
skill_file: %s
dirs:
  user: [{ path: "~/.escape-test/skills", role: own }]
  project: [{ path: ".escape-test/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd"} {
		raw := []byte(fmt.Sprintf(base, bad))
		if _, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames(), KindSkill); err == nil {
			t.Errorf("expected skill_file %q to be rejected", bad)
		}
	}

	sidecarBase := `
id: escape-sidecar-test
dirs:
  user: [{ path: "~/.escape-sidecar-test/skills", role: own }]
  project: [{ path: ".escape-sidecar-test/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
sidecar: { path: %s, codec: codex-openai }
`
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd"} {
		raw := []byte(fmt.Sprintf(sidecarBase, bad))
		if _, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames(), KindSkill); err == nil {
			t.Errorf("expected sidecar.path %q to be rejected", bad)
		}
	}
}

// TestResolveHonorsProviderCustomNameRegex: a provider config's custom
// name_regex is honored by Load/Project (via the adapter's own validateName),
// but the public ResolveSkillPath/ResolveAgentPath validated every name
// against the package-default lowercase-hyphen regex regardless — so a name a
// custom-regex provider's own artifacts actually use could be rejected by the
// very functions migrate/scan/diff rely on to find them by name.
func TestResolveHonorsProviderCustomNameRegex(t *testing.T) {
	raw := []byte(`
id: strict-upper
name_regex: "^[A-Z]+$"
layout: flat
dirs:
  user: [{ path: "~/.strict-upper/agents", role: own }]
  project: [{ path: ".strict-upper/agents", role: own }]
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
  - { ir: description, key: description, type: string, presence: required }
`)
	cfg, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames(), KindAgent)
	if err != nil {
		t.Fatalf("parseProviderConfig: %v", err)
	}
	a := newAgentConfigAdapter(*cfg)

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".strict-upper", "agents")
	writeFiles(t, dir, map[string][]byte{
		"REVIEWER.md": []byte("---\ndescription: d\n---\n\nbody\n"),
	})

	// "REVIEWER" matches this provider's own ^[A-Z]+$ regex but NOT the
	// package-default ^[a-z0-9]+(-[a-z0-9]+)*$ — ResolveAgentPath must use
	// the provider's own rule, not silently reject a name its own Load
	// would accept.
	if _, err := ResolveAgentPath(a, ScopeUser, "REVIEWER"); err != nil {
		t.Fatalf("expected ResolveAgentPath to honor the provider's custom name_regex, got: %v", err)
	}
}

// The scanner previously retained the full matched secret in
// ScanFinding.Excerpt, and every scan output path (CLI print, JSON, logs)
// renders that field verbatim. Safe behavior: the original secret substring
// must never appear in a finding's Excerpt, for both Skill and Agent
// scanning.

var syntheticGithubToken = "ghp" + "_" + "ABCDEFGHIJ0123456789abcdefghij01234567"

func TestSkillScanRedactsSecretExcerpt(t *testing.T) {
	s := &Skill{
		Name:        "leaky-skill",
		Description: "a skill that accidentally hardcodes a credential",
		Body:        "Set your token: " + syntheticGithubToken + "\n",
	}
	result := Scan(s)

	var found bool
	for _, f := range result.Findings {
		if f.Pattern != "github-token" {
			continue
		}
		found = true
		if strings.Contains(f.Excerpt, syntheticGithubToken) {
			t.Fatalf("scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding, got %#v", result.Findings)
	}
}

func TestAgentScanRedactsSecretExcerpt(t *testing.T) {
	a := &Agent{
		Name:        "leaky-agent",
		Description: "an agent that accidentally hardcodes a credential",
		Body:        "Set your token: " + syntheticGithubToken + "\n",
	}
	result := AgentScan(a)

	var found bool
	for _, f := range result.Findings {
		if f.Pattern != "github-token" {
			continue
		}
		found = true
		if strings.Contains(f.Excerpt, syntheticGithubToken) {
			t.Fatalf("agent scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding, got %#v", result.Findings)
	}
}

// TestSkillScanResourceSecretRedacted covers the resource-file scan
// path (not just SKILL.md body) to ensure redaction applies uniformly.
func TestSkillScanResourceSecretRedacted(t *testing.T) {
	s := &Skill{
		Name:        "leaky-skill-resource",
		Description: "a skill whose bundled resource hardcodes a credential",
		Body:        "See scripts/setup.sh\n",
		Resources: map[string]ResourceFile{
			"scripts/setup.sh": {Data: []byte("export TOKEN=" + syntheticGithubToken + "\n"), Mode: 0o644},
		},
	}
	result := Scan(s)

	var found bool
	for _, f := range result.Findings {
		if f.Pattern != "github-token" {
			continue
		}
		found = true
		if strings.Contains(f.Excerpt, syntheticGithubToken) {
			t.Fatalf("resource scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding in resource scan, got %#v", result.Findings)
	}
}

// The scanner previously covered only SKILL.md's body and already-loaded
// resources: a credential placed in frontmatter went undetected ("no
// findings", exit 0), and a binary resource or a loader-excluded file
// (symlink, control metadata) vanished from the scan report with no trace of
// ever having existed. Safe behavior: every original input is either scanned
// or explicitly disclosed as skipped with a reason, and a clean result is
// never presented as more than a heuristic lint outcome.

func TestFrontmatterCredentialIsDetected(t *testing.T) {
	s := &Skill{
		Name:        "frontmatter-secret-skill",
		Description: "a skill with a credential hidden in frontmatter metadata, not the body",
		Body:        "This skill has an entirely clean body with no risky content.\n",
		Metadata:    map[string]string{"contact-token": syntheticGithubToken},
	}
	result := Scan(s)

	var found bool
	for _, f := range result.Findings {
		if f.Pattern == "github-token" && f.File == "frontmatter:metadata.contact-token" {
			found = true
			if strings.Contains(f.Excerpt, syntheticGithubToken) {
				t.Fatalf("frontmatter finding excerpt contains the raw secret: %q", f.Excerpt)
			}
		}
	}
	if !found {
		t.Fatalf("expected a frontmatter metadata finding, got %#v", result.Findings)
	}
}

func TestBinaryResourceDisclosedAsSkipped(t *testing.T) {
	s := &Skill{
		Name:        "binary-resource-skill",
		Description: "a skill bundling a binary asset that cannot be pattern-matched as text",
		Body:        "See assets/logo.png\n",
		Resources: map[string]ResourceFile{
			"assets/logo.png": {Data: []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, 0x03}, Mode: 0o644},
		},
	}
	result := Scan(s)

	for _, name := range result.Scanned {
		if name == "assets/logo.png" {
			t.Fatalf("binary resource was pattern-matched as text instead of being skipped: %#v", result.Scanned)
		}
	}
	var found bool
	for _, sk := range result.Skipped {
		if sk.File == "assets/logo.png" {
			found = true
			if sk.Reason == "" {
				t.Fatalf("skipped binary resource has no disclosed reason")
			}
		}
	}
	if !found {
		t.Fatalf("binary resource silently vanished from the scan report instead of being disclosed as skipped: %#v", result.Skipped)
	}
}

func TestLoaderExcludedResourceDisclosedAsSkipped(t *testing.T) {
	s := &Skill{
		Name:        "excluded-resource-skill",
		Description: "a skill whose loader already excluded a symlinked resource file",
		Body:        "body\n",
		ExcludedResources: []ExcludedResource{
			{Path: "scripts/evil-link", Reason: "symlinked or non-regular file, refused (untrusted resource link)"},
		},
	}
	result := Scan(s)

	var found bool
	for _, sk := range result.Skipped {
		if sk.File == "scripts/evil-link" && sk.Reason == "symlinked or non-regular file, refused (untrusted resource link)" {
			found = true
		}
	}
	if !found {
		t.Fatalf("loader-excluded resource not disclosed in scan Skipped, got %#v", result.Skipped)
	}
}

// TestScoreDisclaimerDoesNotOverstateTrust guards against the score
// (or a "no findings" result) ever being re-labeled as an execution-safety
// or trust signal — it is exactly and only "no configured heuristic
// pattern matched".
func TestScoreDisclaimerDoesNotOverstateTrust(t *testing.T) {
	if !strings.Contains(ScoreDisclaimer, "not an execution-safety") {
		t.Fatalf("score disclaimer must state it is not an execution-safety/trust certification, got %q", ScoreDisclaimer)
	}

	s := &Skill{
		Name:        "clean-skill",
		Description: "a completely clean skill with no risky patterns anywhere in it",
		Body:        "This skill has a long, entirely benign body with nothing suspicious in it at all.\n",
	}
	result := Scan(s)
	if len(result.Findings) != 0 {
		t.Fatalf("expected no findings for a clean skill, got %#v", result.Findings)
	}
	// A clean scan (zero findings) is a fact about pattern-matching, not a
	// safety verdict — callers must render it via ScoreDisclaimer, never a
	// bare "safe"/"trusted" claim.
}

// Regression test for object-shaped Codex dependencies: a
// `dependencies.tools` entry per the current vendor schema
// (https://learn.chatgpt.com/docs/build-skills) is an object with type,
// value, description, transport, and url — not a bare string. Before the
// fix, hooks.go declared `Tools []string`, so this fixture failed to
// decode with "cannot unmarshal !!map into string" instead of loading (and
// later round-tripping) successfully.
func TestCodexObjectShapedDependencyRoundTrips(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "demo")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: demo\ndescription: d\n---\n\nbody\n"),
		"agents/openai.yaml": []byte(`dependencies:
  tools:
    - type: mcp
      value: example
      description: Example tool
      transport: streamable_http
      url: https://example.invalid/mcp
`),
	})

	codex := NewCodexAdapter()
	s, err := codex.Load(skillDir)
	if err != nil {
		t.Fatalf("Load must decode the documented object-shaped dependency, got: %v", err)
	}
	want := []CodexToolDependency{{
		Type:        "mcp",
		Value:       "example",
		Description: "Example tool",
		Transport:   "streamable_http",
		URL:         "https://example.invalid/mcp",
	}}
	if s.CodexTools == nil || !reflect.DeepEqual(s.CodexTools.Tools, want) {
		t.Fatalf("CodexTools = %#v, want Tools=%#v", s.CodexTools, want)
	}

	files, loss, err := codex.Project(s)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(loss) != 0 {
		t.Fatalf("same-provider round trip must report no loss, got %#v", loss)
	}

	tmp := t.TempDir()
	reDir := filepath.Join(tmp, "demo")
	writeFiles(t, reDir, files)
	got, err := codex.Load(reDir)
	if err != nil {
		t.Fatalf("re-Load of the projected sidecar failed: %v", err)
	}
	if !reflect.DeepEqual(got.CodexTools, s.CodexTools) {
		t.Fatalf("CodexTools did not round-trip, got %#v, want %#v", got.CodexTools, s.CodexTools)
	}

	// LoadGenericSkill (used by install.go for an arbitrary local directory
	// of unknown provider) shares the same codec — must decode identically.
	generic, err := LoadGenericSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadGenericSkill must also decode the object-shaped dependency, got: %v", err)
	}
	if generic.CodexTools == nil || !reflect.DeepEqual(generic.CodexTools.Tools, want) {
		t.Fatalf("LoadGenericSkill CodexTools = %#v, want Tools=%#v", generic.CodexTools, want)
	}
}

// Regression tests for provider root paths corrected against current
// vendor documentation, with the previously-shipped (incorrect) root kept
// as an explicit READ-only compatibility fallback, never a write/rollback
// target (see providers/
// opencode.yml's plural "agents" directories and providers/windsurf.yml's
// "~/.codeium/windsurf/skills" root).
//
// opencode's skill provider config (providers/opencode.yml) is unaffected —
// it never used the singular "agent" path in the first place. The
// documented-vs-actual mismatch is in the opencode AGENT provider config
// (agents/opencode.yml), which previously wrote agents to
// singular ".opencode/agent" / "~/.config/opencode/agent" instead of the
// documented plural "agents" directories.
func TestOpencodeAgentProviderUsesDocumentedPluralRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	oc := NewOpencodeAgentAdapter()
	dirs := oc.UserDirs()
	if len(dirs) == 0 || dirs[0] != filepath.Join(home, ".config", "opencode", "agents") {
		t.Fatalf("opencode agent UserDirs()[0] = %#v, want primary root ~/.config/opencode/agents (documented plural root)", dirs)
	}

	projDirs := oc.ProjectDirs()
	if len(projDirs) == 0 || projDirs[0] != filepath.Join(".opencode", "agents") {
		t.Fatalf("opencode agent ProjectDirs()[0] = %#v, want .opencode/agents", projDirs)
	}
}

// TestOpencodeAgentLegacySingularRootIsReadOnlyFallback confirms the
// old singular "agent" root is still searched (so a pre-existing install
// under the old path is still found/migratable) but is NEVER counted as an
// owned/writable directory — Detect(), OwnUserDirCount(), and rollback's
// write-target resolution must never treat it as the provider's own
// directory.
func TestOpencodeAgentLegacySingularRootIsReadOnlyFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	legacyDir := filepath.Join(home, ".config", "opencode", "agent")
	writeFiles(t, legacyDir, map[string][]byte{
		"reviewer.md": []byte("---\ndescription: legacy install\n---\n\nbody\n"),
	})

	oc := NewOpencodeAgentAdapter()
	dirs := oc.UserDirs()
	var sawLegacy bool
	for _, d := range dirs {
		if d == legacyDir {
			sawLegacy = true
		}
	}
	if !sawLegacy {
		t.Fatalf("legacy singular root must remain a read fallback in UserDirs(), got %#v", dirs)
	}

	if n := oc.OwnUserDirCount(); n != 1 {
		t.Fatalf("OwnUserDirCount() = %d, want 1 — the legacy compat root must never be counted as own/writable", n)
	}

	path, err := ResolveAgentPath(oc, ScopeUser, "reviewer")
	if err != nil {
		t.Fatalf("expected the legacy install to still be discoverable for read/migrate: %v", err)
	}
	if path != filepath.Join(legacyDir, "reviewer.md") {
		t.Fatalf("resolved path = %q, want the legacy root %q", path, legacyDir)
	}

	// The provider looks "not installed" via Detect() unless the NEW
	// (documented) root exists — a stale legacy directory alone must never
	// make an old root look like the provider's live install location.
	if oc.Detect() {
		t.Fatalf("Detect() must not report the provider installed based on the legacy compat root alone")
	}
}

func TestWindsurfSkillProviderUsesDocumentedUserRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	a, ok := AdapterByID(ProviderID("windsurf"))
	if !ok {
		t.Fatal("windsurf provider config not found")
	}
	dirs := a.UserDirs()
	want := filepath.Join(home, ".codeium", "windsurf", "skills")
	if len(dirs) == 0 || dirs[0] != want {
		t.Fatalf("windsurf UserDirs()[0] = %#v, want documented root %q", dirs, want)
	}

	legacy := filepath.Join(home, ".windsurf", "skills")
	var sawLegacy bool
	for _, d := range dirs {
		if d == legacy {
			sawLegacy = true
		}
	}
	if !sawLegacy {
		t.Fatalf("legacy ~/.windsurf/skills root must remain a read fallback, got %#v", dirs)
	}
	if n := a.OwnUserDirCount(); n != 1 {
		t.Fatalf("windsurf OwnUserDirCount() = %d, want 1 — legacy compat root must not be own/writable", n)
	}
}

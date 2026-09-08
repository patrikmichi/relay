package agentport

// Regression tests for findings R05-R08
// (vault/reviews/relay/2026-09-07/relay-deep-codebase-review.md), derived
// from the evidence bundle's reproduce.py probes (destination-symlink-write,
// rollback-wrong-project, overwrite-rollback, strict-partial-write,
// failed-install-leaves-files). Do not weaken these assertions to obtain a
// green baseline.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR05_WriteRejectsDestinationSymlink reproduces the review's
// "destination-symlink-write" probe for the skill write engine: the
// destination file is itself a symlink to a file outside the artifact
// directory. Safe behavior: Write must refuse rather than follow it.
func TestR05_WriteRejectsDestinationSymlink(t *testing.T) {
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
		t.Fatal("R05: expected Write to refuse a symlinked destination file")
	} else if !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("err = %v, want ErrDestinationSymlink", err)
	}

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "SAFE SENTINEL\n" {
		t.Fatalf("R05: write mutated the symlink target outside the artifact directory: %q", data)
	}
}

// TestR05_WriteAgentRejectsDestinationSymlink is the agent write engine
// analogue of TestR05_WriteRejectsDestinationSymlink.
func TestR05_WriteAgentRejectsDestinationSymlink(t *testing.T) {
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
		t.Fatal("R05: expected WriteAgent to refuse a symlinked destination file")
	} else if !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("err = %v, want ErrDestinationSymlink", err)
	}

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "SAFE SENTINEL\n" {
		t.Fatalf("R05: write mutated the symlink target outside the artifact directory: %q", data)
	}
}

// TestR05_RollbackRejectsSymlinkTarget covers the Rollback side of R05: an
// owned file is replaced with a symlink after installation (an attacker,
// or a corrupted reinstall). Safe behavior: Rollback must refuse to
// delete/read through it rather than acting on whatever it points to.
func TestR05_RollbackRejectsSymlinkTarget(t *testing.T) {
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
		t.Fatal("R05: expected Rollback to refuse deleting through a symlink")
	} else if !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("err = %v, want ErrDestinationSymlink", err)
	}

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "SAFE SENTINEL\n" {
		t.Fatalf("R05: rollback followed the symlink and touched the outside sentinel: %q", data)
	}

	m2, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m2.Entries) != 1 {
		t.Fatalf("expected the manifest entry to remain after a refused rollback, got %#v", m2.Entries)
	}
}

// TestR06_RollbackRejectsDifferentProject reproduces the review's
// "rollback-wrong-project" probe: install into project A, copy identical
// content into project B, then roll back A's entry from B. Safe behavior:
// Rollback must refuse rather than deleting B's file and dropping A's
// ledger entry.
func TestR06_RollbackRejectsDifferentProject(t *testing.T) {
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
		t.Fatal("R06: expected Rollback to refuse a rollback initiated from a different project")
	} else if !errors.Is(err, ErrAmbiguousProjectRollback) {
		t.Fatalf("err = %v, want ErrAmbiguousProjectRollback", err)
	}

	if _, err := os.Stat(bFile); err != nil {
		t.Fatalf("R06: expected project B's file to remain untouched: %v", err)
	}
	if _, err := os.Stat(aFile); err != nil {
		t.Fatalf("R06: expected project A's original installation to remain untouched: %v", err)
	}
	m2, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m2.Entries) != 1 {
		t.Fatalf("R06: expected A's ledger entry to remain after a refused cross-project rollback, got %#v", m2.Entries)
	}
}

// TestR06_RollbackRejectsLegacyProjectEntry: a project-scope manifest
// entry recorded before ProjectRoot existed decodes with it empty and must
// be rejected rather than trusted against whatever the current CWD is.
func TestR06_RollbackRejectsLegacyProjectEntry(t *testing.T) {
	entry := ManifestEntry{ID: "legacy-1", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeProject, Name: "demo"}
	if err := Rollback(entry, false); err == nil {
		t.Fatal("R06: expected Rollback to refuse a legacy project-scope entry with no recorded ProjectRoot")
	} else if !errors.Is(err, ErrAmbiguousProjectRollback) {
		t.Fatalf("err = %v, want ErrAmbiguousProjectRollback", err)
	}
}

// TestR07_WriteRejectsConflictingExistingContent reproduces the review's
// "overwrite-rollback" probe's setup: pre-existing content at the
// destination differs from what's about to be written. Safe behavior:
// Write must refuse rather than silently overwriting it (the prior
// behavior meant a subsequent rollback deleted the file entirely, with no
// way to recover the original bytes — R07's "no recovery path").
func TestR07_WriteRejectsConflictingExistingContent(t *testing.T) {
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
		t.Fatal("R07: expected Write to refuse to overwrite conflicting existing content")
	} else if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("err = %v, want ErrWriteConflict", err)
	}

	data, err := os.ReadFile(filepath.Join(plan.TargetPaths, "SKILL.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "ORIGINAL USER CONTENT\n" {
		t.Fatalf("R07: original content was overwritten: %q", data)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("R07: expected no manifest entry recorded for a refused write, got %#v", m.Entries)
	}
}

// TestR07_WriteAgentRejectsConflictingExistingContent is the agent write
// engine analogue of TestR07_WriteRejectsConflictingExistingContent.
func TestR07_WriteAgentRejectsConflictingExistingContent(t *testing.T) {
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
		t.Fatal("R07: expected WriteAgent to refuse to overwrite conflicting existing content")
	} else if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("err = %v, want ErrWriteConflict", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("R07: expected no manifest entry recorded for a refused write, got %#v", m.Entries)
	}
}

// TestR08_WriteFailsClosedWhenLedgerCorrupt reproduces the review's
// "failed-install-leaves-files" probe: the manifest ledger is corrupt
// before an install runs. Safe behavior: Write must check the ledger
// BEFORE touching the filesystem, so a corrupt ledger leaves no changed
// files behind (the prior behavior wrote the files first and only failed
// afterward on RecordEntry, leaving mutated files with no ledger record).
func TestR08_WriteFailsClosedWhenLedgerCorrupt(t *testing.T) {
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
		t.Fatal("R08: expected Write to fail closed when the ledger is corrupt")
	} else if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("err = %v, want ErrLedgerUnavailable", err)
	}

	if _, statErr := os.Stat(plan.TargetPaths); !os.IsNotExist(statErr) {
		t.Fatalf("R08: files were written to disk before the ledger check failed (target %s), stat err = %v", plan.TargetPaths, statErr)
	}
}

// TestR08_WriteAgentFailsClosedWhenLedgerCorrupt is the agent write engine
// analogue of TestR08_WriteFailsClosedWhenLedgerCorrupt.
func TestR08_WriteAgentFailsClosedWhenLedgerCorrupt(t *testing.T) {
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
		t.Fatal("R08: expected WriteAgent to fail closed when the ledger is corrupt")
	} else if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("err = %v, want ErrLedgerUnavailable", err)
	}

	if _, statErr := os.Stat(filepath.Join(plan.TargetPaths, "reviewer.md")); !os.IsNotExist(statErr) {
		t.Fatalf("R08: the agent file was written to disk before the ledger check failed, stat err = %v", statErr)
	}
}

// TestR08_PreflightWriteValidatesAllTargetsBeforeAnyWrite reproduces the
// review's "strict-partial-write" probe at the engine level: a multi-
// target migration (e.g. `relay skill migrate --to codex --to opencode`)
// must not write an earlier target when a later target fails preflight.
// PreflightWrite is exported precisely so a multi-target caller can
// validate every plan first — see preflight.go.
func TestR08_PreflightWriteValidatesAllTargetsBeforeAnyWrite(t *testing.T) {
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
		t.Fatal("R08: expected PreflightWrite to catch the second target's conflict")
	}
	if !errors.Is(preflightErr, ErrWriteConflict) {
		t.Fatalf("err = %v, want ErrWriteConflict", preflightErr)
	}

	if _, statErr := os.Stat(filepath.Join(firstTarget.TargetPaths, "SKILL.md")); !os.IsNotExist(statErr) {
		t.Fatalf("R08: the first target was written even though a later target failed preflight, stat err = %v", statErr)
	}
}

// Regression tests for R04 and R22: agent_config_adapter.go only decoded
// configured frontmatter keys, so Claude's disallowedTools/permissionMode/
// hooks vanished before loss was ever computed, and denied-tool loss onto
// claude reported only as ordinary degraded fidelity.

// reviewerWithClaudeSecurityFields mirrors the review's exact probe fixture
// (evidence/reproduce.py line 61): a Claude agent with disallowedTools,
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

// TestR04_ClaudeSecurityFieldsSurfaceAsLoss reproduces R04: migrating a
// Claude agent carrying disallowedTools/permissionMode/hooks to opencode
// must never report "no loss" — those fields have no Agent-IR mapping and
// must appear in the loss report as Security-flagged LossDropped items, not
// silently disappear before the check ever sees them.
func TestR04_ClaudeSecurityFieldsSurfaceAsLoss(t *testing.T) {
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
		t.Fatal("R04: claude -> opencode reported no loss despite disallowedTools/permissionMode/hooks having no opencode equivalent")
	}
	if !HasSecurityLoss(loss) {
		t.Fatalf("R04: expected at least one Security-flagged loss item, got %#v", loss)
	}

	wantFields := map[string]bool{"disallowedTools": false, "permissionMode": false, "hooks": false}
	for _, l := range loss {
		if _, tracked := wantFields[l.Field]; tracked {
			wantFields[l.Field] = true
			if l.Kind != LossDropped {
				t.Errorf("R04: field %s reported as %s, want %s", l.Field, l.Kind, LossDropped)
			}
			if !l.Security {
				t.Errorf("R04: field %s not flagged Security — an unrepresentable restriction must never look like ordinary fidelity loss", l.Field)
			}
		}
	}
	for field, seen := range wantFields {
		if !seen {
			t.Errorf("R04: expected a loss item naming %q, got %#v", field, loss)
		}
	}
}

// TestR04_ProjectDoesNotSilentlyDropUnknownNonSecurityField is a control:
// an unrecognized field NOT in agentSecurityFrontmatterKeys (e.g. a made-up
// "color" field) must not be captured — the fix targets the reviewed
// security-relevant keys only, not every unknown field.
func TestR04_ProjectDoesNotSilentlyDropUnknownNonSecurityField(t *testing.T) {
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

// TestR22_DeniedToolsLossIsFlaggedSecurity reproduces R22: migrating an
// opencode agent with an explicitly-denied tool onto claude (an
// allowlist-only shape with no boolean-denial equivalent) must flag that
// loss as Security — restricted-agent semantics, not cosmetic reshaping —
// so it can never be waved through as ordinary degraded fidelity.
func TestR22_DeniedToolsLossIsFlaggedSecurity(t *testing.T) {
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
		t.Fatalf("R22: expected the denied-tool loss to be Security-flagged, got %#v", loss)
	}
	var found bool
	for _, l := range loss {
		if l.Field == "Tools" && l.Kind == LossDropped {
			found = true
			if !l.Security {
				t.Errorf("R22: Tools LossDropped item not flagged Security: %#v", l)
			}
		}
	}
	if !found {
		t.Fatalf("R22: expected a Tools LossDropped item, got %#v", loss)
	}
}

// Regression tests derived from the 2026-09-07 deep review (R27) — see
// vault/reviews/relay/2026-09-07/relay-deep-codebase-review.md. The scanner
// previously retained the full matched secret in ScanFinding.Excerpt, and
// every scan output path (CLI print, JSON, logs) renders that field
// verbatim. Safe behavior: the original secret substring must never appear
// in a finding's Excerpt, for both Skill and Agent scanning.

const syntheticGithubToken = "ghp_ABCDEFGHIJ0123456789abcdefghij01234567"

func TestR27_SkillScanRedactsSecretExcerpt(t *testing.T) {
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
			t.Fatalf("R27: scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding, got %#v", result.Findings)
	}
}

func TestR27_AgentScanRedactsSecretExcerpt(t *testing.T) {
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
			t.Fatalf("R27: agent scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding, got %#v", result.Findings)
	}
}

// TestR27_SkillScanResourceSecretRedacted covers the resource-file scan
// path (not just SKILL.md body) to ensure redaction applies uniformly.
func TestR27_SkillScanResourceSecretRedacted(t *testing.T) {
	s := &Skill{
		Name:        "leaky-skill-resource",
		Description: "a skill whose bundled resource hardcodes a credential",
		Body:        "See scripts/setup.sh\n",
		Resources: map[string][]byte{
			"scripts/setup.sh": []byte("export TOKEN=" + syntheticGithubToken + "\n"),
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
			t.Fatalf("R27: resource scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding in resource scan, got %#v", result.Findings)
	}
}

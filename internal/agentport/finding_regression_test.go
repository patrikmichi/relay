package agentport

// Regression tests derived from the 2026-09-07 deep review's reproduction
// evidence (vault/reviews/relay/2026-09-07/evidence/reproduce.py's
// "permission-loss" probe and cli-probes.jsonl). The evidence bundle only
// carried a black-box CLI probe for these two findings, not a Go test body
// — these are written fresh per Phase 0's documented gap. They fail against
// baseline 931aac4 (agent_config_adapter.go only decodes configured
// frontmatter keys, so disallowedTools/permissionMode/hooks vanish before
// loss is ever computed) and must pass once P1.4's fix lands in the same
// commit.

import (
	"path/filepath"
	"testing"
)

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
// an unrecognized frontmatter field that is NOT in the reviewed
// security-field table (e.g. a made-up "color" field) is out of scope for
// this containment fix — Phase 4 handles full unknown-field fidelity. This
// asserts the security-specific fix doesn't (yet) claim to catch every
// unknown field, only the reviewed security-relevant ones, so a future
// broadening of agentSecurityFrontmatterKeys is a deliberate, reviewed edit.
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

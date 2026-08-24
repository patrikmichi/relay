package agentport

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestAgentConfigAdapter_ClaudeRoundTrip exercises Load -> Project for the
// claude agent adapter against a realistic fixture (mirroring
// claude-infra/shared-claude/agents/*.md's verified shape).
func TestAgentConfigAdapter_ClaudeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"backend-developer.md": []byte("---\n" +
			"name: backend-developer\n" +
			"description: Implements backend features.\n" +
			"tools: Read, Write, Bash\n" +
			"model: sonnet\n" +
			"memory: project\n" +
			"skills:\n  - api-design\n  - error-handling\n" +
			"---\n\nYou implement backend features.\n"),
	})

	a := NewClaudeAgentAdapter()
	if a.ID() != ProviderClaude {
		t.Fatalf("ID() = %s, want %s", a.ID(), ProviderClaude)
	}

	ag, err := a.Load(filepath.Join(dir, "backend-developer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ag.Name != "backend-developer" || ag.Description != "Implements backend features." {
		t.Fatalf("unexpected agent: %#v", ag)
	}
	if ag.Model != "sonnet" || ag.Memory != "project" {
		t.Fatalf("unexpected agent: %#v", ag)
	}
	if len(ag.Tools) != 3 || len(ag.Skills) != 2 {
		t.Fatalf("Tools/Skills = %#v/%#v", ag.Tools, ag.Skills)
	}
	if ag.Provenance.SourceProvider != ProviderClaude {
		t.Fatalf("Provenance = %#v", ag.Provenance)
	}

	files, loss, err := a.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	content, ok := files["backend-developer.md"]
	if !ok {
		t.Fatalf("Project files = %#v, want a backend-developer.md entry", files)
	}
	if !strings.Contains(string(content), "name: backend-developer") {
		t.Errorf("projected content missing name field: %s", content)
	}
	if !strings.Contains(string(content), "You implement backend features.") {
		t.Errorf("projected content missing body: %s", content)
	}
	// Same-provider round trip (claude -> claude): Model needs no mapping,
	// so no Model loss item; Tools stays list-shaped natively, so no Tools
	// loss item either.
	for _, l := range loss {
		if l.Field == "Model" || l.Field == "Tools" {
			t.Errorf("unexpected %s loss on a same-provider round trip: %#v", l.Field, l)
		}
	}
}

// TestAgentConfigAdapter_OpencodeNameInferredFromFilename confirms the
// opencode agent adapter infers Name from the filename (no on-disk name
// field mapped) and reports the expected degraded/dropped losses when
// projecting an opencode-sourced agent back onto claude.
func TestAgentConfigAdapter_OpencodeNameInferredFromFilename(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\ndescription: Reviews code.\nmode: subagent\nmodel: anthropic/claude-sonnet-4\ntemperature: 0.2\n---\n\nReview the diff.\n"),
	})

	oc := NewOpencodeAgentAdapter()
	ag, err := oc.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ag.Name != "reviewer" {
		t.Fatalf("Name = %q, want inferred from filename", ag.Name)
	}
	if ag.Mode != "subagent" || ag.Temperature == nil || *ag.Temperature != 0.2 {
		t.Fatalf("unexpected agent: %#v", ag)
	}

	// Project the opencode-sourced agent onto claude: Temperature/Mode are
	// dropped (claude has no equivalent), and Model degrades via the
	// alias-mapping table (opencode "anthropic/claude-sonnet-4" -> claude
	// "sonnet").
	claude := NewClaudeAgentAdapter()
	files, loss, err := claude.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	content := string(files["reviewer.md"])
	if !strings.Contains(content, "model: sonnet") {
		t.Errorf("expected the mapped claude model alias in projected content: %s", content)
	}

	gotKinds := map[string]LossKind{}
	for _, l := range loss {
		gotKinds[l.Field] = l.Kind
	}
	if gotKinds["Temperature"] != LossDropped {
		t.Errorf("Temperature loss = %v, want %v", gotKinds["Temperature"], LossDropped)
	}
	if gotKinds["Mode"] != LossDropped {
		t.Errorf("Mode loss = %v, want %v", gotKinds["Mode"], LossDropped)
	}
	if gotKinds["Model"] != LossDegraded {
		t.Errorf("Model loss = %v, want %v", gotKinds["Model"], LossDegraded)
	}
}

// TestAgentConfigAdapter_OpencodeLoadCapturesDeniedTools confirms Load
// populates Agent.DeniedTools from `tool: false` on-disk entries — the
// information toolsMapToList alone would otherwise silently discard (Fix
// for the opencode explicitly-denied-tools fidelity gap).
func TestAgentConfigAdapter_OpencodeLoadCapturesDeniedTools(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\ndescription: Reviews code.\ntools:\n  read: true\n  write: false\n  bash: false\n---\n\nReview the diff.\n"),
	})

	oc := NewOpencodeAgentAdapter()
	ag, err := oc.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ag.Tools) != 1 || ag.Tools[0] != "read" {
		t.Fatalf("Tools = %#v, want [read]", ag.Tools)
	}
	wantDenied := []string{"bash", "write"}
	if !reflect.DeepEqual(ag.DeniedTools, wantDenied) {
		t.Fatalf("DeniedTools = %#v, want %#v", ag.DeniedTools, wantDenied)
	}
}

// TestAgentConfigAdapter_OpencodeRoundTripPreservesDeniedTools confirms an
// opencode -> opencode round trip re-encodes DeniedTools as `tool: false`
// entries instead of silently dropping them.
func TestAgentConfigAdapter_OpencodeRoundTripPreservesDeniedTools(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.md": []byte("---\ndescription: Reviews code.\ntools:\n  read: true\n  write: false\n---\n\nReview the diff.\n"),
	})

	oc := NewOpencodeAgentAdapter()
	ag, err := oc.Load(filepath.Join(dir, "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	files, loss, err := oc.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	content := string(files["reviewer.md"])
	if !strings.Contains(content, "write: false") {
		t.Errorf("expected the projected opencode file to preserve `write: false`: %s", content)
	}
	if !strings.Contains(content, "read: true") {
		t.Errorf("expected the projected opencode file to preserve `read: true`: %s", content)
	}

	// opencode preserves denial semantics, so there is no LossDropped Tools
	// item on an opencode -> opencode round trip — only the pre-existing
	// LossDegraded shape-reshape note (list <-> map) applies.
	for _, l := range loss {
		if l.Field == "Tools" && l.Kind == LossDropped {
			t.Errorf("unexpected LossDropped Tools item on an opencode round trip (denial should be preserved, not dropped): %#v", l)
		}
	}
}

// TestAgentConfigAdapter_OpencodeToClaudeMigrateSurfacesDeniedToolsLoss
// confirms migrating an opencode agent with explicitly-denied tools onto
// claude (an allowlist-only shape with no boolean-denial equivalent)
// surfaces a LossDropped item naming the denied tools, rather than
// silently omitting them with no record at all.
func TestAgentConfigAdapter_OpencodeToClaudeMigrateSurfacesDeniedToolsLoss(t *testing.T) {
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
	files, loss, err := claude.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	content := string(files["reviewer.md"])
	if strings.Contains(content, "write") {
		t.Errorf("claude's allowlist-only tools field should not mention the denied tool at all: %s", content)
	}

	var deniedLoss *LossItem
	for i, l := range loss {
		if l.Field == "Tools" && l.Kind == LossDropped {
			deniedLoss = &loss[i]
		}
	}
	if deniedLoss == nil {
		t.Fatalf("expected a LossDropped Tools item for the explicitly-denied tool, loss = %#v", loss)
	}
	if !strings.Contains(deniedLoss.Note, "write") {
		t.Errorf("loss note should name the denied tool: %q", deniedLoss.Note)
	}
}

// TestAgentConfigAdapter_Detect exercises Detect() against a temp $HOME
// with and without the provider's own user dir present.
func TestAgentConfigAdapter_Detect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	a := NewClaudeAgentAdapter()
	if a.Detect() {
		t.Fatalf("Detect() = true before ~/.claude/agents exists")
	}

	if err := os.MkdirAll(filepath.Join(home, ".claude", "agents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !a.Detect() {
		t.Fatalf("Detect() = false after ~/.claude/agents exists")
	}
}

// TestAgentConfigAdapter_DirsAndCapabilities exercises ProjectDirs,
// Capabilities, and Own*DirCount — every shipped agent provider has
// exactly one own user/project dir (no legacy/compat agent dirs yet).
func TestAgentConfigAdapter_DirsAndCapabilities(t *testing.T) {
	for _, a := range AllAgentAdapters() {
		if got := a.OwnUserDirCount(); got != 1 {
			t.Errorf("%s: OwnUserDirCount() = %d, want 1", a.ID(), got)
		}
		if got := a.OwnProjectDirCount(); got != 1 {
			t.Errorf("%s: OwnProjectDirCount() = %d, want 1", a.ID(), got)
		}
		if len(a.ProjectDirs()) == 0 {
			t.Errorf("%s: ProjectDirs() is empty", a.ID())
		}
		caps := a.Capabilities()
		if !caps.Model || !caps.Tools {
			t.Errorf("%s: Capabilities() = %#v, want Model and Tools both true for every shipped provider", a.ID(), caps)
		}
	}

	claudeCaps := agentCapSetFor(ProviderClaude)
	if !claudeCaps.Memory || !claudeCaps.Skills || claudeCaps.Temperature || claudeCaps.Mode {
		t.Errorf("claude AgentCapSet = %#v, want Memory/Skills true, Temperature/Mode false", claudeCaps)
	}
	opencodeCaps := agentCapSetFor(ProviderOpencode)
	if opencodeCaps.Memory || opencodeCaps.Skills || !opencodeCaps.Temperature || !opencodeCaps.Mode {
		t.Errorf("opencode AgentCapSet = %#v, want Memory/Skills false, Temperature/Mode true", opencodeCaps)
	}
}

// TestAgentConfigAdapter_LoadRejectsInvalidName confirms an invalid inferred
// name (e.g. an uppercase filename) surfaces as an error, not a silently
// accepted Agent.
func TestAgentConfigAdapter_LoadRejectsInvalidName(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"Not_Valid.md": []byte("---\ndescription: d\n---\n\nbody\n"),
	})
	oc := NewOpencodeAgentAdapter()
	if _, err := oc.Load(filepath.Join(dir, "Not_Valid.md")); err == nil {
		t.Fatalf("Load: expected an error for an invalid inferred name")
	}
}

// TestAgentConfigAdapter_ProjectRejectsInvalidName confirms Project
// validates the agent's Name before serializing.
func TestAgentConfigAdapter_ProjectRejectsInvalidName(t *testing.T) {
	a := NewClaudeAgentAdapter()
	if _, _, err := a.Project(&Agent{Name: "Not Valid", Description: "d"}); err == nil {
		t.Fatalf("Project: expected an error for an invalid name")
	}
}

// TestAllAgentAdapters_StableOrder confirms AllAgentAdapters returns the 2
// shipped providers in the documented order (claude, then opencode).
func TestAllAgentAdapters_StableOrder(t *testing.T) {
	adapters := AllAgentAdapters()
	if len(adapters) < 2 {
		t.Fatalf("AllAgentAdapters() = %d adapters, want at least 2", len(adapters))
	}
	if adapters[0].ID() != ProviderClaude || adapters[1].ID() != ProviderOpencode {
		t.Fatalf("AllAgentAdapters() order = [%s, %s], want [claude, opencode]", adapters[0].ID(), adapters[1].ID())
	}
}

// TestAgentAdapterByID_UnknownReturnsFalse confirms an unregistered
// provider id (e.g. codex/cursor — no agent-file primitive) reports
// ok=false rather than a zero-value adapter.
func TestAgentAdapterByID_UnknownReturnsFalse(t *testing.T) {
	if _, ok := AgentAdapterByID(ProviderCodex); ok {
		t.Fatalf("AgentAdapterByID(codex): ok = true, want false (no agent provider config)")
	}
}

// TestAgentConfigAdapter_CustomNameRegex exercises the (currently unused by
// either shipped agent config, but supported) custom name_regex field —
// the Agent-IR analogue of TestParseProviderConfig_CustomNameRegex.
func TestAgentConfigAdapter_CustomNameRegex(t *testing.T) {
	raw := []byte(`
id: strict-upper-agent
name_regex: "^[A-Z]+$"
layout: flat
dirs:
  user: [{ path: "~/.strict/agents", role: own }]
  project: [{ path: ".strict/agents", role: own }]
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
  - { ir: description, key: description, type: string, presence: required }
`)
	cfg, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames())
	if err != nil {
		t.Fatalf("parseProviderConfig: %v", err)
	}
	a := newAgentConfigAdapter(*cfg)

	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"ABC.md": []byte("---\nname: ABC\ndescription: d\n---\n\nbody\n"),
	})
	if _, err := a.Load(filepath.Join(dir, "ABC.md")); err != nil {
		t.Fatalf("Load with custom name_regex accepting uppercase: %v", err)
	}

	writeFiles(t, dir, map[string][]byte{
		"abc.md": []byte("---\nname: abc\ndescription: d\n---\n\nbody\n"),
	})
	if _, err := a.Load(filepath.Join(dir, "abc.md")); err == nil {
		t.Fatalf("Load with custom name_regex: expected an error for lowercase name")
	}

	if err := a.validateName(""); err == nil {
		t.Fatalf("validateName(\"\"): expected an error for empty name under a custom regex")
	}
	if _, _, err := a.Project(&Agent{Name: "abc", Description: "d"}); err == nil {
		t.Fatalf("Project: expected an error for a name that fails the custom regex")
	}
}

// TestAgentConfigAdapter_LoadErrors exercises Load's error paths: a
// missing file, and a malformed frontmatter field value.
func TestAgentConfigAdapter_LoadErrors(t *testing.T) {
	a := NewClaudeAgentAdapter()

	if _, err := a.Load(filepath.Join(t.TempDir(), "does-not-exist.md")); err == nil {
		t.Fatalf("Load(missing file): expected an error")
	}

	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		// tools is declared string-or-list; a nested mapping value fails to decode.
		"bad-tools.md": []byte("---\nname: bad-tools\ndescription: d\ntools:\n  nested: true\n---\n\nbody\n"),
	})
	if _, err := a.Load(filepath.Join(dir, "bad-tools.md")); err == nil {
		t.Fatalf("Load(malformed tools field): expected an error")
	}
}

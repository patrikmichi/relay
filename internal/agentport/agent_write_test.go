package agentport

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type dirlessAdapter struct{}

func (dirlessAdapter) ID() ProviderID        { return "dirless" }
func (dirlessAdapter) UserDirs() []string    { return nil }
func (dirlessAdapter) ProjectDirs() []string { return nil }

func reviewerAgent() *Agent {
	return &Agent{
		Name:        "reviewer",
		Description: "Reviews code.",
		Body:        "Review the diff.\n",
		Provenance:  Provenance{SourceProvider: ProviderClaude},
	}
}

func TestMigrateAgent_RejectsInvalidInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	claude := NewClaudeAgentAdapter()

	if _, err := MigrateAgent(nil, claude, ScopeUser); err == nil {
		t.Error("nil source accepted")
	}
	if _, err := MigrateAgent(reviewerAgent(), nil, ScopeUser); err == nil {
		t.Error("nil target accepted")
	}
	bad := reviewerAgent()
	bad.Name = "../escape"
	if _, err := MigrateAgent(bad, claude, ScopeUser); err == nil {
		t.Error("traversal name accepted")
	}
}

func TestAgentTargetDir_Errors(t *testing.T) {
	if _, err := AgentTargetDir(nil, ScopeUser); err == nil {
		t.Error("nil adapter accepted")
	}
	for _, scope := range []Scope{ScopeUser, ScopeProject} {
		if _, err := AgentTargetDir(dirlessAdapter{}, scope); err == nil || !strings.Contains(err.Error(), "no directories") {
			t.Errorf("AgentTargetDir(%s) = %v, want no-directories error", scope, err)
		}
		if _, err := TargetDir(dirlessAdapter{}, scope, "x"); err == nil {
			t.Errorf("TargetDir(%s) accepted an adapter with no directories", scope)
		}
	}
	if _, err := TargetDir(nil, ScopeUser, "x"); err == nil {
		t.Error("TargetDir accepted a nil adapter")
	}
}

func TestAgentTargetDir_ProjectScopeIsAbsoluteAndFlat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	t.Chdir(root)
	claude := NewClaudeAgentAdapter()
	dir, err := AgentTargetDir(claude, ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) || !strings.HasPrefix(dir, root) {
		t.Fatalf("project dir %q is not an absolute path under %s", dir, root)
	}
	if filepath.Base(dir) == "reviewer" {
		t.Fatalf("agent dir %q must not contain a per-agent subdirectory", dir)
	}
}

func TestWriteAgent_UserScopeRecordsLedgerEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := NewClaudeAgentAdapter()

	plan, err := MigrateAgent(reviewerAgent(), claude, ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}
	if plan.HasDropped() {
		t.Fatalf("same-provider plan reports dropped fields: %+v", plan.Loss)
	}
	if err := WriteAgent(plan); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}

	dest := filepath.Join(plan.TargetPaths, "reviewer.md")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("written agent missing: %v", err)
	}
	if string(got) != string(plan.Files["reviewer.md"]) {
		t.Fatalf("written bytes differ from the plan")
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := LastEntryOfKind(m, KindAgent)
	if !ok {
		t.Fatal("no agent entry recorded")
	}
	if entry.Name != "reviewer" || entry.Provider != ProviderClaude || entry.ProjectRoot != "" || entry.TransactionID == "" {
		t.Fatalf("unexpected entry %+v", entry)
	}
	if _, ok := LastEntryOfKind(m, KindSkill); ok {
		t.Fatal("agent write recorded a skill entry")
	}

	// Writing identical bytes again is allowed; different bytes conflict.
	if err := WriteAgent(plan); err != nil {
		t.Fatalf("idempotent rewrite: %v", err)
	}
	changed := reviewerAgent()
	changed.Body = "Something else.\n"
	plan2, err := MigrateAgent(changed, claude, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAgent(plan2); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("conflicting rewrite = %v, want ErrWriteConflict", err)
	}
	if got2, _ := os.ReadFile(dest); string(got2) != string(got) {
		t.Fatal("conflicting rewrite changed the destination")
	}
}

func TestWriteAgent_ProjectScopeRecordsProjectRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	t.Chdir(root)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := MigrateAgent(reviewerAgent(), NewClaudeAgentAdapter(), ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAgent(plan); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}
	m, _ := LoadManifest()
	entry, ok := LastEntryOfKind(m, KindAgent)
	if !ok || entry.ProjectRoot != wd || entry.Scope != ScopeProject {
		t.Fatalf("entry = %+v, want project scope rooted at %s", entry, wd)
	}
}

func TestWriteAgent_RefusesSecurityLossAndNilPlan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := WriteAgent(nil); err == nil {
		t.Fatal("nil plan accepted")
	}
	plan, err := MigrateAgent(reviewerAgent(), NewClaudeAgentAdapter(), ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	plan.Loss = append(plan.Loss, LossItem{Field: "permissionMode", Kind: LossDropped, Security: true})
	if !plan.HasDropped() {
		t.Fatal("HasDropped false with a dropped item")
	}
	err = WriteAgent(plan)
	if err == nil || !strings.Contains(err.Error(), "permissionMode") {
		t.Fatalf("WriteAgent = %v, want refusal naming the security field", err)
	}
	if _, statErr := os.Stat(filepath.Join(plan.TargetPaths, "reviewer.md")); !os.IsNotExist(statErr) {
		t.Fatal("refused write still created the destination")
	}
}

func TestWriteAgent_RefusesSymlinkedDestination(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	plan, err := MigrateAgent(reviewerAgent(), NewClaudeAgentAdapter(), ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(plan.TargetPaths, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.Symlink(outside, filepath.Join(plan.TargetPaths, "reviewer.md")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgent(plan); !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("WriteAgent = %v, want ErrDestinationSymlink", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("write followed the symlink outside the agent directory")
	}
}

func TestMigrate_RejectsInvalidInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	claude, ok := AdapterByID(ProviderClaude)
	if !ok {
		t.Fatal("claude adapter missing")
	}
	if _, err := Migrate(nil, claude, ScopeUser); err == nil {
		t.Error("nil source accepted")
	}
	if _, err := Migrate(&Skill{Name: "ok"}, nil, ScopeUser); err == nil {
		t.Error("nil target accepted")
	}
	if _, err := Migrate(&Skill{Name: "Bad Name"}, claude, ScopeUser); err == nil {
		t.Error("invalid name accepted")
	}
	if err := Write(nil); err == nil {
		t.Error("Write accepted a nil plan")
	}
	p := &Plan{Loss: []LossItem{{Kind: LossDegraded}}}
	if p.HasDropped() {
		t.Error("HasDropped true without a dropped item")
	}
	p.Loss = append(p.Loss, LossItem{Kind: LossDropped})
	if !p.HasDropped() {
		t.Error("HasDropped false with a dropped item")
	}
}

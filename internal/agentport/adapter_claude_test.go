package agentport

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestClaudeAdapter_RoundTrip(t *testing.T) {
	a := NewClaudeAdapter()

	src, err := a.Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if src.Name != "git-helper" {
		t.Fatalf("Name = %q, want git-helper", src.Name)
	}
	if src.License != "MIT" {
		t.Fatalf("License = %q, want MIT", src.License)
	}
	wantTools := []string{"Bash(git diff *)", "Bash(git status *)"}
	if !reflect.DeepEqual(src.AllowedTools, wantTools) {
		t.Fatalf("AllowedTools = %#v, want %#v", src.AllowedTools, wantTools)
	}
	if src.DisableModelInvocation == nil || *src.DisableModelInvocation != false {
		t.Fatalf("DisableModelInvocation = %v, want pointer to false", src.DisableModelInvocation)
	}
	if len(src.Resources) != 1 {
		t.Fatalf("Resources = %#v, want 1 entry (scripts/run.sh)", src.Resources)
	}
	if _, ok := src.Resources["scripts/run.sh"]; !ok {
		t.Fatalf("missing scripts/run.sh in Resources: %#v", src.Resources)
	}

	files, loss, err := a.Project(src)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(loss) != 0 {
		t.Fatalf("Loss = %#v, want none (round trip within the same provider)", loss)
	}
	if _, ok := files["SKILL.md"]; !ok {
		t.Fatalf("Project() files missing SKILL.md: %#v", files)
	}

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, src.Name)
	writeFiles(t, skillDir, files)

	got, err := a.Load(skillDir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}

	sameCommon(t, got, src)
	if got.License != src.License {
		t.Errorf("License = %q, want %q", got.License, src.License)
	}
	if !reflect.DeepEqual(got.AllowedTools, src.AllowedTools) {
		t.Errorf("AllowedTools = %#v, want %#v", got.AllowedTools, src.AllowedTools)
	}
	if got.DisableModelInvocation == nil || *got.DisableModelInvocation != *src.DisableModelInvocation {
		t.Errorf("DisableModelInvocation = %v, want %v", got.DisableModelInvocation, src.DisableModelInvocation)
	}
}

func TestClaudeAdapter_LegacyCommandFile(t *testing.T) {
	a := NewClaudeAdapter()

	s, err := a.Load(filepath.Join("testdata", "claude-legacy", "old-command.md"))
	if err != nil {
		t.Fatalf("Load legacy command file: %v", err)
	}
	if s.Name != "old-command" {
		t.Fatalf("Name = %q, want old-command", s.Name)
	}
	if s.Description == "" {
		t.Fatalf("Description should not be empty")
	}
	if len(s.Resources) != 0 {
		t.Fatalf("legacy command file should have no resources, got %#v", s.Resources)
	}
}

func TestClaudeAdapter_NameMustMatchDirName(t *testing.T) {
	a := NewClaudeAdapter()
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "wrong-dir-name")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: git-helper\ndescription: mismatched name test\n---\n\nbody\n"),
	})

	if _, err := a.Load(skillDir); err == nil {
		t.Fatalf("expected error when frontmatter name does not match directory name")
	}
}

func TestClaudeAdapter_Capabilities(t *testing.T) {
	caps := NewClaudeAdapter().Capabilities()
	if !caps.AllowedTools || !caps.DisableModelInvocation || !caps.License {
		t.Fatalf("unexpected Capabilities: %#v", caps)
	}
	if caps.Paths || caps.AllowImplicitInvocation || caps.CodexInterface || caps.CodexTools || caps.Compatibility || caps.Metadata {
		t.Fatalf("Claude should not claim unsupported capabilities: %#v", caps)
	}
}

package agentport

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestCodexAdapter_RoundTrip(t *testing.T) {
	a := NewCodexAdapter()

	src, err := a.Load(filepath.Join("testdata", "codex", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if src.Name != "git-helper" {
		t.Fatalf("Name = %q, want git-helper", src.Name)
	}
	if src.CodexInterface == nil {
		t.Fatalf("CodexInterface should be populated from the openai.yaml sidecar")
	}
	if src.CodexInterface.DisplayName != "Git Helper" {
		t.Fatalf("CodexInterface.DisplayName = %q, want Git Helper", src.CodexInterface.DisplayName)
	}
	if src.CodexInterface.BrandColor != "#f34f29" {
		t.Fatalf("CodexInterface.BrandColor = %q, want #f34f29", src.CodexInterface.BrandColor)
	}
	if src.AllowImplicitInvocation == nil || *src.AllowImplicitInvocation != true {
		t.Fatalf("AllowImplicitInvocation = %v, want pointer to true", src.AllowImplicitInvocation)
	}
	if src.CodexTools == nil || !reflect.DeepEqual(src.CodexTools.MCPServers, []string{"git-mcp"}) {
		t.Fatalf("CodexTools = %#v, want MCPServers=[git-mcp]", src.CodexTools)
	}
	if len(src.Resources) != 1 {
		t.Fatalf("Resources = %#v, want 1 entry (references/notes.md, sidecar excluded)", src.Resources)
	}
	if _, ok := src.Resources["references/notes.md"]; !ok {
		t.Fatalf("missing references/notes.md in Resources: %#v", src.Resources)
	}
	if _, ok := src.Resources["agents/openai.yaml"]; ok {
		t.Fatalf("sidecar agents/openai.yaml must not appear in Resources: %#v", src.Resources)
	}

	files, loss, err := a.Project(src)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(loss) != 0 {
		t.Fatalf("Loss = %#v, want none (round trip within the same provider)", loss)
	}
	if _, ok := files["agents/openai.yaml"]; !ok {
		t.Fatalf("Project() should re-emit the openai.yaml sidecar: %#v", files)
	}

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, src.Name)
	writeFiles(t, skillDir, files)

	got, err := a.Load(skillDir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}

	sameCommon(t, got, src)
	if !reflect.DeepEqual(got.CodexInterface, src.CodexInterface) {
		t.Errorf("CodexInterface = %#v, want %#v", got.CodexInterface, src.CodexInterface)
	}
	if got.AllowImplicitInvocation == nil || *got.AllowImplicitInvocation != *src.AllowImplicitInvocation {
		t.Errorf("AllowImplicitInvocation = %v, want %v", got.AllowImplicitInvocation, src.AllowImplicitInvocation)
	}
	if !reflect.DeepEqual(got.CodexTools, src.CodexTools) {
		t.Errorf("CodexTools = %#v, want %#v", got.CodexTools, src.CodexTools)
	}
}

func TestCodexAdapter_NoSidecarIsOptional(t *testing.T) {
	a := NewCodexAdapter()
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "plain-skill")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: plain-skill\ndescription: no sidecar here\n---\n\nbody\n"),
	})

	s, err := a.Load(skillDir)
	if err != nil {
		t.Fatalf("Load without sidecar should succeed: %v", err)
	}
	if s.CodexInterface != nil || s.AllowImplicitInvocation != nil || s.CodexTools != nil {
		t.Fatalf("expected no codex extras without a sidecar, got interface=%v policy=%v tools=%v",
			s.CodexInterface, s.AllowImplicitInvocation, s.CodexTools)
	}

	files, _, err := a.Project(s)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if _, ok := files["agents/openai.yaml"]; ok {
		t.Fatalf("Project() should not synthesize a sidecar when there are no codex extras: %#v", files)
	}
}

func TestCodexAdapter_Capabilities(t *testing.T) {
	caps := NewCodexAdapter().Capabilities()
	if !caps.AllowImplicitInvocation || !caps.CodexInterface || !caps.CodexTools {
		t.Fatalf("unexpected Capabilities: %#v", caps)
	}
	if caps.AllowedTools || caps.Paths || caps.DisableModelInvocation || caps.Compatibility || caps.License || caps.Metadata {
		t.Fatalf("Codex should not claim unsupported capabilities: %#v", caps)
	}
}

// TestCodexAdapter_UserDirsIncludesAdminScope closes the Wave-1 deferral:
// Codex also reads the admin (org-wide) scope /etc/codex/skills. It must
// appear in UserDirs() (so Detect()/ResolveSkillPath/List can read it) but
// NOT as dirs[0] (Migrate/Write/TargetDir always target dirs[0] — this
// scope is read-only, relay must never write there).
func TestCodexAdapter_UserDirsIncludesAdminScope(t *testing.T) {
	dirs := NewCodexAdapter().UserDirs()

	found := false
	for _, d := range dirs {
		if d == codexAdminSkillsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("UserDirs() = %v, want it to include the admin scope %q", dirs, codexAdminSkillsDir)
	}
	if dirs[0] == codexAdminSkillsDir {
		t.Fatalf("UserDirs()[0] must not be the read-only admin scope (that's the write target): %v", dirs)
	}
}

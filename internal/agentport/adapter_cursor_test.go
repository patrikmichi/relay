package agentport

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestCursorAdapter_RoundTrip(t *testing.T) {
	a := NewCursorAdapter()

	src, err := a.Load(filepath.Join("testdata", "cursor", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wantPaths := []string{"**/*.go", "**/*.ts"}
	if !reflect.DeepEqual(src.Paths, wantPaths) {
		t.Fatalf("Paths = %#v, want %#v", src.Paths, wantPaths)
	}
	if src.DisableModelInvocation == nil || *src.DisableModelInvocation != true {
		t.Fatalf("DisableModelInvocation = %v, want pointer to true", src.DisableModelInvocation)
	}
	wantMeta := map[string]string{"author": "acme"}
	if !reflect.DeepEqual(src.Metadata, wantMeta) {
		t.Fatalf("Metadata = %#v, want %#v", src.Metadata, wantMeta)
	}

	files, loss, err := a.Project(src)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(loss) != 0 {
		t.Fatalf("Loss = %#v, want none (round trip within the same provider)", loss)
	}

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, src.Name)
	writeFiles(t, skillDir, files)

	got, err := a.Load(skillDir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}

	sameCommon(t, got, src)
	if !reflect.DeepEqual(got.Paths, src.Paths) {
		t.Errorf("Paths = %#v, want %#v", got.Paths, src.Paths)
	}
	if got.DisableModelInvocation == nil || *got.DisableModelInvocation != *src.DisableModelInvocation {
		t.Errorf("DisableModelInvocation = %v, want %v", got.DisableModelInvocation, src.DisableModelInvocation)
	}
	if !reflect.DeepEqual(got.Metadata, src.Metadata) {
		t.Errorf("Metadata = %#v, want %#v", got.Metadata, src.Metadata)
	}
}

func TestCursorAdapter_PathsAcceptsCommaSeparatedString(t *testing.T) {
	a := NewCursorAdapter()
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "glob-skill")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: glob-skill\ndescription: comma separated paths\npaths: \"**/*.go, **/*.ts\"\n---\n\nbody\n"),
	})

	s, err := a.Load(skillDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"**/*.go", "**/*.ts"}
	if !reflect.DeepEqual(s.Paths, want) {
		t.Fatalf("Paths = %#v, want %#v", s.Paths, want)
	}
}

func TestCursorAdapter_Capabilities(t *testing.T) {
	caps := NewCursorAdapter().Capabilities()
	if !caps.Paths || !caps.DisableModelInvocation || !caps.Metadata {
		t.Fatalf("unexpected Capabilities: %#v", caps)
	}
	if caps.AllowedTools || caps.AllowImplicitInvocation || caps.CodexInterface || caps.CodexTools || caps.Compatibility || caps.License {
		t.Fatalf("Cursor should not claim unsupported capabilities: %#v", caps)
	}
}

package agentport

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestOpencodeAdapter_RoundTrip(t *testing.T) {
	a := NewOpencodeAdapter()

	src, err := a.Load(filepath.Join("testdata", "opencode", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if src.License != "MIT" {
		t.Fatalf("License = %q, want MIT", src.License)
	}
	if src.Compatibility != "opencode>=1.0.0" {
		t.Fatalf("Compatibility = %q, want opencode>=1.0.0", src.Compatibility)
	}
	wantMeta := map[string]string{"author": "acme", "team": "platform"}
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
	if got.License != src.License {
		t.Errorf("License = %q, want %q", got.License, src.License)
	}
	if got.Compatibility != src.Compatibility {
		t.Errorf("Compatibility = %q, want %q", got.Compatibility, src.Compatibility)
	}
	if !reflect.DeepEqual(got.Metadata, src.Metadata) {
		t.Errorf("Metadata = %#v, want %#v", got.Metadata, src.Metadata)
	}
}

func TestOpencodeAdapter_Capabilities(t *testing.T) {
	caps := NewOpencodeAdapter().Capabilities()
	if !caps.License || !caps.Compatibility || !caps.Metadata {
		t.Fatalf("unexpected Capabilities: %#v", caps)
	}
	if caps.AllowedTools || caps.Paths || caps.DisableModelInvocation || caps.AllowImplicitInvocation || caps.CodexInterface || caps.CodexTools {
		t.Fatalf("opencode should not claim unsupported capabilities: %#v", caps)
	}
}

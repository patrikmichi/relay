package agentport

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeFiles materializes a files map (as returned by Adapter.Project) under
// dir, creating parent directories as needed.
func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", dest, err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", dest, err)
		}
	}
}

// sameCommon asserts that two Skills agree on the fields every provider is
// expected to round-trip (name/description/body/resources), ignoring
// Provenance (which legitimately differs — different SourcePath/
// SourceProvider — across a round trip through a different temp dir or a
// different adapter).
func sameCommon(t *testing.T, got, want *Skill) {
	t.Helper()
	if got.Name != want.Name {
		t.Errorf("Name = %q, want %q", got.Name, want.Name)
	}
	if got.Description != want.Description {
		t.Errorf("Description = %q, want %q", got.Description, want.Description)
	}
	if got.Body != want.Body {
		t.Errorf("Body = %q, want %q", got.Body, want.Body)
	}
	if !reflect.DeepEqual(normalizeResources(got.Resources), normalizeResources(want.Resources)) {
		t.Errorf("Resources = %#v, want %#v", got.Resources, want.Resources)
	}
}

// normalizeResources treats nil and empty maps as equal (Load returns "{}"
// for a skill with no resource files; a hand-built fixture Skill{} literal
// leaves Resources nil).
func normalizeResources(m map[string][]byte) map[string][]byte {
	if len(m) == 0 {
		return map[string][]byte{}
	}
	return m
}

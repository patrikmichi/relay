package agentport

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectManifestDetectsDriftAndUnsafeFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	dir := filepath.Join(home, ".claude", "skills", "sample")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	body := []byte("original")
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	entry := ManifestEntry{ID: "new", Name: "sample", Kind: KindSkill, Provider: "claude", Scope: ScopeUser, TargetPaths: map[string]string{"SKILL.md": fmt.Sprintf("%x", sha256.Sum256(body))}, FileModes: map[string]uint32{"SKILL.md": 0644}}
	old := entry
	old.ID = "old"
	old.TargetPaths = map[string]string{"SKILL.md": "stale"}
	check := func(want bool) {
		t.Helper()
		r := InspectManifest(Manifest{Entries: []ManifestEntry{old, entry}})
		if len(r) != 1 || r[0].ID != "new" || r[0].OK != want {
			t.Fatalf("unexpected integrity: %+v", r)
		}
	}
	check(true)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(false)
	other := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(other, body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	check(false)
	entry.TargetPaths = map[string]string{}
	check(false)
}

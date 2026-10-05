package agentport

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadResources_SkipsSymlinks is the regression test for the
// SUPPLY-CHAIN issue: a resource file that's actually a symlink (e.g.
// scripts/x -> ~/.ssh/id_rsa in a malicious `relay skill install <path>`
// source package) must never have its target's content read into the
// Resources map, and the skip must be reported rather than silently
// dropped.
func TestLoadResources_SkipsSymlinks(t *testing.T) {
	skillDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("skill md"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write scripts/run.sh: %v", err)
	}

	// The "secret" a malicious skill package would try to exfiltrate by
	// symlinking a resource path to it.
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "id_rsa")
	secretContent := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n")
	if err := os.WriteFile(secretPath, secretContent, 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	symlinkRel := filepath.Join("scripts", "evil")
	symlinkAbs := filepath.Join(skillDir, symlinkRel)
	if err := os.Symlink(secretPath, symlinkAbs); err != nil {
		t.Skipf("symlink not supported on this platform/filesystem: %v", err)
	}

	resources, excluded, err := loadResources(skillDir, map[string]bool{"SKILL.md": true})
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}

	if _, ok := resources[filepath.ToSlash(symlinkRel)]; ok {
		t.Fatalf("symlinked resource must NEVER be read into Resources: %#v", resources)
	}
	for _, rf := range resources {
		if string(rf.Data) == string(secretContent) {
			t.Fatalf("secret content leaked into Resources: %#v", resources)
		}
	}

	if _, ok := resources["scripts/run.sh"]; !ok {
		t.Fatalf("legitimate regular-file resource should still load: %#v", resources)
	}

	found := false
	for _, ex := range excluded {
		if ex.Path == filepath.ToSlash(symlinkRel) {
			found = true
		}
	}
	if !found {
		t.Fatalf("excluded should report the symlinked resource path, got %#v", excluded)
	}
}

// TestLoadResources_SkipsSymlinkedDirectory ensures a symlinked directory
// (not just a symlinked file) is not traversed into either.
func TestLoadResources_SkipsSymlinkedDirectory(t *testing.T) {
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("skill md"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	symlinkAbs := filepath.Join(skillDir, "linked-dir")
	if err := os.Symlink(secretDir, symlinkAbs); err != nil {
		t.Skipf("symlink not supported on this platform/filesystem: %v", err)
	}

	resources, excluded, err := loadResources(skillDir, map[string]bool{"SKILL.md": true})
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}
	for rel := range resources {
		if rel == "linked-dir/secret.txt" {
			t.Fatalf("must not have descended into a symlinked directory: %#v", resources)
		}
	}
	if len(excluded) != 1 || excluded[0].Path != "linked-dir" {
		t.Fatalf("expected the symlinked directory itself reported as excluded, got %#v", excluded)
	}
}

// TestLoadResources_PreservesFilesOutsideRecommendedDirs: a stray top-level
// helper file and an entirely custom subdirectory (neither of which is
// scripts/references/assets) must still load — the recommended
// resource-directory names are documentation, not an exhaustive schema.
func TestLoadResources_PreservesFilesOutsideRecommendedDirs(t *testing.T) {
	skillDir := t.TempDir()

	write := func(rel, content string) {
		full := filepath.Join(skillDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("SKILL.md", "skill md")
	write("scripts/run.sh", "#!/bin/sh\necho hi\n")
	write("references/notes.md", "notes")
	write("helper.py", "print('hi')")
	write("custom-dir/file.txt", "a custom resource directory")

	resources, excluded, err := loadResources(skillDir, map[string]bool{"SKILL.md": true})
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}
	for _, want := range []string{"scripts/run.sh", "references/notes.md", "helper.py", "custom-dir/file.txt"} {
		if _, ok := resources[want]; !ok {
			t.Errorf("expected %s to be preserved regardless of directory convention, got resources: %#v", want, resources)
		}
	}
	if len(excluded) != 0 {
		t.Errorf("expected nothing excluded for ordinary regular files, got %#v", excluded)
	}
}

// TestLoadResources_ExcludesControlMetadata confirms generated/VCS state is
// still excluded (only the recommended-directory allowlist is gone, not
// all exclusion) and that every exclusion is reported.
func TestLoadResources_ExcludesControlMetadata(t *testing.T) {
	skillDir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(skillDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("SKILL.md", "skill md")
	write(".git/HEAD", "ref: refs/heads/main")
	write("node_modules/pkg/index.js", "module.exports = {}")
	write("package-lock.json", "{}")

	resources, excluded, err := loadResources(skillDir, map[string]bool{"SKILL.md": true})
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}
	for _, notWant := range []string{".git/HEAD", "node_modules/pkg/index.js", "package-lock.json"} {
		if _, ok := resources[notWant]; ok {
			t.Errorf("expected %s excluded as control metadata, got resources: %#v", notWant, resources)
		}
	}
	if len(excluded) != 3 {
		t.Errorf("expected 3 reported exclusions (.git, node_modules, package-lock.json), got %#v", excluded)
	}
}

// TestLoadResources_PreservesExecutableMode: a resource file's executable bit
// must survive the load, not collapse to 0644.
func TestLoadResources_PreservesExecutableMode(t *testing.T) {
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("skill md"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	scriptPath := filepath.Join(skillDir, "scripts", "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write scripts/run.sh: %v", err)
	}

	resources, _, err := loadResources(skillDir, map[string]bool{"SKILL.md": true})
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}
	rf, ok := resources["scripts/run.sh"]
	if !ok {
		t.Fatalf("scripts/run.sh missing from resources: %#v", resources)
	}
	if rf.Mode != 0o755 {
		t.Errorf("mode = %v, want 0755 (executable bit lost)", rf.Mode)
	}
}

// TestLoadResources_StripsSetuidBit confirms special/dangerous mode bits are
// never passed through, even though the executable bit is preserved: a setuid
// script must be written back as an ordinary 0755, never with the setuid bit
// intact.
func TestLoadResources_StripsSetuidBit(t *testing.T) {
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("skill md"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	scriptPath := filepath.Join(skillDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	if err := os.Chmod(scriptPath, 0o4755); err != nil {
		t.Skipf("chmod setuid not supported on this platform/filesystem: %v", err)
	}

	resources, _, err := loadResources(skillDir, map[string]bool{"SKILL.md": true})
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}
	rf, ok := resources["run.sh"]
	if !ok {
		t.Fatalf("run.sh missing from resources: %#v", resources)
	}
	if rf.Mode != 0o755 {
		t.Errorf("mode = %v, want exactly 0755 — setuid bit must never survive", rf.Mode)
	}
}

func TestWarnExcludedResources_NoOpWhenEmpty(t *testing.T) {
	// Just a smoke test that this doesn't panic with no excluded entries.
	warnExcludedResources("/some/dir", nil)
}

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

	resources, skipped, err := loadResources(skillDir, map[string]bool{"SKILL.md": true}, nil)
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}

	if _, ok := resources[filepath.ToSlash(symlinkRel)]; ok {
		t.Fatalf("symlinked resource must NEVER be read into Resources: %#v", resources)
	}
	for _, data := range resources {
		if string(data) == string(secretContent) {
			t.Fatalf("secret content leaked into Resources: %#v", resources)
		}
	}

	if _, ok := resources["scripts/run.sh"]; !ok {
		t.Fatalf("legitimate regular-file resource should still load: %#v", resources)
	}

	found := false
	for _, s := range skipped {
		if s == filepath.ToSlash(symlinkRel) {
			found = true
		}
	}
	if !found {
		t.Fatalf("skipped should report the symlinked resource path, got %#v", skipped)
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

	resources, skipped, err := loadResources(skillDir, map[string]bool{"SKILL.md": true}, nil)
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}
	for rel := range resources {
		if rel == "linked-dir/secret.txt" {
			t.Fatalf("must not have descended into a symlinked directory: %#v", resources)
		}
	}
	if len(skipped) != 1 || skipped[0] != "linked-dir" {
		t.Fatalf("expected the symlinked directory itself reported as skipped, got %#v", skipped)
	}
}

// TestLoadResources_ScopesToDeclaredResourceDirs is the regression test for
// ProviderConfig.ResourceDirs actually being consulted (previously a
// no-op TODO — see config.go). A stray top-level file, and an entire
// top-level directory not in resourceDirs, must both be excluded — while a
// nested file inside a declared resource dir still loads.
func TestLoadResources_ScopesToDeclaredResourceDirs(t *testing.T) {
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
	write("stray-top-level.txt", "should be excluded — not in resource_dirs")
	write("undeclared-dir/file.txt", "should be excluded — whole dir not in resource_dirs")

	resources, _, err := loadResources(skillDir, map[string]bool{"SKILL.md": true}, []string{"scripts", "references"})
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}

	for _, want := range []string{"scripts/run.sh", "references/notes.md"} {
		if _, ok := resources[want]; !ok {
			t.Errorf("expected %s to load (declared resource dir), got resources: %#v", want, resources)
		}
	}
	for _, notWant := range []string{"stray-top-level.txt", "undeclared-dir/file.txt"} {
		if _, ok := resources[notWant]; ok {
			t.Errorf("expected %s to be excluded (outside declared resource_dirs), got resources: %#v", notWant, resources)
		}
	}
}

// TestLoadResources_EmptyResourceDirsWalksEverything confirms the
// backward-compatible default: a nil/empty resourceDirs (e.g.
// LoadGenericSkill's call, which has no ProviderConfig) still walks the
// whole skillDir, unscoped.
func TestLoadResources_EmptyResourceDirsWalksEverything(t *testing.T) {
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
	write("stray-top-level.txt", "included when resourceDirs is unset")
	write("any-dir/file.txt", "included when resourceDirs is unset")

	resources, _, err := loadResources(skillDir, map[string]bool{"SKILL.md": true}, nil)
	if err != nil {
		t.Fatalf("loadResources: %v", err)
	}
	for _, want := range []string{"stray-top-level.txt", "any-dir/file.txt"} {
		if _, ok := resources[want]; !ok {
			t.Errorf("expected %s to load with unscoped (nil) resourceDirs, got resources: %#v", want, resources)
		}
	}
}

func TestWarnSkippedResources_NoOpWhenEmpty(t *testing.T) {
	// Just a smoke test that this doesn't panic with no skipped entries.
	warnSkippedResources("/some/dir", nil)
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillInstall_WritesToTargetAndRecordsManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A detected provider is required for --to to default; but here we
	// pass --to explicitly so detection doesn't matter.
	src := t.TempDir()
	srcDir := filepath.Join(src, "my-skill")
	writeSkillFiles(t, srcDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: my-skill\ndescription: a locally authored skill\n---\n\nbody\n"),
	})

	cmd := SkillInstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{srcDir, "--to", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	wantPath := filepath.Join(home, ".claude", "skills", "my-skill", "SKILL.md")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected %s to be written: %v\noutput:\n%s", wantPath, err, buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("written:")) {
		t.Errorf("expected output to report a written path, got:\n%s", buf.String())
	}
}

func TestSkillInstall_DryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := t.TempDir()
	srcDir := filepath.Join(src, "preview-skill")
	writeSkillFiles(t, srcDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: preview-skill\ndescription: d\n---\n\nbody\n"),
	})

	cmd := SkillInstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{srcDir, "--to", "claude", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	wantPath := filepath.Join(home, ".claude", "skills", "preview-skill")
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written under %s in dry-run, stat err = %v", wantPath, err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("[dry-run]")) {
		t.Errorf("expected dry-run marker in output, got:\n%s", buf.String())
	}
}

func TestSkillInstall_NoTargetsErrorsWhenNoneDetectedAndNoneSpecified(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // nothing detected

	src := t.TempDir()
	srcDir := filepath.Join(src, "orphan-skill")
	writeSkillFiles(t, srcDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: orphan-skill\ndescription: d\n---\n\nbody\n"),
	})

	cmd := SkillInstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{srcDir})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when no providers are detected and --to is omitted")
	}
}

func TestSkillInstall_InvalidPathErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := SkillInstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{filepath.Join(t.TempDir(), "does-not-exist"), "--to", "claude"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for a nonexistent source path")
	}
}

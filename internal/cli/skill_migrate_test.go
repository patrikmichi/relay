package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillMigrate_FlagValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserSkill(t, home)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"git-helper", "--from", "claude", "--scope", "global"}, `invalid --scope "global"`},
		{[]string{"git-helper"}, "--from is required"},
		{[]string{"git-helper", "--from", "notepad"}, `unknown --from provider "notepad"`},
		{[]string{"git-helper", "--from", "claude", "--to", "notepad"}, `unknown --to provider "notepad"`},
		{[]string{"missing-skill", "--from", "claude", "--to", "codex"}, "missing-skill"},
		{[]string{"git-helper", "--from", "claude"}, "no other providers detected"},
	}
	for _, tc := range cases {
		_, err := runCommand(t, SkillMigrateCmd(), tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}

func writeLicensedSkill(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "skills", "reviewer")
	writeSkillFiles(t, dir, map[string][]byte{"SKILL.md": []byte("---\nname: reviewer\ndescription: reviews\nlicense: MIT\n---\nReview.\n")})
}

func TestSkillMigrate_DryRunReportsLossAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLicensedSkill(t, home)

	out, err := runCommand(t, SkillMigrateCmd(), "reviewer", "--from", "claude", "--to", "codex", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "== claude -> codex (user scope) ==") || !strings.Contains(out, "license") || !strings.Contains(out, "[dry-run] no files written") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "reviewer")); !os.IsNotExist(err) {
		t.Error("dry-run wrote the target")
	}
}

func TestSkillMigrate_SecurityLossIsRefusedEvenInDryRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserSkill(t, home)

	_, err := runCommand(t, SkillMigrateCmd(), "git-helper", "--from", "claude", "--to", "codex", "--dry-run", "--accept-loss")
	if err == nil || !strings.Contains(err.Error(), "not overridable") {
		t.Fatalf("got %v", err)
	}
}

func TestSkillMigrate_StrictAbortsOnDroppedField(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLicensedSkill(t, home)

	_, err := runCommand(t, SkillMigrateCmd(), "reviewer", "--from", "claude", "--to", "codex", "--strict")
	if err == nil || !strings.Contains(err.Error(), "aborting (--strict)") {
		t.Fatalf("got %v", err)
	}
}

func TestSkillMigrate_DefaultsToEveryOtherDetectedProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "skills", "plain")
	writeSkillFiles(t, dir, map[string][]byte{"SKILL.md": []byte("---\nname: plain\ndescription: A plain skill.\n---\nDo it.\n")})
	if err := os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runCommand(t, SkillMigrateCmd(), "plain", "--from", "claude")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "claude -> codex") || !strings.Contains(out, "written:") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if strings.Contains(out, "claude -> claude") {
		t.Error("the source provider must never be a default target")
	}
	got, err := os.ReadFile(filepath.Join(home, ".agents", "skills", "plain", "SKILL.md"))
	if err != nil || !strings.Contains(string(got), "Do it.") {
		t.Fatalf("migrated skill = %q (err %v)", got, err)
	}
}

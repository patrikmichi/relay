package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const validSkillOverride = `
id: %s
dirs:
  user:
    - { path: "~/.x/skills", role: own }
  project:
    - { path: ".x/skills", role: own }
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
`

func writeOverride(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func diagnosticsByPath(d []OverrideDiagnostic) map[string]string {
	out := map[string]string{}
	for _, x := range d {
		out[x.Path] = x.Status
	}
	return out
}

func TestOverrideDiagnostics_ReportsEveryOverrideOutcome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	skills := filepath.Join(home, ".config", "relay", "providers")
	want := map[string]string{
		writeOverride(t, skills, "a-custom.yml", fmt.Sprintf(validSkillOverride, "custom")): "loaded",
		writeOverride(t, skills, "b-dup.yml", fmt.Sprintf(validSkillOverride, "custom")):    "duplicate provider id",
		writeOverride(t, skills, "c-noid.yml", "dirs: {}\n"):                                "missing provider id",
		writeOverride(t, skills, "d-badyaml.yml", "id: [unclosed\n"):                        "unreadable or malformed override",
		writeOverride(t, skills, "e-unknown-base.yml", "id: z\nextends: nonexistent\n"):     "unknown base provider",
		writeOverride(t, skills, "f-invalid.yml", "id: broken\ndirs: {}\n"):                 "invalid provider override",
		writeOverride(t, skills, "g-extends.yml", "id: claude\nextends: claude\n"):          "loaded",
		writeOverride(t, skills, "h-badextend.yml", "id: codex\nextends: codex\ndirs: 7\n"): "unreadable or malformed override",
	}
	writeOverride(t, skills, "ignored.txt", "not yaml")
	if err := os.Mkdir(filepath.Join(skills, "sub.yml"), 0o755); err != nil {
		t.Fatal(err)
	}

	agents := filepath.Join(home, ".config", "relay", "agents")
	want[writeOverride(t, agents, "noid.yml", "format: markdown\n")] = "missing provider id"

	got := diagnosticsByPath(OverrideDiagnostics())
	for path, status := range want {
		if got[path] != status {
			t.Errorf("%s: status %q, want %q", filepath.Base(path), got[path], status)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d diagnostics, want %d: %v", len(got), len(want), got)
	}
}

func TestOverrideDiagnostics_ProjectTierGatedByFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	writeOverride(t, filepath.Join(".relay", "providers"), "p.yml", fmt.Sprintf(validSkillOverride, "projprov"))
	writeOverride(t, filepath.Join(".relay", "agents"), "readme.md", "ignored")

	got := diagnosticsByPath(OverrideDiagnostics())
	path := filepath.Join(".relay", "providers", "p.yml")
	if got[path] != "project overrides disabled" || len(got) != 1 {
		t.Fatalf("diagnostics = %v, want only %s reported as disabled", got, path)
	}

	AllowProjectProviderOverrides = true
	t.Cleanup(func() { AllowProjectProviderOverrides = false })
	got = diagnosticsByPath(OverrideDiagnostics())
	if got[path] != "loaded" {
		t.Fatalf("with project overrides enabled, %s = %q, want loaded", path, got[path])
	}
}

func TestApplyOverrideTier_UnreadableDirectoryAndFile(t *testing.T) {
	dir := t.TempDir()
	unreadable := writeOverride(t, dir, "x.yml", fmt.Sprintf(validSkillOverride, "x"))
	skipWithoutPermissionChecks(t)
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}

	hooks, codecs := registeredHookNames(), registeredCodecNames()
	configs := map[string]ProviderConfig{}
	var reports []OverrideDiagnostic
	report := func(p, s string) { reports = append(reports, OverrideDiagnostic{p, s}) }

	applyOverrideTier(configs, dir, hooks, codecs, KindSkill, report)
	if len(reports) != 1 || reports[0].Status != "unreadable or malformed override" {
		t.Fatalf("unreadable file reports = %v", reports)
	}
	if _, ok := configs["x"]; ok {
		t.Fatal("unreadable override was applied")
	}

	reports = nil
	skipWithoutPermissionChecks(t)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	applyOverrideTier(configs, dir, hooks, codecs, KindSkill, report)
	if len(reports) != 1 || reports[0].Status != "override directory unreadable" {
		t.Fatalf("unreadable dir reports = %v", reports)
	}

	reports = nil
	applyOverrideTier(configs, filepath.Join(t.TempDir(), "missing"), hooks, codecs, KindSkill, report)
	if len(reports) != 0 {
		t.Fatalf("missing dir should be silent, got %v", reports)
	}
}

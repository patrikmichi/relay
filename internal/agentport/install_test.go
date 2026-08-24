package agentport

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadGenericSkill_FromDirectory(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "my-local-skill")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte(`---
name: my-local-skill
description: a hand-authored skill in an arbitrary directory
allowed-tools:
  - Bash(git diff *)
paths:
  - "**/*.go"
metadata:
  author: acme
---

body text
`),
		"scripts/run.sh": []byte("#!/bin/sh\necho hi\n"),
	})

	s, err := LoadGenericSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadGenericSkill: %v", err)
	}
	if s.Name != "my-local-skill" {
		t.Errorf("Name = %q, want my-local-skill", s.Name)
	}
	if s.Description != "a hand-authored skill in an arbitrary directory" {
		t.Errorf("Description = %q", s.Description)
	}
	if !reflect.DeepEqual(s.AllowedTools, []string{"Bash(git diff *)"}) {
		t.Errorf("AllowedTools = %#v", s.AllowedTools)
	}
	if !reflect.DeepEqual(s.Paths, []string{"**/*.go"}) {
		t.Errorf("Paths = %#v", s.Paths)
	}
	if !reflect.DeepEqual(s.Metadata, map[string]string{"author": "acme"}) {
		t.Errorf("Metadata = %#v", s.Metadata)
	}
	if _, ok := s.Resources["scripts/run.sh"]; !ok {
		t.Errorf("Resources missing scripts/run.sh: %#v", s.Resources)
	}
	if s.Provenance.SourceProvider != "" {
		t.Errorf("SourceProvider = %q, want empty (arbitrary local source)", s.Provenance.SourceProvider)
	}
}

func TestLoadGenericSkill_FromSkillMdPath(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "another-skill")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: another-skill\ndescription: d\n---\n\nbody\n"),
	})

	s, err := LoadGenericSkill(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("LoadGenericSkill(path to SKILL.md): %v", err)
	}
	if s.Name != "another-skill" {
		t.Errorf("Name = %q, want another-skill", s.Name)
	}
}

func TestLoadGenericSkill_NameInferredFromDirWhenFrontmatterOmitsIt(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "inferred-name")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\ndescription: no name field here\n---\n\nbody\n"),
	})

	s, err := LoadGenericSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadGenericSkill: %v", err)
	}
	if s.Name != "inferred-name" {
		t.Errorf("Name = %q, want inferred-name (from directory)", s.Name)
	}
}

func TestLoadGenericSkill_InvalidDirNameErrors(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "Not_A_Valid_Name")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\ndescription: no name field\n---\n\nbody\n"),
	})

	if _, err := LoadGenericSkill(skillDir); err == nil {
		t.Fatalf("expected an error for an invalid inferred name")
	}
}

// TestLoadGenericSkill_RoundTripsToTarget is the "install-from-path
// round-trips to a target" test: a generic local skill, migrated to a real
// provider (Claude), should preserve every field Claude's format supports.
func TestLoadGenericSkill_RoundTripsToTarget(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "install-me")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte(`---
name: install-me
description: installed from an arbitrary local directory
license: MIT
allowed-tools:
  - Bash(ls *)
disable-model-invocation: true
---

Do the thing.
`),
	})

	src, err := LoadGenericSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadGenericSkill: %v", err)
	}

	plan, err := Migrate(src, NewClaudeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Claude's format supports name/description/license/allowed-tools/
	// disable-model-invocation — nothing here should be dropped.
	if len(plan.Loss) != 0 {
		t.Fatalf("Loss = %#v, want none", plan.Loss)
	}

	dest := filepath.Join(t.TempDir(), src.Name)
	writeFiles(t, dest, plan.Files)

	got, err := NewClaudeAdapter().Load(dest)
	if err != nil {
		t.Fatalf("re-Load projected claude skill: %v", err)
	}
	sameCommon(t, got, src)
	if got.License != "MIT" {
		t.Errorf("License = %q, want MIT", got.License)
	}
	if got.DisableModelInvocation == nil || !*got.DisableModelInvocation {
		t.Errorf("DisableModelInvocation = %v, want pointer to true", got.DisableModelInvocation)
	}
}

func TestLoadGenericSkill_ParsesCodexSidecarIfPresent(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "with-sidecar")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: with-sidecar\ndescription: d\n---\n\nbody\n"),
		"agents/openai.yaml": []byte(`interface:
  display_name: With Sidecar
policy:
  allow_implicit_invocation: true
dependencies:
  tools:
    - some-mcp
`),
	})

	s, err := LoadGenericSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadGenericSkill: %v", err)
	}
	if s.CodexInterface == nil || s.CodexInterface.DisplayName != "With Sidecar" {
		t.Fatalf("CodexInterface = %#v, want DisplayName=With Sidecar", s.CodexInterface)
	}
	if s.AllowImplicitInvocation == nil || !*s.AllowImplicitInvocation {
		t.Fatalf("AllowImplicitInvocation = %v, want pointer to true", s.AllowImplicitInvocation)
	}
	if s.CodexTools == nil || !reflect.DeepEqual(s.CodexTools.MCPServers, []string{"some-mcp"}) {
		t.Fatalf("CodexTools = %#v", s.CodexTools)
	}
	if _, ok := s.Resources["agents/openai.yaml"]; ok {
		t.Fatalf("sidecar should be excluded from Resources: %#v", s.Resources)
	}
}

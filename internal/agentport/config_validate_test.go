package agentport

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestParseProviderConfig_ValidationErrors exercises config.go's validate()
// error branches — every failure must be a clear, field-scoped error, never
// a panic (§4.4).
func TestParseProviderConfig_ValidationErrors(t *testing.T) {
	hooks := registeredHookNames()
	codecs := registeredCodecNames()

	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			"missing id",
			`
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"missing required field \"id\"",
		},
		{
			"bad discovery",
			`
id: x
discovery: sideways
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"discovery:",
		},
		{
			"bad name_regex",
			`
id: x
name_regex: "(["
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"name_regex:",
		},
		{
			"missing user dirs",
			`
id: x
dirs:
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"dirs.user:",
		},
		{
			"user dirs[0] not own",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: compat }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"dirs.user[0].role:",
		},
		{
			"missing project dirs",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"dirs.project:",
		},
		{
			"project dirs[0] not own",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: admin }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"dirs.project[0].role:",
		},
		{
			"empty dir path",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }, { path: "", role: compat }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"dirs.user[1].path:",
		},
		{
			"invalid dir role",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }, { path: "~/.y/skills", role: bogus }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"dirs.user[1].role: invalid",
		},
		{
			"bare tilde user dir",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }, { path: "~", role: compat }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			`dirs.user[1].path: must start with "~/"`,
		},
		{
			"dot-dot escape after tilde",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }, { path: "~/../etc", role: compat }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			`dirs.user[1].path: must not contain ".."`,
		},
		{
			"absolute non-admin user dir",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }, { path: "/etc/passwd", role: compat }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			`dirs.user[1].path: must start with "~/"`,
		},
		{
			"absolute project dir",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }, { path: "/etc", role: compat }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"dirs.project[1].path: must be relative",
		},
		{
			"dot-dot escape in project dir",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }, { path: "../../etc", role: compat }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			`dirs.project[1].path: must not contain ".."`,
		},
		{
			"frontmatter type mismatch vs canonical ir type",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: metadata, key: metadata, type: string, presence: optional }]
`,
			`frontmatter[0].type: "string" does not match canonical type "map" for ir "metadata"`,
		},
		{
			"no frontmatter fields",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: []
`,
			"frontmatter: must declare",
		},
		{
			"empty ir",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: "", key: name, type: string, presence: required }]
`,
			"frontmatter[0].ir:",
		},
		{
			"unknown ir",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: not_a_real_field, key: name, type: string, presence: required }]
`,
			"unknown canonical IR field",
		},
		{
			"empty key",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: "", type: string, presence: required }]
`,
			"frontmatter[0].key:",
		},
		{
			"duplicate key",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
  - { ir: description, key: name, type: string, presence: required }
`,
			"duplicate key",
		},
		{
			"invalid type",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: number, presence: required }]
`,
			"frontmatter[0].type:",
		},
		{
			"invalid presence",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: sometimes }]
`,
			"frontmatter[0].presence:",
		},
		{
			"sidecar missing path",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
sidecar: { codec: codex-openai }
`,
			"sidecar.path:",
		},
		{
			"sidecar missing codec",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
sidecar: { path: "agents/x.yaml" }
`,
			"sidecar.codec:",
		},
		{
			"sidecar unknown codec",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
sidecar: { path: "agents/x.yaml", codec: not-a-real-codec }
`,
			"unknown codec",
		},
		{
			"unknown capability",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
capabilities: [NotARealCapability]
`,
			"unknown capability",
		},
		{
			"bad layout",
			`
id: x
layout: sideways
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`,
			"layout:",
		},
		{
			"unknown load hook",
			`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
load_hooks: [not-a-real-hook]
`,
			"unknown hook",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseProviderConfig([]byte(c.yaml), hooks, codecs)
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestParseProviderConfig_AdminAbsoluteDirAccepted confirms an absolute
// path is accepted for role admin — the documented read-only org-scope
// case (e.g. Codex's /etc/codex/skills) — even though the same absolute
// path is rejected for own/compat roles.
func TestParseProviderConfig_AdminAbsoluteDirAccepted(t *testing.T) {
	raw := []byte(`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }, { path: "/etc/x/skills", role: admin }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`)
	if _, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames()); err != nil {
		t.Fatalf("parseProviderConfig: expected admin absolute dir to be accepted, got: %v", err)
	}
}

// TestParseProviderConfig_CustomNameRegex exercises the (currently unused
// by any shipped/new provider, but supported) custom name_regex field, and
// configAdapter.validateName's non-default branch.
func TestParseProviderConfig_CustomNameRegex(t *testing.T) {
	raw := []byte(`
id: strict-upper
name_regex: "^[A-Z]+$"
dirs:
  user: [{ path: "~/.strict/skills", role: own }]
  project: [{ path: ".strict/skills", role: own }]
frontmatter:
  - { ir: name, key: name, type: string, presence: required }
  - { ir: description, key: description, type: string, presence: required }
`)
	cfg, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames())
	if err != nil {
		t.Fatalf("parseProviderConfig: %v", err)
	}

	a := newConfigAdapter(*cfg)

	skillDir := filepath.Join(t.TempDir(), "ABC")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: ABC\ndescription: d\n---\n\nbody\n"),
	})
	if _, err := a.Load(skillDir); err != nil {
		t.Fatalf("Load with custom name_regex accepting uppercase: %v", err)
	}

	badDir := filepath.Join(t.TempDir(), "abc")
	writeFiles(t, badDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: abc\ndescription: d\n---\n\nbody\n"),
	})
	if _, err := a.Load(badDir); err == nil {
		t.Fatalf("Load with custom name_regex: expected an error for lowercase name")
	}

	if err := a.validateName(""); err == nil {
		t.Fatalf("validateName(\"\"): expected an error for empty name under a custom regex")
	}
}

// TestParseProviderConfig_LayoutDefaultsToDir confirms an omitted `layout`
// defaults to "dir" — every existing provider config predates this field
// and must be unaffected (§P1.1: "no behavior change for existing dir
// providers").
func TestParseProviderConfig_LayoutDefaultsToDir(t *testing.T) {
	raw := []byte(`
id: x
dirs:
  user: [{ path: "~/.x/skills", role: own }]
  project: [{ path: ".x/skills", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`)
	cfg, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames())
	if err != nil {
		t.Fatalf("parseProviderConfig: %v", err)
	}
	if cfg.Layout != LayoutDir {
		t.Fatalf("Layout = %q, want %q (default)", cfg.Layout, LayoutDir)
	}
}

// TestParseProviderConfig_LayoutFlatAccepted confirms an explicit
// `layout: flat` parses and validates cleanly.
func TestParseProviderConfig_LayoutFlatAccepted(t *testing.T) {
	raw := []byte(`
id: x
layout: flat
dirs:
  user: [{ path: "~/.x/agents", role: own }]
  project: [{ path: ".x/agents", role: own }]
frontmatter: [{ ir: name, key: name, type: string, presence: required }]
`)
	cfg, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames())
	if err != nil {
		t.Fatalf("parseProviderConfig: %v", err)
	}
	if cfg.Layout != LayoutFlat {
		t.Fatalf("Layout = %q, want %q", cfg.Layout, LayoutFlat)
	}
}

// TestConfigAdapter_LayoutFlat_LoadsFlatFileDirectly exercises the
// first-class layout: flat Load path (config_adapter.go's loadFlatFile):
// skillDir is the flat file itself (no directory, no resources), name
// falls back to the filename when frontmatter omits it, and an explicit
// frontmatter name need not match the filename (mirrors the
// claude-legacy-commands hook's behavior exactly, but as a first-class,
// non-hook-gated layout).
func TestConfigAdapter_LayoutFlat_LoadsFlatFileDirectly(t *testing.T) {
	raw := []byte(`
id: flat-test
layout: flat
dirs:
  user: [{ path: "~/.flat-test/agents", role: own }]
  project: [{ path: ".flat-test/agents", role: own }]
frontmatter:
  - { ir: name, key: name, type: string, presence: optional }
  - { ir: description, key: description, type: string, presence: required }
`)
	cfg, err := parseProviderConfig(raw, registeredHookNames(), registeredCodecNames())
	if err != nil {
		t.Fatalf("parseProviderConfig: %v", err)
	}
	a := newConfigAdapter(*cfg)

	dir := t.TempDir()
	flatFile := filepath.Join(dir, "my-agent.md")
	writeFiles(t, dir, map[string][]byte{
		"my-agent.md": []byte("---\ndescription: does a thing\n---\n\nSystem prompt body.\n"),
	})

	s, err := a.Load(flatFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Name != "my-agent" {
		t.Fatalf("Name = %q, want %q (inferred from filename)", s.Name, "my-agent")
	}
	if s.Description != "does a thing" {
		t.Fatalf("Description = %q", s.Description)
	}
	if strings.TrimSpace(s.Body) != "System prompt body." {
		t.Fatalf("Body = %q", s.Body)
	}
	if len(s.Resources) != 0 {
		t.Fatalf("Resources = %#v, want none (flat layout has no resources)", s.Resources)
	}

	// An explicit frontmatter name need not match the filename — there's no
	// containing directory to compare against.
	writeFiles(t, dir, map[string][]byte{
		"other-file.md": []byte("---\nname: totally-different\ndescription: d\n---\n\nbody\n"),
	})
	s2, err := a.Load(filepath.Join(dir, "other-file.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s2.Name != "totally-different" {
		t.Fatalf("Name = %q, want %q (explicit frontmatter name wins)", s2.Name, "totally-different")
	}
}

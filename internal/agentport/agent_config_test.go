package agentport

import "testing"

// TestEmbeddedAgentConfigsValid asserts every embedded agents/*.yml parses
// and validates cleanly — the Agent-IR analogue of TestEmbeddedConfigsValid
// (§P1.3 exit bar).
func TestEmbeddedAgentConfigsValid(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("embedded agent provider configs are invalid: %v", r)
		}
	}()

	configs := mustParseEmbeddedAgentConfigs(registeredHookNames(), registeredCodecNames())

	wantIDs := []string{"claude", "opencode"}
	for _, id := range wantIDs {
		cfg, ok := configs[id]
		if !ok {
			t.Errorf("embedded agent provider %q missing", id)
			continue
		}
		if cfg.ID != id {
			t.Errorf("provider %q: cfg.ID = %q", id, cfg.ID)
		}
		if cfg.Layout != LayoutFlat {
			t.Errorf("provider %q: Layout = %q, want %q (agents have no resource-bearing directory)", id, cfg.Layout, LayoutFlat)
		}
		if len(cfg.Dirs.User) == 0 || cfg.Dirs.User[0].Role != DirRoleOwn {
			t.Errorf("provider %q: dirs.user[0] must be role=own", id)
		}
		if len(cfg.Dirs.Project) == 0 || cfg.Dirs.Project[0].Role != DirRoleOwn {
			t.Errorf("provider %q: dirs.project[0] must be role=own", id)
		}
		for i, f := range cfg.Frontmatter {
			if _, ok := agentIrFieldDescriptors[f.IR]; !ok {
				t.Errorf("provider %q: frontmatter[%d].ir = %q is not a registered Agent IR field", id, i, f.IR)
			}
		}
	}

	// Agent and skill provider configs are loaded from separate embed
	// dirs and must never collide: "claude"/"opencode" exist in both sets,
	// but as distinct ProviderConfig instances (different Layout/dirs/
	// frontmatter targeting the Agent IR, not the Skill IR).
	skillConfigs := mustParseEmbeddedConfigs(registeredHookNames(), registeredCodecNames())
	if skillConfigs["claude"].Layout == LayoutFlat {
		t.Errorf("skill claude.yml must remain layout: dir (unaffected by the new agent config)")
	}
}

// TestParseAgentProviderConfig_ClaudeShape exercises the claude agent
// config's declared shape against the real, verified format
// (claude-infra/shared-claude/agents/*.md): name, description, tools (CSV),
// model, memory, skills (list) — all mapped to their fixed Agent IR names
// with matching canonical types.
func TestParseAgentProviderConfig_ClaudeShape(t *testing.T) {
	configs := mustParseEmbeddedAgentConfigs(registeredHookNames(), registeredCodecNames())
	cfg, ok := configs["claude"]
	if !ok {
		t.Fatalf("embedded claude agent config missing")
	}

	wantIRs := map[string]FieldType{
		"name":        FieldString,
		"description": FieldString,
		"tools":       FieldStringOrList,
		"model":       FieldString,
		"memory":      FieldString,
		"skills":      FieldStringOrList,
	}
	got := map[string]FieldType{}
	for _, f := range cfg.Frontmatter {
		got[f.IR] = f.Type
	}
	for ir, typ := range wantIRs {
		gotTyp, present := got[ir]
		if !present {
			t.Errorf("claude agent frontmatter missing ir %q", ir)
			continue
		}
		if gotTyp != typ {
			t.Errorf("claude agent frontmatter[%q].type = %q, want %q", ir, gotTyp, typ)
		}
	}

	if len(cfg.Dirs.User) != 1 || cfg.Dirs.User[0].Path != "~/.claude/agents" {
		t.Errorf("claude agent dirs.user = %#v, want a single ~/.claude/agents entry", cfg.Dirs.User)
	}
	if len(cfg.Dirs.Project) != 1 || cfg.Dirs.Project[0].Path != ".claude/agents" {
		t.Errorf("claude agent dirs.project = %#v, want a single .claude/agents entry", cfg.Dirs.Project)
	}
}

// TestParseAgentProviderConfig_OpencodeShape exercises the opencode agent
// config's declared shape: description/mode/model/temperature/tools, and
// confirms "name" is deliberately NOT mapped (no on-disk name field —
// inferred from the filename by the layout: flat Load path). tools IS
// mapped (declared type string-or-list, matching the canonical Agent IR
// descriptor) even though its on-disk shape is a {tool: bool} map — the
// actual map<->list reshape is bespoke Go logic in
// agentConfigAdapter.Load/Project (agent_config_adapter.go), not something
// this generic shape check can observe.
func TestParseAgentProviderConfig_OpencodeShape(t *testing.T) {
	configs := mustParseEmbeddedAgentConfigs(registeredHookNames(), registeredCodecNames())
	cfg, ok := configs["opencode"]
	if !ok {
		t.Fatalf("embedded opencode agent config missing")
	}

	wantIRs := map[string]FieldType{
		"description": FieldString,
		"mode":        FieldString,
		"model":       FieldString,
		"temperature": FieldFloat,
		"tools":       FieldStringOrList,
	}
	got := map[string]FieldType{}
	for _, f := range cfg.Frontmatter {
		got[f.IR] = f.Type
	}
	for ir, typ := range wantIRs {
		gotTyp, present := got[ir]
		if !present {
			t.Errorf("opencode agent frontmatter missing ir %q", ir)
			continue
		}
		if gotTyp != typ {
			t.Errorf("opencode agent frontmatter[%q].type = %q, want %q", ir, gotTyp, typ)
		}
	}
	if _, present := got["name"]; present {
		t.Errorf("opencode agent config must not map ir: name (no name field on disk; inferred from filename)")
	}
}

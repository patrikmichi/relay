package agentport

import (
	"reflect"
	"testing"
)

func TestAgentFieldDiff(t *testing.T) {
	half, one := 0.5, 1.0
	base := Agent{
		Name: "a", Description: "d", Body: "b", Model: "sonnet",
		Tools: []string{"Read"}, Temperature: &half, Mode: "primary",
		Memory: "project", Skills: []string{"s"}, Metadata: map[string]string{"k": "v"},
	}
	same := base
	sameTemp := 0.5
	same.Temperature = &sameTemp
	if got := AgentFieldDiff(&base, &same); len(got) != 0 {
		t.Fatalf("equal agents (distinct but equal pointers) diff = %v", got)
	}

	other := Agent{
		Name: "b", Description: "e", Body: "c", Model: "opus",
		Tools: []string{"Bash"}, Temperature: &one, Mode: "subagent",
		Memory: "global", Skills: []string{"t"}, Metadata: map[string]string{"k": "w"},
	}
	want := []string{
		"Name: a -> b",
		"Description: d -> e",
		"Body: changed",
		"Model: sonnet -> opus",
		"Tools: [Read] -> [Bash]",
		"Temperature: 0.5 -> 1",
		"Mode: primary -> subagent",
		"Memory: project -> global",
		"Skills: [s] -> [t]",
		"Metadata: map[k:v] -> map[k:w]",
	}
	if got := AgentFieldDiff(&base, &other); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff =\n%q\nwant\n%q", got, want)
	}

	unset := base
	unset.Temperature = nil
	if got := AgentFieldDiff(&base, &unset); !reflect.DeepEqual(got, []string{"Temperature: 0.5 -> <nil>"}) {
		t.Fatalf("unset temperature diff = %q", got)
	}
}

func TestFieldDiff_ProviderExtensionFields(t *testing.T) {
	yes, no := true, false
	src := &Skill{
		Name: "s", License: "MIT", AllowedTools: []string{"Read"}, Paths: []string{"a/**"},
		DisableModelInvocation: &yes, AllowImplicitInvocation: &yes,
		CodexInterface: &CodexInterface{DisplayName: "S"},
		CodexTools:     &CodexTools{Tools: []CodexToolDependency{{Type: "mcp", Value: "x"}}},
		Compatibility:  "claude", Metadata: map[string]string{"k": "v"},
	}
	other := &Skill{
		Name: "s", License: "Apache-2.0", AllowedTools: []string{"Bash"}, Paths: nil,
		DisableModelInvocation: &no, AllowImplicitInvocation: nil,
		Compatibility: "codex",
	}
	got := FieldDiff(src, other)
	wantFields := []string{"License", "AllowedTools", "Paths", "DisableModelInvocation", "AllowImplicitInvocation", "CodexInterface", "CodexTools", "Compatibility", "Metadata"}
	if len(got) != len(wantFields) {
		t.Fatalf("diff = %q, want one line per field %v", got, wantFields)
	}
	for i, f := range wantFields {
		if len(got[i]) < len(f) || got[i][:len(f)] != f {
			t.Errorf("line %d = %q, want field %s", i, got[i], f)
		}
	}
	if got[4] != "AllowImplicitInvocation: true -> <nil>" {
		t.Errorf("nil bool rendering = %q", got[4])
	}
}

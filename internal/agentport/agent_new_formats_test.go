package agentport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestAdditionalAgentFormatsRoundTrip(t *testing.T) {
	for _, id := range []ProviderID{ProviderCodex, ProviderCursor, "gemini-cli"} {
		t.Run(string(id), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())
			adapter, ok := AgentAdapterByID(id)
			if !ok {
				t.Fatal("missing adapter")
			}
			original := &Agent{Name: "reviewer", Description: "Quotes \" and backslashes \\ and Unicode 🦉", Body: "First\n```\nname = \"not-a-field\"\n```\ntriple: \"\"\" and '''\n", Provenance: Provenance{SourceProvider: id}}
			plan, err := MigrateAgent(original, adapter, ScopeUser)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteAgent(plan); err != nil {
				t.Fatal(err)
			}
			path, err := ResolveAgentPath(adapter, ScopeUser, "reviewer")
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := adapter.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Name != original.Name || loaded.Description != original.Description || loaded.Body != original.Body {
				t.Fatalf("roundtrip lost content: %+v", loaded)
			}
			refs, err := AgentList(adapter, ScopeUser)
			if err != nil || len(refs) != 1 || refs[0].Path != path {
				t.Fatalf("discovery mismatch: %+v %v", refs, err)
			}
			if id == ProviderCodex {
				raw, _ := os.ReadFile(path)
				var fields map[string]any
				if err := toml.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if fields["developer_instructions"] != original.Body {
					t.Fatal("incorrect TOML body")
				}
			}
		})
	}
}

func TestCodexNestedAndDuplicateSettingsNeverDisappear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	adapter, _ := AgentAdapterByID(ProviderCodex)
	for _, extra := range []string{"\n[sandbox_workspace_write]\nnetwork_access = false\n", "\nsandbox_workspace_write.network_access = false\n", "\nsandbox_mode = 'read-only'\n"} {
		path := filepath.Join(t.TempDir(), "reviewer.toml")
		if err := os.WriteFile(path, []byte("name = 'reviewer'\ndescription = 'Review'\ndeveloper_instructions = 'Instructions'\n"+extra), 0600); err != nil {
			t.Fatal(err)
		}
		agent, err := adapter.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(agent.UnmappedSecurityFields) == 0 {
			t.Fatal("execution settings silently discarded")
		}
		plan, err := MigrateAgent(agent, NewClaudeAgentAdapter(), ScopeUser)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, loss := range plan.Loss {
			found = found || loss.Security
		}
		if !found {
			t.Fatal("missing refusal evidence")
		}
	}
	path := filepath.Join(t.TempDir(), "duplicate.toml")
	if err := os.WriteFile(path, []byte("name='a'\nname='b'\ndeveloper_instructions='body'"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Load(path); err == nil {
		t.Fatal("duplicate TOML keys accepted")
	}
}

func TestNewAgentFormatsRejectRestrictionLoss(t *testing.T) {
	for _, tc := range []struct {
		id    ProviderID
		extra string
	}{{ProviderCursor, "readonly: true"}, {"gemini-cli", "mcpServers:\n  private:\n    command: secret"}, {"gemini-cli", "tools: []"}} {
		t.Run(string(tc.id)+tc.extra[:4], func(t *testing.T) {
			adapter, _ := AgentAdapterByID(tc.id)
			path := filepath.Join(t.TempDir(), "reviewer.md")
			if err := os.WriteFile(path, []byte("---\nname: reviewer\ndescription: Review\n"+tc.extra+"\n---\nBody\n"), 0600); err != nil {
				t.Fatal(err)
			}
			agent, err := adapter.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			_, loss, err := NewClaudeAgentAdapter().Project(agent)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range loss {
				found = found || item.Security
			}
			if !found {
				t.Fatal("security field loss not marked")
			}
		})
	}
	for _, id := range []ProviderID{ProviderCodex, ProviderCursor, "gemini-cli"} {
		adapter, _ := AgentAdapterByID(id)
		files, loss, err := adapter.Project(&Agent{Name: "reviewer", Description: "Review", Model: "sonnet", Tools: []string{"Read"}, Provenance: Provenance{SourceProvider: ProviderClaude}})
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range files {
			if strings.Contains(string(body), "sonnet") {
				t.Fatal("foreign model leaked")
			}
		}
		found := false
		for _, item := range loss {
			found = found || item.Security
		}
		if !found {
			t.Fatalf("restriction lost for %s", id)
		}
	}
}

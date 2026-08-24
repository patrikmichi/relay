package agentport

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlNodeFor is a small test helper: parses a YAML scalar/collection
// literal into a *yaml.Node, mirroring how config_adapter.go's
// mappingLookup hands a value node to decodeAgentIRField.
func yamlNodeFor(t *testing.T, raw string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("yaml.Unmarshal(%q): %v", raw, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		t.Fatalf("expected a document node wrapping one value, got %#v", doc)
	}
	return doc.Content[0]
}

// TestAgent_RoundTripGetSet exercises decodeAgentIRField -> agentFieldValue
// for every declared agentIrFieldDescriptors name — the Agent-IR analogue
// of the Skill IR's get/set round trip, proving the two functions agree on
// every field's shape (§P1.2).
func TestAgent_RoundTripGetSet(t *testing.T) {
	cases := []struct {
		ir       string
		raw      string
		wantZero bool
		check    func(t *testing.T, a *Agent)
	}{
		{"name", "my-agent", false, func(t *testing.T, a *Agent) {
			if a.Name != "my-agent" {
				t.Errorf("Name = %q", a.Name)
			}
		}},
		{"description", "does a thing", false, func(t *testing.T, a *Agent) {
			if a.Description != "does a thing" {
				t.Errorf("Description = %q", a.Description)
			}
		}},
		{"metadata", "team: platform\nowner: acme\n", false, func(t *testing.T, a *Agent) {
			if a.Metadata["team"] != "platform" || a.Metadata["owner"] != "acme" {
				t.Errorf("Metadata = %#v", a.Metadata)
			}
		}},
		{"model", "sonnet", false, func(t *testing.T, a *Agent) {
			if a.Model != "sonnet" {
				t.Errorf("Model = %q", a.Model)
			}
		}},
		{"tools", "Read, Write, Bash", false, func(t *testing.T, a *Agent) {
			if !reflect.DeepEqual(a.Tools, []string{"Read", "Write", "Bash"}) {
				t.Errorf("Tools = %#v", a.Tools)
			}
		}},
		{"temperature", "0.7", false, func(t *testing.T, a *Agent) {
			if a.Temperature == nil || *a.Temperature != 0.7 {
				t.Errorf("Temperature = %v", a.Temperature)
			}
		}},
		{"mode", "subagent", false, func(t *testing.T, a *Agent) {
			if a.Mode != "subagent" {
				t.Errorf("Mode = %q", a.Mode)
			}
		}},
		{"memory", "project", false, func(t *testing.T, a *Agent) {
			if a.Memory != "project" {
				t.Errorf("Memory = %q", a.Memory)
			}
		}},
		{"skills", "[api-design, error-handling]", false, func(t *testing.T, a *Agent) {
			if !reflect.DeepEqual(a.Skills, []string{"api-design", "error-handling"}) {
				t.Errorf("Skills = %#v", a.Skills)
			}
		}},
	}

	for _, c := range cases {
		t.Run(c.ir, func(t *testing.T) {
			a := &Agent{}
			node := yamlNodeFor(t, c.raw)
			if err := decodeAgentIRField(a, c.ir, node); err != nil {
				t.Fatalf("decodeAgentIRField(%q): %v", c.ir, err)
			}
			c.check(t, a)

			val, isZero := agentFieldValue(a, c.ir)
			if isZero != c.wantZero {
				t.Fatalf("agentFieldValue(%q) isZero = %v, want %v (val=%#v)", c.ir, isZero, c.wantZero, val)
			}
		})
	}
}

// TestAgent_ZeroValueOmission mirrors the Skill IR's omitempty-style
// Project() omission contract: a freshly zero-valued Agent reports every
// declared field as zero, and decodeAgentIRField rejects an unrecognized
// ir name (parity with decodeIRField's default branch).
func TestAgent_ZeroValueOmission(t *testing.T) {
	a := &Agent{}
	for ir := range agentIrFieldDescriptors {
		_, isZero := agentFieldValue(a, ir)
		if !isZero {
			t.Errorf("agentFieldValue(%q) on a zero-value Agent: isZero = false, want true", ir)
		}
	}

	if _, isZero := agentFieldValue(a, "not_a_real_field"); !isZero {
		t.Errorf("agentFieldValue(unknown): isZero = false, want true (unknown names report zero)")
	}

	node := yamlNodeFor(t, "x")
	if err := decodeAgentIRField(a, "not_a_real_field", node); err == nil {
		t.Fatalf("decodeAgentIRField(unknown ir): expected an error, got nil")
	}
}

// TestAgent_NoResourcesField documents the structural simplification over
// Skill: Agent has no Resources field at all (single flat file, no
// resource dir) — enforced at compile time via reflection over the struct
// tags/fields so a future accidental re-addition fails a test, not just a
// design-doc read.
func TestAgent_NoResourcesField(t *testing.T) {
	typ := reflect.TypeOf(Agent{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name == "Resources" {
			t.Fatalf("Agent must not have a Resources field (agents are single flat files)")
		}
	}
}

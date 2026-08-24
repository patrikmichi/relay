package agentport

import "testing"

// TestAgentIrFieldDescriptors_Complete asserts every field agent.go
// declares as part of the Agent IR (per relay-standalone design §3b) has a
// corresponding agentIrFieldDescriptors entry — catches a field added to
// the struct without wiring its name->field binding.
func TestAgentIrFieldDescriptors_Complete(t *testing.T) {
	want := []string{
		"name", "description", "metadata", "model", "tools",
		"temperature", "mode", "memory", "skills",
	}
	for _, ir := range want {
		if _, ok := agentIrFieldDescriptors[ir]; !ok {
			t.Errorf("agentIrFieldDescriptors missing entry for %q", ir)
		}
	}
	if len(agentIrFieldDescriptors) != len(want) {
		t.Errorf("agentIrFieldDescriptors has %d entries, want %d (declared: %v)", len(agentIrFieldDescriptors), len(want), want)
	}
}

// TestCanonicalIRFieldType_UnionsSkillAndAgent proves config.go's
// canonicalIRFieldType (used by ProviderConfig.validate) accepts both the
// Skill IR and the Agent IR by name, without a self-describing "kind"
// field on ProviderConfig — the mechanism that lets agents/*.yml validate
// through the exact same parseProviderConfig used for providers/*.yml.
func TestCanonicalIRFieldType_UnionsSkillAndAgent(t *testing.T) {
	if typ, ok := canonicalIRFieldType("allowed_tools"); !ok || typ != FieldStringOrList {
		t.Errorf("canonicalIRFieldType(allowed_tools) = (%v, %v), want (%v, true)", typ, ok, FieldStringOrList)
	}
	if typ, ok := canonicalIRFieldType("temperature"); !ok || typ != FieldFloat {
		t.Errorf("canonicalIRFieldType(temperature) = (%v, %v), want (%v, true)", typ, ok, FieldFloat)
	}
	// Overlapping names must agree on type across both IRs.
	if typ, ok := canonicalIRFieldType("name"); !ok || typ != FieldString {
		t.Errorf("canonicalIRFieldType(name) = (%v, %v), want (%v, true)", typ, ok, FieldString)
	}
	if _, ok := canonicalIRFieldType("not_a_real_field_in_either_ir"); ok {
		t.Errorf("canonicalIRFieldType(unknown): ok = true, want false")
	}
}

// TestDecodeAgentIRField_PropagatesDecodeErrors confirms a shape mismatch
// (e.g. a mapping node where a scalar is expected) surfaces as an error
// rather than silently zeroing the field, for every scalar-typed Agent IR
// field.
func TestDecodeAgentIRField_PropagatesDecodeErrors(t *testing.T) {
	a := &Agent{}
	node := yamlNodeFor(t, "key: value\n") // mapping node, not a scalar
	for _, ir := range []string{"name", "description", "model", "temperature", "mode", "memory"} {
		if err := decodeAgentIRField(a, ir, node); err == nil {
			t.Errorf("decodeAgentIRField(%s, <mapping>): expected an error, got nil", ir)
		}
	}

	// tools/skills (flexStringList) reject a mapping node via their own
	// UnmarshalYAML default branch.
	for _, ir := range []string{"tools", "skills"} {
		if err := decodeAgentIRField(a, ir, node); err == nil {
			t.Errorf("decodeAgentIRField(%s, <mapping>): expected an error, got nil", ir)
		}
	}
}

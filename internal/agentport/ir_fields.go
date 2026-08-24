package agentport

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// This file is the bounded, fixed binding between a config's declared
// canonical-IR field NAME (a string in providers/<id>.yml, e.g.
// "allowed_tools") and the actual Go field on *Skill. The Skill IR shape
// itself is fixed Go code (skill.go) — only the mapping from a provider's
// on-disk KEY to one of these fixed IR names is data. This keeps the
// generic frontmatter codec simple and reviewable (a switch over ~8 known
// names) rather than reflection-driven, honoring the "not a Turing-complete
// YAML" non-goal.
//
// irFieldDescriptors is also the source of truth config.go's validate()
// checks a frontmatter[].ir value against — an unrecognized ir name is a
// config validation error, not a silent no-op.
var irFieldDescriptors = map[string]FieldType{
	"name":                      FieldString,
	"description":               FieldString,
	"license":                   FieldString,
	"compatibility":             FieldString,
	"metadata":                  FieldMap,
	"allowed_tools":             FieldStringOrList,
	"paths":                     FieldStringOrList,
	"disable_model_invocation":  FieldBool,
	"allow_implicit_invocation": FieldBool, // sidecar-only in the shipped configs; codec-owned, listed here for completeness/validation
	"codex_interface":           FieldMap,  // sidecar-only; codec-owned
	"codex_tools":               FieldList, // sidecar-only; codec-owned
}

// canonicalIRFieldType looks up ir's fixed canonical FieldType across BOTH
// registered IR shapes — the Skill IR (irFieldDescriptors) and the Agent IR
// (agentIrFieldDescriptors, agent_ir_fields.go) — so a single, kind-agnostic
// config.go validate() pass accepts either a skill or an agent provider
// config without needing a self-describing "kind" field on ProviderConfig:
// which IR a config actually targets is determined by which embed
// directory/constructor loads it (providers/*.yml + configAdapter for
// skills, agents/*.yml + agentConfigAdapter for agents), not by this
// lookup. The two descriptor sets' overlapping names ("name",
// "description") already agree on FieldString, so a union lookup is safe:
// Skill IR is checked first (cheap, and preserves the exact error/behavior
// for every pre-existing skill config).
func canonicalIRFieldType(ir string) (FieldType, bool) {
	if t, ok := irFieldDescriptors[ir]; ok {
		return t, true
	}
	if t, ok := agentIrFieldDescriptors[ir]; ok {
		return t, true
	}
	return "", false
}

// irFieldValue returns the current value of ir on s, and whether it is the
// Go zero value for that field — the latter drives the universal
// omitempty-style Project() omission every shipped provider's per-field
// struct tags apply (every field across all 4 hard-coded frontmatter
// structs was tagged ",omitempty" — this reproduces that exactly, byte for
// byte, regardless of the field's declared "presence").
func irFieldValue(s *Skill, ir string) (value interface{}, isZero bool) {
	switch ir {
	case "name":
		return s.Name, s.Name == ""
	case "description":
		return s.Description, s.Description == ""
	case "license":
		return s.License, s.License == ""
	case "compatibility":
		return s.Compatibility, s.Compatibility == ""
	case "metadata":
		return s.Metadata, len(s.Metadata) == 0
	case "allowed_tools":
		return flexStringList(s.AllowedTools), len(s.AllowedTools) == 0
	case "paths":
		return flexStringList(s.Paths), len(s.Paths) == 0
	case "disable_model_invocation":
		return s.DisableModelInvocation, s.DisableModelInvocation == nil
	default:
		return nil, true
	}
}

// decodeIRField parses one frontmatter-level yaml node into the
// corresponding *Skill field. Only handles the flat, non-sidecar IR names —
// sidecar-only names (allow_implicit_invocation, codex_interface,
// codex_tools) are never reached here because they never appear in a
// config's top-level "frontmatter" list (validated indirectly: no shipped
// or new config maps them there; a sidecar codec owns them instead).
func decodeIRField(s *Skill, ir string, node *yaml.Node) error {
	switch ir {
	case "name":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		s.Name = v
	case "description":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		s.Description = v
	case "license":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		s.License = v
	case "compatibility":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		s.Compatibility = v
	case "metadata":
		var v map[string]string
		if err := node.Decode(&v); err != nil {
			return err
		}
		s.Metadata = v
	case "allowed_tools":
		var v flexStringList
		if err := v.UnmarshalYAML(node); err != nil {
			return err
		}
		if len(v) > 0 {
			s.AllowedTools = []string(v)
		}
	case "paths":
		var v flexStringList
		if err := v.UnmarshalYAML(node); err != nil {
			return err
		}
		if len(v) > 0 {
			s.Paths = []string(v)
		}
	case "disable_model_invocation":
		var v bool
		if err := node.Decode(&v); err != nil {
			return err
		}
		s.DisableModelInvocation = &v
	default:
		return fmt.Errorf("unknown canonical IR field %q", ir)
	}
	return nil
}

package agentport

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// This file is the bounded, fixed binding between a config's declared canonical-IR field NAME (a string in providers/<id>.yml, e.g.
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

func canonicalIRFieldType(ir string) (FieldType, bool) {
	if t, ok := irFieldDescriptors[ir]; ok {
		return t, true
	}
	if t, ok := agentIrFieldDescriptors[ir]; ok {
		return t, true
	}
	return "", false
}

func canonicalIRFieldTypeForKind(ir string, kind ArtifactKind) (FieldType, bool) {
	if kind == KindAgent {
		t, ok := agentIrFieldDescriptors[ir]
		return t, ok
	}
	t, ok := irFieldDescriptors[ir]
	return t, ok
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

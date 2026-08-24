package agentport

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// This file is the Agent-IR analogue of ir_fields.go: the bounded, fixed
// binding between a config's declared canonical-IR field NAME (a string in
// agents/<id>.yml, e.g. "temperature") and the actual Go field on *Agent.
// The Agent IR shape itself is fixed Go code (agent.go) — only the mapping
// from a provider's on-disk KEY to one of these fixed IR names is data.
// Kept as a distinct switch/map pair from ir_fields.go (rather than a
// unified, kind-parameterized function) per design §3b Option A: the
// byte-exact Skill path (config_adapter_parity_test.go) must never be put
// at risk by agent-only field additions, and a small, reviewable,
// per-kind switch is the codebase's stated non-goal-compliant alternative
// to reflection (see ir_fields.go's doc comment).
//
// agentIrFieldDescriptors is unioned into config.go's validate() via
// canonicalIRFieldType (ir_fields.go) — an unrecognized ir name (in either
// set) is a config validation error, not a silent no-op.
var agentIrFieldDescriptors = map[string]FieldType{
	"name":        FieldString,
	"description": FieldString,
	"metadata":    FieldMap,
	"model":       FieldString,
	"tools":       FieldStringOrList,
	"temperature": FieldFloat,
	"mode":        FieldString,
	"memory":      FieldString,
	"skills":      FieldStringOrList,
}

// agentFieldValue returns the current value of ir on a, and whether it is
// the Go zero value for that field — the Agent-IR analogue of
// irFieldValue, driving the same omitempty-style Project() omission.
func agentFieldValue(a *Agent, ir string) (value interface{}, isZero bool) {
	switch ir {
	case "name":
		return a.Name, a.Name == ""
	case "description":
		return a.Description, a.Description == ""
	case "metadata":
		return a.Metadata, len(a.Metadata) == 0
	case "model":
		return a.Model, a.Model == ""
	case "tools":
		return flexStringList(a.Tools), len(a.Tools) == 0
	case "temperature":
		if a.Temperature == nil {
			return nil, true
		}
		return *a.Temperature, false
	case "mode":
		return a.Mode, a.Mode == ""
	case "memory":
		return a.Memory, a.Memory == ""
	case "skills":
		return flexStringList(a.Skills), len(a.Skills) == 0
	default:
		return nil, true
	}
}

// decodeAgentIRField parses one frontmatter-level yaml node into the
// corresponding *Agent field — the Agent-IR analogue of decodeIRField.
func decodeAgentIRField(a *Agent, ir string, node *yaml.Node) error {
	switch ir {
	case "name":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		a.Name = v
	case "description":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		a.Description = v
	case "metadata":
		var v map[string]string
		if err := node.Decode(&v); err != nil {
			return err
		}
		a.Metadata = v
	case "model":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		a.Model = v
	case "tools":
		var v flexStringList
		if err := v.UnmarshalYAML(node); err != nil {
			return err
		}
		if len(v) > 0 {
			a.Tools = []string(v)
		}
	case "temperature":
		var v float64
		if err := node.Decode(&v); err != nil {
			return err
		}
		a.Temperature = &v
	case "mode":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		a.Mode = v
	case "memory":
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		a.Memory = v
	case "skills":
		var v flexStringList
		if err := v.UnmarshalYAML(node); err != nil {
			return err
		}
		if len(v) > 0 {
			a.Skills = []string(v)
		}
	default:
		return fmt.Errorf("unknown canonical IR field %q", ir)
	}
	return nil
}

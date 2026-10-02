package agentport

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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

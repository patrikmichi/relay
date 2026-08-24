package agentport

import (
	"fmt"
	"reflect"
)

// AgentFieldDiff compares two Agents field-by-field and returns a
// human-readable line for every field that differs — the Agent-IR analogue
// of FieldDiff. Body is reported as changed/unchanged only (not a full text
// diff), matching FieldDiff's convention.
func AgentFieldDiff(src, other *Agent) []string {
	var diffs []string
	add := func(field string, from, to interface{}) {
		diffs = append(diffs, fmt.Sprintf("%s: %v -> %v", field, from, to))
	}

	if src.Name != other.Name {
		add("Name", src.Name, other.Name)
	}
	if src.Description != other.Description {
		add("Description", src.Description, other.Description)
	}
	if src.Body != other.Body {
		diffs = append(diffs, "Body: changed")
	}
	if src.Model != other.Model {
		add("Model", src.Model, other.Model)
	}
	if !reflect.DeepEqual(src.Tools, other.Tools) {
		add("Tools", src.Tools, other.Tools)
	}
	if !floatPtrEqual(src.Temperature, other.Temperature) {
		add("Temperature", derefFloat(src.Temperature), derefFloat(other.Temperature))
	}
	if src.Mode != other.Mode {
		add("Mode", src.Mode, other.Mode)
	}
	if src.Memory != other.Memory {
		add("Memory", src.Memory, other.Memory)
	}
	if !reflect.DeepEqual(src.Skills, other.Skills) {
		add("Skills", src.Skills, other.Skills)
	}
	if !reflect.DeepEqual(src.Metadata, other.Metadata) {
		add("Metadata", src.Metadata, other.Metadata)
	}

	return diffs
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func derefFloat(f *float64) interface{} {
	if f == nil {
		return nil
	}
	return *f
}

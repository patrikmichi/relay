package agentport

import (
	"fmt"
	"reflect"
	"sort"
)

// FieldDiff compares two Skills field-by-field (the common fields plus the
// typed-optional provider-extension fields) and returns a human-readable
// line for every field that differs. Used by `relay skill diff` to show
// what a migration would change or drop, alongside the LossItem report
// (which only reports fields the TARGET FORMAT can't represent at all —
// FieldDiff also catches anything else that changed in a round trip).
//
// Body is reported as changed/unchanged only (not a full text diff) to
// keep output short. Resources are compared by key set only — byte content
// is expected to round-trip verbatim; the interesting signal is whether a
// resource file was dropped or added, not its bytes.
func FieldDiff(src, other *Skill) []string {
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
	if src.License != other.License {
		add("License", src.License, other.License)
	}
	if !reflect.DeepEqual(src.AllowedTools, other.AllowedTools) {
		add("AllowedTools", src.AllowedTools, other.AllowedTools)
	}
	if !reflect.DeepEqual(src.Paths, other.Paths) {
		add("Paths", src.Paths, other.Paths)
	}
	if !boolPtrEqual(src.DisableModelInvocation, other.DisableModelInvocation) {
		add("DisableModelInvocation", derefBool(src.DisableModelInvocation), derefBool(other.DisableModelInvocation))
	}
	if !boolPtrEqual(src.AllowImplicitInvocation, other.AllowImplicitInvocation) {
		add("AllowImplicitInvocation", derefBool(src.AllowImplicitInvocation), derefBool(other.AllowImplicitInvocation))
	}
	if !reflect.DeepEqual(src.CodexInterface, other.CodexInterface) {
		add("CodexInterface", src.CodexInterface, other.CodexInterface)
	}
	if !reflect.DeepEqual(src.CodexTools, other.CodexTools) {
		add("CodexTools", src.CodexTools, other.CodexTools)
	}
	if src.Compatibility != other.Compatibility {
		add("Compatibility", src.Compatibility, other.Compatibility)
	}
	if !reflect.DeepEqual(src.Metadata, other.Metadata) {
		add("Metadata", src.Metadata, other.Metadata)
	}
	if srcKeys, otherKeys := resourceKeys(src.Resources), resourceKeys(other.Resources); !reflect.DeepEqual(srcKeys, otherKeys) {
		add("Resources", srcKeys, otherKeys)
	}

	return diffs
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func derefBool(b *bool) interface{} {
	if b == nil {
		return nil
	}
	return *b
}

func resourceKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

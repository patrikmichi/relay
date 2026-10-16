package agentport

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// UnmappedField retains one frontmatter/sidecar key a provider's configured
// field set has no binding for, together with a source provider tag
// (non-security half): a same-provider round trip must be able to write the
// value back out, and a cross-provider projection must be able to name it
// in a loss report, rather than either silently vanishing or being
// reinterpreted under a foreign schema. node is retained only for
// same-provider re-encoding and is deliberately unexported — Raw is the
// only cross-package-visible, re-parseable-adjacent representation.
type UnmappedField struct {
	Key      string
	Raw      string
	Provider ProviderID
	node     *yaml.Node
}

// collectUnmappedFields walks root's top-level frontmatter mapping and
// returns an UnmappedField for every key not already claimed by configured
// or skip (both matched case-insensitively). skip lets a caller carve out
// keys it tracks through a separate, more specific mechanism (e.g. agents'
// agentSecurityFrontmatterKeys, tracked via UnmappedSecurityField instead)
// so a key is never captured twice under two different loss mechanisms.
func collectUnmappedFields(root *yaml.Node, configured, skip map[string]bool, provider ProviderID) []UnmappedField {
	mapping := root
	if mapping.Kind == yaml.DocumentNode {
		if len(mapping.Content) == 0 {
			return nil
		}
		mapping = mapping.Content[0]
	}
	if mapping.Kind != yaml.MappingNode {
		return nil
	}

	var out []UnmappedField
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i].Value
		lower := strings.ToLower(key)
		if configured[lower] || skip[lower] {
			continue
		}
		out = append(out, UnmappedField{
			Key:      key,
			Raw:      renderNodeCompact(mapping.Content[i+1]),
			Provider: provider,
			node:     mapping.Content[i+1],
		})
	}
	return out
}

// projectUnmappedFields appends each unmapped field either as a re-encoded
// mapping entry (same-provider round trip: sourceProvider == targetID, and
// the field carries its original parsed node) or as a LossItem naming the
// field (cross-provider: the target format has no binding for it, and
// re-emitting it under a foreign schema would be inventing a mapping this
// package never does). Returns the extra LossItems to append to the
// caller's loss report; mutates mapping in place for the round-trip case.
func projectUnmappedFields(mapping *yaml.Node, fields []UnmappedField, sourceProvider, targetID ProviderID) []LossItem {
	var loss []LossItem
	sameProvider := sourceProvider != "" && sourceProvider == targetID
	for _, f := range fields {
		if sameProvider && f.node != nil {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.Key}
			mapping.Content = append(mapping.Content, keyNode, f.node)
			continue
		}
		loss = append(loss, LossItem{
			Field: f.Key,
			Kind:  LossDropped,
			Note:  "unknown " + string(f.Provider) + " frontmatter field " + f.Key + " (" + f.Raw + ") has no mapping on " + string(targetID),
		})
	}
	return loss
}

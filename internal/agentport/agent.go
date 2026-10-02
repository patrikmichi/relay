package agentport

type Agent struct {
	Name        string
	Description string
	Body        string // markdown body after the frontmatter block (system prompt)
	Metadata    map[string]string

	// --- shared-ish ---
	Model string   // canonical hint: e.g. "sonnet"|"haiku"|"opus" (mapped per provider)
	Tools []string // canonical allowlist (CSV in Claude, map in opencode)

	// DeniedTools carries opencode's explicitly-denied tools (on-disk `tool: false` map entries).
	DeniedTools []string // opencode: tools explicitly set to `false`

	// --- typed-optional platform extensions (nil/zero = unset) ---
	Temperature *float64 // opencode
	Mode        string   // opencode: primary|subagent|all
	Memory      string   // Claude: project|global
	Skills      []string // Claude: bundled skill ids

	UnmappedSecurityFields []UnmappedSecurityField

	UnmappedFields []UnmappedField

	Provenance Provenance
}

// UnmappedSecurityField is one frontmatter key agentSecurityFrontmatterKeys
// recognized as security-relevant that decodeAgentIRField has no case for.
// Raw is a best-effort, length-capped YAML rendering of the field's value —
// diagnostic text for a loss-report Note, not a re-parseable representation.
type UnmappedSecurityField struct {
	Key string
	Raw string
}

package agentport

// Agent is the canonical, provider-agnostic in-memory representation of an
// agent definition (relay-standalone design §3b) — the Agent-IR analogue of
// Skill. Common fields (Name, Description, Body, Metadata) are understood
// by more than one provider; the typed-optional extension fields below
// belong to a single provider's format and are preserved on the IR only so
// a later migrate back to that same provider (or an explicit provider that
// also understands the field) doesn't lose them — see the agent CapSet /
// computeAgentLoss (agent_caps.go) for how Project() reports fields a
// target can't represent.
//
// Unlike Skill, Agent carries no Resources: every supported agent format
// (Claude, opencode) is a single flat `<name>.md` file with no containing
// resource directory — a structural simplification over skills.
type Agent struct {
	Name        string
	Description string
	Body        string // markdown body after the frontmatter block (system prompt)
	Metadata    map[string]string

	// --- shared-ish ---
	Model string   // canonical hint: e.g. "sonnet"|"haiku"|"opus" (mapped per provider)
	Tools []string // canonical allowlist (CSV in Claude, map in opencode)

	// DeniedTools carries opencode's explicitly-denied tools (on-disk
	// `tool: false` map entries). It is NOT a generic canonical IR field —
	// it has no frontmatter mapping entry and no agentFieldValue/
	// decodeAgentIRField case; it exists purely as a side-channel alongside
	// Tools so opencode's Load (agent_config_adapter.go) doesn't have to
	// silently discard denial information the moment it reshapes the
	// on-disk {tool: bool} map into the canonical allowlist-only Tools
	// list (toolsMapToList, agent_caps.go, drops false entries by
	// necessity — there's no other way to represent "denied" in a list).
	// Project() (agent_config_adapter.go) uses this to (a) re-encode
	// `tool: false` entries when projecting back onto opencode
	// (toolsListToMap), preserving denial semantics on an opencode round
	// trip, and (b) report a LossDropped item (toolsLossForTarget) when
	// projecting onto a target, like claude, whose allowlist-only shape
	// has no way to express "denied" at all.
	DeniedTools []string // opencode: tools explicitly set to `false`

	// --- typed-optional platform extensions (nil/zero = unset) ---
	Temperature *float64 // opencode
	Mode        string   // opencode: primary|subagent|all
	Memory      string   // Claude: project|global
	Skills      []string // Claude: bundled skill ids

	Provenance Provenance
}

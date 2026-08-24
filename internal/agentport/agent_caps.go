package agentport

import (
	"sort"
	"strings"
)

// This file is the Agent-IR analogue of the CapSet/computeLoss mechanism in
// adapter.go (relay-standalone design §3c). Providers: claude (primary),
// opencode (primary) — Codex/Cursor have no agent-file primitive and are
// out of scope (design §3d/Risks).
//
// Unlike skills' CapSet (config-driven via a provider config's
// `capabilities:` list — see config.go/buildCapSet), agent capability sets
// are small, fixed Go tables (agentCapsByProviderID below): only 2
// providers exist, and Model/Tools need bespoke degraded-mapping logic
// (modelLossForTarget/toolsLossForTarget) that a flat YAML capability list
// can't express anyway.
//
// Tools fidelity design note (explicitly-denied opencode tools): opencode's
// on-disk tools shape is a {tool: bool} MAP, so it can express "explicitly
// denied" (`tool: false`) — something Claude's CSV/list allowlist shape
// fundamentally cannot represent (absence there means "unset", never
// "denied"). Reshaping straight into the canonical Tools []string field
// (toolsMapToList) would silently discard `false` entries with no loss
// record at all. To avoid that, Agent carries a second, non-canonical
// side-channel field, DeniedTools (agent.go), populated only for opencode
// sources (agent_config_adapter.go's Load, via deniedTools below). Project()
// then: (a) re-encodes DeniedTools as `tool: false` when the TARGET is
// opencode too (toolsListToMap), so an opencode -> opencode round trip
// preserves denial semantics instead of losing them, and (b) reports a
// LossDropped item (toolsLossForTarget) naming the denied tools when the
// target is anything else (claude) — the smallest change that keeps this
// fidelity gap visible without inventing a new LossKind or touching the
// canonical IR field-binding tables (agent_ir_fields.go) that the parity
// gate depends on.

// AgentCapSet declares which Agent IR extension fields a provider's
// on-disk format can represent at all. Model and Tools are always
// representable by BOTH shipped providers (true for both claude and
// opencode) — what varies for those two fields is fidelity (exact vs
// reshaped/mapped), which computeAgentLoss does not model; see
// modelLossForTarget/toolsLossForTarget for that degraded-loss reporting.
type AgentCapSet struct {
	Model       bool
	Tools       bool
	Temperature bool
	Mode        bool
	Memory      bool
	Skills      bool
}

// agentCapsByProviderID is the fixed, reviewed capability table for the 2
// shipped agent providers (relay-standalone design §3c matrix):
//
//	IR field     claude      opencode
//	Model        preserved   degraded (both can represent it; see modelLossForTarget)
//	Tools        preserved   degraded (both can represent it; see toolsLossForTarget)
//	Temperature  dropped     preserved
//	Mode         dropped     preserved
//	Memory       preserved   dropped
//	Skills       preserved   dropped
var agentCapsByProviderID = map[ProviderID]AgentCapSet{
	ProviderClaude: {
		Model:       true,
		Tools:       true,
		Temperature: false,
		Mode:        false,
		Memory:      true,
		Skills:      true,
	},
	ProviderOpencode: {
		Model:       true,
		Tools:       true,
		Temperature: true,
		Mode:        true,
		Memory:      false,
		Skills:      false,
	},
}

// agentCapSetFor returns the fixed AgentCapSet for a shipped agent
// provider id, or the zero value (every field false/dropped) for an
// unrecognized id — matching buildCapSet's fail-safe-to-empty behavior for
// an unrecognized skill capability.
func agentCapSetFor(id ProviderID) AgentCapSet {
	return agentCapsByProviderID[id]
}

// computeAgentLoss inspects an Agent for populated fields the target
// adapter's AgentCapSet cannot represent AT ALL, returning a LossDropped
// LossItem for each — the Agent-IR analogue of computeLoss. Model and Tools
// are deliberately NOT handled here even though both are declared in
// AgentCapSet: they're always representable by both shipped providers, so
// they never produce a LossDropped item from this function; their fidelity
// loss (degraded, not dropped) is reported separately by
// modelLossForTarget/toolsLossForTarget, which an agent adapter's Project()
// layers on top of this function's result — mirroring how
// codexOpenAICodec.Project adds its own LossItems on top of computeLoss's.
func computeAgentLoss(a *Agent, caps AgentCapSet) []LossItem {
	var loss []LossItem
	add := func(field, note string) {
		loss = append(loss, LossItem{Field: field, Kind: LossDropped, Note: note})
	}
	if a.Temperature != nil && !caps.Temperature {
		add("Temperature", "target provider has no temperature equivalent")
	}
	if a.Mode != "" && !caps.Mode {
		add("Mode", "target provider has no primary/subagent/all mode equivalent")
	}
	if a.Memory != "" && !caps.Memory {
		add("Memory", "target provider has no memory-scope equivalent")
	}
	if len(a.Skills) > 0 && !caps.Skills {
		add("Skills", "target provider has no bundled-skills equivalent")
	}
	return loss
}

// claudeOpencodeModelAlias maps a Claude short model alias (as used in
// Claude agent frontmatter's `model:` key) to an opencode "provider/model"
// string. This is a best-effort, explicitly reviewed table (never inferred
// or reflection-derived) — an unmapped alias degrades to a synthesized
// passthrough (the raw alias string, so nothing is silently dropped) with a
// LossDegraded note explaining the mapping is inexact/unmapped.
var claudeOpencodeModelAlias = map[string]string{
	"opus":   "anthropic/claude-opus-4",
	"sonnet": "anthropic/claude-sonnet-4",
	"haiku":  "anthropic/claude-haiku-4",
}

// opencodeClaudeModelAlias is the reverse of claudeOpencodeModelAlias,
// derived once at init time rather than hand-duplicated (so the two tables
// can never drift out of sync with each other).
var opencodeClaudeModelAlias = reverseStringMap(claudeOpencodeModelAlias)

func reverseStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// modelLossForTarget maps model (in the SOURCE provider's vocabulary) to
// the target provider's vocabulary, returning the mapped string and,
// whenever the mapping wasn't an exact, known alias round-trip, a
// LossDegraded LossItem describing what happened. Returns ("", nil, nil)
// when model is empty (nothing to map). source/target must each be
// ProviderClaude or ProviderOpencode — any other id is treated as
// "unknown vocabulary" and always degrades (passthrough + note).
func modelLossForTarget(model string, source, target ProviderID) (mapped string, loss *LossItem) {
	if model == "" {
		return "", nil
	}
	if source == target {
		return model, nil
	}

	var table map[string]string
	switch {
	case source == ProviderClaude && target == ProviderOpencode:
		table = claudeOpencodeModelAlias
	case source == ProviderOpencode && target == ProviderClaude:
		table = opencodeClaudeModelAlias
	default:
		return model, &LossItem{
			Field: "Model",
			Kind:  LossDegraded,
			Note:  "no model-alias mapping table between " + string(source) + " and " + string(target) + " — passed through unmapped",
		}
	}

	if m, ok := table[model]; ok {
		return m, &LossItem{
			Field: "Model",
			Kind:  LossDegraded,
			Note:  "mapped " + string(source) + " model alias " + model + " -> " + string(target) + " " + m,
		}
	}
	return model, &LossItem{
		Field: "Model",
		Kind:  LossDegraded,
		Note:  "unrecognized " + string(source) + " model " + model + " — passed through unmapped to " + string(target),
	}
}

// toolsListToMap reshapes Claude's CSV/list allowlist shape (plus any
// canonical-IR DeniedTools) into opencode's {tool: bool} map shape: every
// allowed tool maps to true, and every explicitly-denied tool (denied is
// only ever non-empty when the source was itself an opencode agent with
// `tool: false` entries — see agent_config_adapter.go's Load, which
// populates Agent.DeniedTools via deniedTools below) maps to false. This
// is what lets an opencode -> opencode round trip preserve denial
// semantics instead of silently dropping them — see toolsLossForTarget for
// the complementary case: projecting DeniedTools onto a target, like
// claude, that has no boolean-denial shape at all.
func toolsListToMap(tools []string, denied []string) map[string]bool {
	if len(tools) == 0 && len(denied) == 0 {
		return nil
	}
	out := make(map[string]bool, len(tools)+len(denied))
	for _, t := range tools {
		out[t] = true
	}
	for _, t := range denied {
		out[t] = false
	}
	return out
}

// toolsMapToList reshapes opencode's {tool: bool} map shape into Claude's
// CSV/list allowlist shape: only tools explicitly set to true are
// included (a tool set to false — explicitly denied — has no equivalent in
// an allowlist-only shape and is dropped from THIS list, though not lost
// altogether — see deniedTools, which captures the same map's false
// entries onto Agent.DeniedTools separately). Sorted for determinism —
// map iteration order is not stable.
func toolsMapToList(tools map[string]bool) []string {
	if len(tools) == 0 {
		return nil
	}
	var out []string
	for tool, allowed := range tools {
		if allowed {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}

// deniedTools returns the sorted list of keys in tools explicitly set to
// false — the complement of toolsMapToList's "allowed" list. Populated
// onto Agent.DeniedTools by agent_config_adapter.go's Load so denial
// information survives past Load even though toolsMapToList itself must
// still drop those entries when reshaping into Claude's allowlist-only
// list shape (there is no other way to represent "denied" there). Sorted
// for determinism — map iteration order is not stable.
func deniedTools(tools map[string]bool) []string {
	if len(tools) == 0 {
		return nil
	}
	var out []string
	for tool, allowed := range tools {
		if !allowed {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}

// toolsLossForTarget reports tools-shape fidelity loss for a target
// provider:
//   - projecting a non-empty Tools allowlist onto opencode is always a
//     LossDegraded reshape (list -> map), even when denied is empty;
//   - projecting a non-empty DeniedTools onto any target OTHER than
//     opencode is a LossDropped item — an allowlist-only shape (claude)
//     cannot express "explicitly denied" at all, so unlike the opencode
//     case above this is a genuine loss of semantics, not just a shape
//     change.
//
// Returns nil when there is nothing to report for the given target (e.g.
// claude with no DeniedTools — no reshape needed and nothing dropped).
func toolsLossForTarget(tools []string, denied []string, target ProviderID) *LossItem {
	if target == ProviderOpencode {
		if len(tools) == 0 {
			return nil
		}
		return &LossItem{
			Field: "Tools",
			Kind:  LossDegraded,
			Note:  "reshaped CSV/list tools allowlist into opencode's {tool: bool} map (each listed tool set to true)",
		}
	}
	if len(denied) == 0 {
		return nil
	}
	return &LossItem{
		Field: "Tools",
		Kind:  LossDropped,
		Note:  "explicitly-denied tools {" + strings.Join(denied, ", ") + "} cannot be represented on " + string(target) + " — denial semantics lost",
	}
}

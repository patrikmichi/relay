package agentport

import (
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file is the Agent-IR analogue of the CapSet/computeLoss mechanism in
// adapter.go, covering the 5 shipped agent providers: claude, opencode,
// codex, cursor, gemini-cli.
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
	// ProviderCodex: agents/codex.yml (format: toml) declares name,
	// description, developer_instructions (body), model only — no
	// tools/temperature/mode/memory/skills key exists in Codex's
	// custom-agent TOML shape.
	ProviderCodex: {
		Model:       true,
		Tools:       false,
		Temperature: false,
		Mode:        false,
		Memory:      false,
		Skills:      false,
	},
	// ProviderCursor: agents/cursor.yml declares name, description, model
	// only — Cursor subagents have no tools-allowlist, temperature, mode,
	// memory, or bundled-skills key.
	ProviderCursor: {
		Model:       true,
		Tools:       false,
		Temperature: false,
		Mode:        false,
		Memory:      false,
		Skills:      false,
	},
	// ProviderID("gemini-cli"): agents/gemini-cli.yml declares name,
	// description, tools (real list, carried), model, temperature —
	// no mode/memory/skills equivalent.
	ProviderID("gemini-cli"): {
		Model:       true,
		Tools:       true,
		Temperature: true,
		Mode:        false,
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

// computeAgentLoss inspects an Agent for populated fields the target adapter's AgentCapSet cannot represent AT ALL, returning a LossDropped LossItem for each — the Agent-IR analogue of computeLoss.
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
	for _, f := range a.UnmappedSecurityFields {
		loss = append(loss, LossItem{
			Field:    f.Key,
			Kind:     LossDropped,
			Security: true,
			Note:     "security/restriction field " + f.Key + " (" + f.Raw + ") has no Agent-IR mapping — refused rather than silently dropped",
		})
	}
	return loss
}

// agentSecurityFrontmatterKeys is the fixed, reviewed set of source
// frontmatter keys (lower-cased for matching) that are permission or
// lifecycle controls agentIrFieldDescriptors has no binding for. "hooks" is
// included because lifecycle hooks execute code — their disappearance is a
// restriction gap, not cosmetic loss. Deliberately small and hand-reviewed,
// never inferred or pattern-matched, matching claudeOpencodeModelAlias's
// style above.
var agentSecurityFrontmatterKeys = map[string]bool{
	"disallowedtools": true, // Claude subagent config
	"permissionmode":  true, // Claude subagent config
	"hooks":           true, // Claude subagent config (lifecycle, executes code)
	"permission":      true,
	"readonly":        true,
	"mcpservers":      true,
	"sandbox_mode":    true,
	"approval_policy": true, // opencode agent config
}

// collectUnmappedSecurityFields walks root's top-level frontmatter mapping
// and returns an UnmappedSecurityField for every key that (a) is not one of
// this provider's configured frontmatter keys (case-insensitive — configured
// keys are already handled, correctly or not, by decodeAgentIRField) and (b)
// matches agentSecurityFrontmatterKeys. Called from agentConfigAdapter.Load
// after the normal frontmatter decode loop.
func collectUnmappedSecurityFields(root *yaml.Node, configured map[string]bool) []UnmappedSecurityField {
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

	var out []UnmappedSecurityField
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i].Value
		lower := strings.ToLower(key)
		if configured[lower] || !agentSecurityFrontmatterKeys[lower] {
			continue
		}
		out = append(out, UnmappedSecurityField{Key: key, Raw: renderNodeCompact(mapping.Content[i+1])})
	}
	return out
}

// renderNodeCompact renders a yaml.Node's value into short diagnostic text
// for a LossItem.Note — best-effort and length-capped; not a re-parseable
// representation of the field (see UnmappedSecurityField's doc comment).
func renderNodeCompact(node *yaml.Node) string {
	b, err := yaml.Marshal(node)
	if err != nil {
		return "<unrenderable value>"
	}
	text := strings.Join(strings.Fields(string(b)), " ")
	const maxLen = 120
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}
	return text
}

// claudeOpencodeModelAlias maps a Claude short model alias (as used in
// Claude agent frontmatter's `model:` key) to an opencode "provider/model"
// string. This is an explicitly reviewed table (never inferred
// or reflection-derived) — an unmapped alias degrades to a synthesized
// passthrough (the raw alias string, so nothing is silently dropped) with a
// LossDegraded note explaining the mapping is inexact/unmapped.
var claudeOpencodeModelAlias = map[string]string{
	"opus":   "anthropic/claude-opus-5",
	"sonnet": "anthropic/claude-sonnet-5",
	"haiku":  "anthropic/claude-haiku-4-5-20251001",
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
		return "", &LossItem{
			Field: "Model",
			Kind:  LossDropped,
			Note:  "no model-alias mapping table between " + string(source) + " and " + string(target) + " — omitted; target uses its default",
		}
	}

	if m, ok := table[model]; ok {
		return m, &LossItem{
			Field: "Model",
			Kind:  LossDegraded,
			Note:  "mapped " + string(source) + " model alias " + model + " -> " + string(target) + " " + m,
		}
	}
	return "", &LossItem{
		Field: "Model",
		Kind:  LossDropped,
		Note:  "unrecognized " + string(source) + " model " + model + " — omitted; target uses its default to " + string(target),
	}
}

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

var claudeOpencodeToolAlias = map[string]string{
	"Read":      "read",
	"Write":     "write",
	"Edit":      "edit",
	"Bash":      "bash",
	"Glob":      "glob",
	"Grep":      "grep",
	"Task":      "task",
	"TodoWrite": "todowrite",
	"WebFetch":  "webfetch",
	"WebSearch": "websearch",
}

// opencodeClaudeToolAlias is the reverse of claudeOpencodeToolAlias,
// derived once so the two tables can never drift out of sync.
var opencodeClaudeToolAlias = reverseStringMap(claudeOpencodeToolAlias)

// translateToolName maps one tool identity from source's vocabulary to
// target's. Same-provider and unrecognized source/target pairs return name
// unchanged; an unrecognized NAME within a known pair also passes through
// unchanged (see claudeOpencodeToolAlias's doc comment).
func translateToolName(name string, source, target ProviderID) string {
	if source == target {
		return name
	}
	var table map[string]string
	switch {
	case source == ProviderClaude && target == ProviderOpencode:
		table = claudeOpencodeToolAlias
	case source == ProviderOpencode && target == ProviderClaude:
		table = opencodeClaudeToolAlias
	default:
		return name
	}
	if mapped, ok := table[name]; ok {
		return mapped
	}
	return name
}

// translateToolNames maps every entry of names via translateToolName,
// returning nil for an empty input (matching the rest of this file's
// zero-value/omitempty conventions).
func translateToolNames(names []string, source, target ProviderID) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = translateToolName(n, source, target)
	}
	return out
}

func claudeAllowlistIsRestrictive(tools []string, source ProviderID) bool {
	return source == ProviderClaude && len(tools) > 0
}

// toolsLossForTarget reports tools-shape fidelity loss for a target
// provider:
//   - projecting a non-empty Tools allowlist onto opencode is always a
//     LossDegraded reshape (list -> map), even when denied is empty;
//   - projecting onto codex or cursor — neither has a tools key at all
//     (agent_caps.go's AgentCapSet.Tools == false for both) — is a
//     LossDropped item whenever there's anything to drop (either the
//     allowlist itself or any denied entries): unlike opencode's map
//     reshape, there is no partial representation to fall back to;
//   - projecting a non-empty DeniedTools onto any OTHER target (claude,
//     gemini-cli — both carry the Tools allowlist itself natively) is a
//     LossDropped item — an allowlist-only/plain-list shape cannot express
//     "explicitly denied" at all, so unlike the opencode case above this is
//     a genuine loss of semantics, not just a shape change.
//
// Returns nil when there is nothing to report for the given target (e.g.
// claude/gemini-cli with no DeniedTools — no reshape needed and nothing
// dropped).
func toolsLossForTarget(tools []string, denied []string, target ProviderID) *LossItem {
	switch target {
	case ProviderOpencode:
		if len(tools) == 0 {
			return nil
		}
		return &LossItem{
			Field: "Tools",
			Kind:  LossDegraded,
			Note:  "reshaped CSV/list tools allowlist into opencode's {tool: bool} map (each listed tool set to true)",
		}
	case ProviderCodex, ProviderCursor:
		if len(tools) == 0 && len(denied) == 0 {
			return nil
		}
		return &LossItem{
			Field: "Tools",
			Kind:  LossDropped,
			Note:  string(target) + " has no tools-allowlist equivalent — tools list dropped",
		}
	}
	// A literal "*" wildcard-deny entry (opencode's "everything else
	// denied" default) is exactly what an allowlist-only target — every
	// non-opencode target today — already implies by omitting a tool from
	// the list. It is not itself a loss; only a SPECIFIC per-tool denial
	// (deny this one tool while defaulting everything else to allowed) has
	// no allowlist-only equivalent.
	var specific []string
	for _, d := range denied {
		if d != "*" {
			specific = append(specific, d)
		}
	}
	if len(specific) == 0 {
		return nil
	}
	return &LossItem{
		Field:    "Tools",
		Kind:     LossDropped,
		Security: true,
		Note:     "explicitly-denied tools {" + strings.Join(specific, ", ") + "} cannot be represented on " + string(target) + " — denial semantics lost",
	}
}

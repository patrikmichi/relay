package agentport

import "fmt"

// builtinAgentProviderOrder is the stable display/enumeration order for the
// 2 shipped agent providers — the Agent-IR analogue of
// builtinProviderOrder.
var builtinAgentProviderOrder = []string{"claude", "opencode"}

// AllAgentAdapters returns one agentConfigAdapter per loaded agent provider
// config (agents/*.yml plus any user/project overrides) — the Agent-IR
// analogue of AllAdapters.
func AllAgentAdapters() []AgentAdapter {
	configs := loadedAgentProviderConfigs()
	seen := make(map[string]bool, len(configs))
	out := make([]AgentAdapter, 0, len(configs))

	for _, id := range builtinAgentProviderOrder {
		if cfg, ok := configs[id]; ok {
			out = append(out, newAgentConfigAdapter(cfg))
			seen[id] = true
		}
	}
	for _, id := range sortedConfigIDs(configs) {
		if seen[id] {
			continue
		}
		out = append(out, newAgentConfigAdapter(configs[id]))
	}
	return out
}

// AgentAdapterByID returns the agent adapter for the given ProviderID, or
// ok=false if id has no loaded agent provider config.
func AgentAdapterByID(id ProviderID) (a AgentAdapter, ok bool) {
	cfg, exists := loadedAgentProviderConfigs()[string(id)]
	if !exists {
		return nil, false
	}
	return newAgentConfigAdapter(cfg), true
}

// AgentDetectedProviders returns the subset of AllAgentAdapters() whose
// Detect() reports true (i.e. appear installed on this machine) — the
// Agent-IR analogue of DetectedProviders.
func AgentDetectedProviders() []AgentAdapter {
	var out []AgentAdapter
	for _, a := range AllAgentAdapters() {
		if a.Detect() {
			out = append(out, a)
		}
	}
	return out
}

// NewClaudeAgentAdapter returns the Claude Code agent AgentAdapter — a
// thin, named wrapper around the embedded agents/claude.yml config,
// mirroring NewClaudeAdapter's back-compat-constructor shape.
func NewClaudeAgentAdapter() AgentAdapter { return mustBuiltinAgentAdapter("claude") }

// NewOpencodeAgentAdapter returns the opencode agent AgentAdapter.
func NewOpencodeAgentAdapter() AgentAdapter { return mustBuiltinAgentAdapter("opencode") }

// mustBuiltinAgentAdapter resolves one of the 2 shipped agent provider ids.
// Since these ids are always embedded (TestEmbeddedAgentConfigsValid
// guards this at CI time), a lookup failure here indicates a packaging
// bug, not a runtime/user condition — mirrors mustBuiltinAdapter's panic
// rationale exactly.
func mustBuiltinAgentAdapter(id string) AgentAdapter {
	a, ok := AgentAdapterByID(ProviderID(id))
	if !ok {
		panic(fmt.Sprintf("agentport: built-in agent provider config %q missing or failed to load", id))
	}
	return a
}

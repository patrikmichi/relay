package agentport

import (
	"fmt"
	"sort"
)

// builtinProviderOrder is the stable display/enumeration order for the 4
// originally hard-coded providers — AllAdapters() preserves this order for
// back-compat with every existing caller/test, then appends any additional
// loaded providers (new YAML-only platforms, or a user/project override
// introducing a brand-new id) sorted by id.
var builtinProviderOrder = []string{"claude", "codex", "opencode", "cursor"}

// AllAdapters returns one configAdapter per loaded provider config —
// embedded defaults plus any user/project overrides (embed.go) — in a
// stable order: the 4 originally built-in providers first (claude, codex,
// opencode, cursor), then any additional providers sorted by id.
func AllAdapters() []Adapter {
	configs := loadedProviderConfigs()
	seen := make(map[string]bool, len(configs))
	out := make([]Adapter, 0, len(configs))

	for _, id := range builtinProviderOrder {
		if cfg, ok := configs[id]; ok {
			out = append(out, newConfigAdapter(cfg))
			seen[id] = true
		}
	}
	for _, id := range sortedConfigIDs(configs) {
		if seen[id] {
			continue
		}
		out = append(out, newConfigAdapter(configs[id]))
	}
	return out
}

// AdapterByID returns the adapter for the given ProviderID, or ok=false if
// id has no loaded provider config (embedded or override).
func AdapterByID(id ProviderID) (a Adapter, ok bool) {
	cfg, exists := loadedProviderConfigs()[string(id)]
	if !exists {
		return nil, false
	}
	return newConfigAdapter(cfg), true
}

// DetectedProviders returns the subset of AllAdapters() whose Detect()
// reports true (i.e. appear installed on this machine).
func DetectedProviders() []Adapter {
	var out []Adapter
	for _, a := range AllAdapters() {
		if a.Detect() {
			out = append(out, a)
		}
	}
	return out
}

// NewClaudeAdapter returns the Claude Code Adapter — a thin, named wrapper
// around the embedded claude.yml config. Kept for back-compat with every
// existing call site and test: the Strangler-Fig migration (design §5.2)
// deliberately never changes this constructor's signature, only what it
// returns internally (a *configAdapter instead of the deleted claudeAdapter
// struct).
func NewClaudeAdapter() Adapter { return mustBuiltinAdapter("claude") }

// NewCodexAdapter returns the Codex Adapter.
func NewCodexAdapter() Adapter { return mustBuiltinAdapter("codex") }

// NewOpencodeAdapter returns the opencode Adapter.
func NewOpencodeAdapter() Adapter { return mustBuiltinAdapter("opencode") }

// NewCursorAdapter returns the Cursor Adapter.
func NewCursorAdapter() Adapter { return mustBuiltinAdapter("cursor") }

// mustBuiltinAdapter resolves one of the 4 originally hard-coded provider
// ids. Since these ids are always embedded (mustParseEmbeddedConfigs panics
// at load time if they were ever missing/invalid — see
// TestEmbeddedConfigsValid), a lookup failure here indicates a packaging
// bug, not a runtime/user condition, so it panics rather than returning an
// error every one of these no-error-return constructors' 4 call sites
// (detect_test.go, resolve_test.go, migrate_test.go, and every internal/cli
// call site) would otherwise have to handle.
func mustBuiltinAdapter(id string) Adapter {
	a, ok := AdapterByID(ProviderID(id))
	if !ok {
		panic(fmt.Sprintf("agentport: built-in provider config %q missing or failed to load", id))
	}
	return a
}

func sortedConfigIDs(configs map[string]ProviderConfig) []string {
	out := make([]string, 0, len(configs))
	for id := range configs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

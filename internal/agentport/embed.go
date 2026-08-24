package agentport

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file is the packaging + override layer (§4 of the
// config-driven-adapters design): go:embed bakes the shipped provider
// configs into the binary so it works standalone with zero external files,
// exactly like today's hard-coded behavior; user and (opt-in) project tiers
// let a provider be added or patched without a new binary.

//go:embed providers/*.yml
var embeddedProviderFS embed.FS

// embeddedAgentProviderFS is the Agent-IR analogue of embeddedProviderFS:
// go:embed for agents/*.yml (relay-standalone design §3c/P1.3) — a
// separate embedded directory (not providers/*.yml) so skill and agent
// provider configs never collide by id, and so TestEmbeddedConfigsValid's
// skill-scoped assertions are untouched by the new agent configs.
//
//go:embed agents/*.yml
var embeddedAgentProviderFS embed.FS

// AllowProjectProviderOverrides gates whether .relay/providers/*.yml
// (repo-local, untrusted-input provider overrides — see §4.2/R6) are loaded
// at all. Off by default. A caller (e.g. the CLI, after an explicit user
// opt-in flag or config setting) may set this to true before invoking any
// agentport provider-resolution function (AllAdapters, AdapterByID,
// DetectedProviders, New*Adapter).
var AllowProjectProviderOverrides = false

// loadedProviderConfigs loads the full provider-config set for THIS call:
// embedded defaults, overlaid by ~/.config/relay/providers/*.yml (user
// tier), overlaid by .relay/providers/*.yml (project tier, gated by
// AllowProjectProviderOverrides) — whole-provider replacement by id,
// precedence project > user > embedded (§4.2).
//
// Deliberately NOT memoized: re-scanned on every call so tests that change
// $HOME (t.Setenv("HOME", ...)) or the working directory per test case
// never observe a stale cross-test cache, and so an override file edited
// mid-session takes effect on the next call without a process restart. The
// embedded/override YAML files are tiny; re-parsing them per call is not a
// meaningful cost next to the filesystem walks the rest of the package
// already does per operation.
func loadedProviderConfigs() map[string]ProviderConfig {
	hookNames := registeredHookNames()
	codecNames := registeredCodecNames()

	configs := mustParseEmbeddedConfigs(hookNames, codecNames)

	if home, err := os.UserHomeDir(); err == nil {
		applyOverrideTier(configs, filepath.Join(home, ".config", "relay", "providers"), hookNames, codecNames)
	}
	if AllowProjectProviderOverrides {
		applyOverrideTier(configs, filepath.Join(".relay", "providers"), hookNames, codecNames)
	}

	return configs
}

// mustParseEmbeddedConfigs parses every embedded providers/*.yml. A
// malformed embedded config is a build-time programmer error, not
// something a user can trigger — TestEmbeddedConfigsValid asserts this
// never panics in CI, so a panic here (rather than a silently degraded
// provider set) fails loudly and immediately if it ever does regress.
func mustParseEmbeddedConfigs(hookNames, codecNames map[string]bool) map[string]ProviderConfig {
	entries, err := embeddedProviderFS.ReadDir("providers")
	if err != nil {
		panic(fmt.Sprintf("agentport: read embedded providers dir: %v", err))
	}

	out := map[string]ProviderConfig{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		raw, err := embeddedProviderFS.ReadFile(filepath.Join("providers", e.Name()))
		if err != nil {
			panic(fmt.Sprintf("agentport: read embedded %s: %v", e.Name(), err))
		}
		cfg, err := parseProviderConfig(raw, hookNames, codecNames)
		if err != nil {
			panic(fmt.Sprintf("agentport: embedded provider config %s is invalid: %v", e.Name(), err))
		}
		if _, dup := out[cfg.ID]; dup {
			panic(fmt.Sprintf("agentport: duplicate embedded provider id %q (in %s)", cfg.ID, e.Name()))
		}
		out[cfg.ID] = *cfg
	}
	return out
}

// loadedAgentProviderConfigs is the Agent-IR analogue of
// loadedProviderConfigs: embedded agents/*.yml defaults, overlaid by
// ~/.config/relay/agents/*.yml (user tier), overlaid by .relay/agents/*.yml
// (project tier, gated by the SAME AllowProjectProviderOverrides flag skill
// overrides use — one opt-in switch for both kinds). Deliberately not
// memoized, for the same reasons as loadedProviderConfigs.
func loadedAgentProviderConfigs() map[string]ProviderConfig {
	hookNames := registeredHookNames()
	codecNames := registeredCodecNames()

	configs := mustParseEmbeddedAgentConfigs(hookNames, codecNames)

	if home, err := os.UserHomeDir(); err == nil {
		applyOverrideTier(configs, filepath.Join(home, ".config", "relay", "agents"), hookNames, codecNames)
	}
	if AllowProjectProviderOverrides {
		applyOverrideTier(configs, filepath.Join(".relay", "agents"), hookNames, codecNames)
	}

	return configs
}

// mustParseEmbeddedAgentConfigs parses every embedded agents/*.yml — the
// Agent-IR analogue of mustParseEmbeddedConfigs. A malformed embedded
// config is a build-time programmer error (see TestEmbeddedAgentConfigsValid),
// so this panics rather than degrading silently, exactly like its skill
// counterpart.
func mustParseEmbeddedAgentConfigs(hookNames, codecNames map[string]bool) map[string]ProviderConfig {
	entries, err := embeddedAgentProviderFS.ReadDir("agents")
	if err != nil {
		panic(fmt.Sprintf("agentport: read embedded agents dir: %v", err))
	}

	out := map[string]ProviderConfig{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		raw, err := embeddedAgentProviderFS.ReadFile(filepath.Join("agents", e.Name()))
		if err != nil {
			panic(fmt.Sprintf("agentport: read embedded %s: %v", e.Name(), err))
		}
		cfg, err := parseProviderConfig(raw, hookNames, codecNames)
		if err != nil {
			panic(fmt.Sprintf("agentport: embedded agent provider config %s is invalid: %v", e.Name(), err))
		}
		if _, dup := out[cfg.ID]; dup {
			panic(fmt.Sprintf("agentport: duplicate embedded agent provider id %q (in %s)", cfg.ID, e.Name()))
		}
		out[cfg.ID] = *cfg
	}
	return out
}

// applyOverrideTier scans dir for *.yml provider config files and merges
// them into configs (whole-provider replace by id, with an optional
// `extends` shallow-overlay onto an already-present config of the same id
// — §4.2). A missing dir is not an error (no overrides at this tier). An
// invalid file is reported to stderr and skipped, leaving whatever was
// already in configs for that id untouched (embedded, or a lower override
// tier) — §4.4's fail-safe, so a typo in a user override can never brick a
// shipped provider.
func applyOverrideTier(configs map[string]ProviderConfig, dir string, hookNames, codecNames map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // no overrides at this tier — not an error
	}

	seenThisTier := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentport: skipping provider override %s: %v\n", path, err)
			continue
		}

		var probe struct {
			ID      string `yaml:"id"`
			Extends string `yaml:"extends"`
		}
		if err := yaml.Unmarshal(raw, &probe); err != nil {
			fmt.Fprintf(os.Stderr, "agentport: skipping provider override %s: %v\n", path, err)
			continue
		}
		if probe.ID == "" {
			fmt.Fprintf(os.Stderr, "agentport: skipping provider override %s: missing required field \"id\"\n", path)
			continue
		}
		if seenThisTier[probe.ID] {
			fmt.Fprintf(os.Stderr, "agentport: skipping provider override %s: duplicate id %q within this tier\n", path, probe.ID)
			continue
		}

		var cfg *ProviderConfig
		if probe.Extends != "" {
			base, ok := configs[probe.Extends]
			if !ok {
				fmt.Fprintf(os.Stderr, "agentport: skipping provider override %s: extends unknown provider %q\n", path, probe.Extends)
				continue
			}
			merged, err := shallowOverlay(base, raw, hookNames, codecNames)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agentport: skipping provider override %s: %v\n", path, err)
				continue
			}
			cfg = merged
		} else {
			parsed, err := parseProviderConfig(raw, hookNames, codecNames)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agentport: skipping invalid provider override %s: %v\n", path, err)
				continue
			}
			cfg = parsed
		}

		seenThisTier[probe.ID] = true
		configs[probe.ID] = *cfg
	}
}

// shallowOverlay applies override's top-level keys (except "extends"
// itself) onto a copy of base, then re-validates the merged result. This is
// an explicit, shallow, named-key overlay — never a deep merge — so a user
// authoring a partial patch config gets predictable behavior (§4.2: "so
// users don't get surprised by deep-merge").
func shallowOverlay(base ProviderConfig, overrideRaw []byte, hookNames, codecNames map[string]bool) (*ProviderConfig, error) {
	baseRaw, err := yaml.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("re-marshal base config %q: %w", base.ID, err)
	}
	var baseMap map[string]interface{}
	if err := yaml.Unmarshal(baseRaw, &baseMap); err != nil {
		return nil, fmt.Errorf("re-parse base config %q: %w", base.ID, err)
	}

	var overrideMap map[string]interface{}
	if err := yaml.Unmarshal(overrideRaw, &overrideMap); err != nil {
		return nil, fmt.Errorf("parse override: %w", err)
	}

	for k, v := range overrideMap {
		if k == "extends" {
			continue
		}
		baseMap[k] = v
	}

	mergedRaw, err := yaml.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("re-marshal merged config: %w", err)
	}
	return parseProviderConfig(mergedRaw, hookNames, codecNames)
}

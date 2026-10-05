package agentport

import (
	"os"
	"path/filepath"
)

type OverrideDiagnostic struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// OverrideDiagnostics reuses the actual override validator and precedence rules.
func OverrideDiagnostics() []OverrideDiagnostic {
	out := []OverrideDiagnostic{}
	hooks, codecs := registeredHookNames(), registeredCodecNames()
	for _, kind := range []ArtifactKind{KindSkill, KindAgent} {
		folder := "providers"
		configs := mustParseEmbeddedConfigs(hooks, codecs)
		if kind == KindAgent {
			folder = "agents"
			configs = mustParseEmbeddedAgentConfigs(hooks, codecs)
		}
		report := func(path, reason string) { out = append(out, OverrideDiagnostic{Path: path, Status: reason}) }
		if home, err := os.UserHomeDir(); err == nil {
			applyOverrideTier(configs, filepath.Join(home, ".config", "relay", folder), hooks, codecs, kind, report)
		}
		dir := filepath.Join(".relay", folder)
		if AllowProjectProviderOverrides {
			applyOverrideTier(configs, dir, hooks, codecs, kind, report)
		} else if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				if filepath.Ext(entry.Name()) == ".yml" {
					report(filepath.Join(dir, entry.Name()), "project overrides disabled")
				}
			}
		}
	}
	return out
}

package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentRef identifies one on-disk agent instance discovered by AgentList —
// the Agent-IR analogue of SkillRef.
type AgentRef struct {
	Name     string
	Provider ProviderID
	Scope    Scope
	Path     string // flat "<name>.md" file
}

// AgentList enumerates agents present for adapter a in the given scope,
// across every directory the adapter searches (UserDirs()/ProjectDirs(), in
// priority order) — the Agent-IR analogue of List. Every shipped agent
// provider is layout: flat with direct discovery (no SKILL.md-style
// resource-dir shape, no recursive walk — see agents/*.yml), so this is
// simpler than List: a direct-children "*.md" scan only. Duplicate names
// across directories are resolved by priority order (the first directory
// that has the name wins). Returns an empty (non-nil-error) slice if
// nothing is found — not an error.
func AgentList(a AgentAdapter, scope Scope) ([]AgentRef, error) {
	dirs := a.UserDirs()
	if scope == ScopeProject {
		rel := a.ProjectDirs()
		dirs = make([]string, len(rel))
		for i, r := range rel {
			abs, err := filepath.Abs(r)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", r, err)
			}
			dirs[i] = abs
		}
	}

	seen := map[string]bool{}
	var out []AgentRef
	for _, d := range dirs {
		if !dirExists(d) {
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", d, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, AgentRef{Name: name, Provider: a.ID(), Scope: scope, Path: filepath.Join(d, e.Name())})
		}
	}
	return out, nil
}

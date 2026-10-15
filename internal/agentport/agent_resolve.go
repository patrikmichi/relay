package agentport

import (
	"fmt"
	"os"
	"path/filepath"
)

// ResolveAgentPath finds the on-disk location of a named agent for the
// given AgentAdapter + scope — the Agent-IR analogue of ResolveSkillPath.
// Every shipped agent provider is layout: flat (agents/*.yml): there is no
// SKILL.md-style resource-dir shape to fall back to, so this only ever
// looks for "<dir>/<name><a.FileExt()>" (".md" for every markdown-format
// provider, ".toml" for codex).
func ResolveAgentPath(a AgentAdapter, scope Scope, name string) (string, error) {
	if err := validateNameFor(a, name); err != nil {
		return "", err
	}
	if scope == ScopeProject {
		return resolveProjectAgentPath(a.ProjectDirs(), name, a.FileExt())
	}
	path, ok := findAgentInDirs(a.UserDirs(), name, a.FileExt())
	if !ok {
		return "", fmt.Errorf("agent %q not found for provider %s (user scope)", name, a.ID())
	}
	return path, nil
}

// ResolveOwnAgentPath is the destructive-operation-safe analogue of
// ResolveAgentPath: it searches ONLY the adapter's own writable directories
// (the leading OwnUserDirCount()/OwnProjectDirCount() entries) — the
// Agent-IR analogue of ResolveOwnSkillPath, for the same reason (agent
// uninstall must never remove a file from a directory the provider doesn't
// own).
func ResolveOwnAgentPath(a AgentAdapter, scope Scope, name string) (string, error) {
	if err := validateNameFor(a, name); err != nil {
		return "", err
	}

	if scope == ScopeProject {
		dirs := ownDirs(a.ProjectDirs(), a.OwnProjectDirCount())
		path, err := resolveProjectAgentPath(dirs, name, a.FileExt())
		if err != nil {
			return "", fmt.Errorf("agent %q not found for provider %s (project scope, own dirs only): %w", name, a.ID(), err)
		}
		return path, nil
	}

	dirs := ownDirs(a.UserDirs(), a.OwnUserDirCount())
	path, ok := findAgentInDirs(dirs, name, a.FileExt())
	if !ok {
		return "", fmt.Errorf("agent %q not found for provider %s (user scope, own dirs only)", name, a.ID())
	}
	return path, nil
}

// findAgentInDirs checks each candidate base directory (in order) for
// "<dir>/<name><ext>", returning the first match — the Agent-IR analogue of
// findSkillInDirs, minus the SKILL.md-directory branch (agents have no
// resource-bearing directory shape).
func findAgentInDirs(dirs []string, name string, extensions ...string) (string, bool) {
	for _, d := range dirs {
		flatFile := filepath.Join(d, name+extensionOrMarkdown(extensions))
		if fi, err := os.Stat(flatFile); err == nil && !fi.IsDir() {
			return flatFile, true
		}
	}
	return "", false
}

// resolveProjectAgentPath searches relDirs (relative to the current working
// directory, walking up to the filesystem root) for name+extensionOrMarkdown(extensions) — the
// Agent-IR analogue of resolveProjectSkillPath.
func resolveProjectAgentPath(relDirs []string, name string, extensions ...string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	dir := cwd
	for {
		bases := make([]string, len(relDirs))
		for i, rel := range relDirs {
			bases[i] = filepath.Join(dir, rel)
		}
		if path, ok := findAgentInDirs(bases, name, extensions...); ok {
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("agent %q not found in project dirs (searched from %s upward)", name, cwd)
}

func extensionOrMarkdown(extensions []string) string {
	if len(extensions) > 0 {
		return extensions[0]
	}
	return ".md"
}

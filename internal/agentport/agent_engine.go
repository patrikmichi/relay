package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AgentPlan is the Agent-IR analogue of Plan (engine.go): the preview of an
// agent migration — which file would be written to the target provider, the
// projected fidelity-loss report, and where it'd land. MigrateAgent builds
// an AgentPlan without writing anything, mirroring Migrate/Plan exactly.
type AgentPlan struct {
	Agent  *Agent
	Target AgentAdapter
	Scope  Scope
	Files  map[string][]byte // relative path -> content, from Target.Project() — always a single "<name>.md" entry (agents carry no resources)
	Loss   []LossItem
	// TargetPaths is the absolute directory the files would be written
	// under. Unlike Plan.TargetPaths (a skill's own "<dir>/<name>/"
	// subdirectory), this is the provider's directory ITSELF (e.g.
	// "~/.claude/agents") — agents are flat "<name>.md" files with no
	// containing subdirectory (see AgentTargetDir).
	TargetPaths string
}

// HasDropped reports whether the plan's loss report contains any
// LossDropped item (used by `--strict`) — the AgentPlan analogue of
// Plan.HasDropped.
func (p *AgentPlan) HasDropped() bool {
	for _, l := range p.Loss {
		if l.Kind == LossDropped {
			return true
		}
	}
	return false
}

// AgentTargetDir resolves the absolute directory an agent would be written
// to (or read back from) for the given target adapter + scope — the
// Agent-IR analogue of TargetDir. Unlike TargetDir, this does NOT join a
// "<name>/" subdirectory onto the base: every shipped agent provider is
// layout: flat (agents/*.yml) — a single "<name>.md" file lives directly
// inside the provider's own directory, with no per-agent containing
// subdirectory the way a skill has "<dir>/<name>/SKILL.md". Shared by
// MigrateAgent (to compute AgentPlan.TargetPaths) and Rollback (for
// KindAgent manifest entries — see rollback.go).
func AgentTargetDir(target pathAdapter, scope Scope) (string, error) {
	if target == nil {
		return "", fmt.Errorf("nil target adapter")
	}
	dirs := target.UserDirs()
	if scope == ScopeProject {
		dirs = target.ProjectDirs()
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("provider %s has no directories for scope %s", target.ID(), scope)
	}

	base := dirs[0]
	if scope == ScopeProject {
		abs, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", base, err)
		}
		base = abs
	}
	return base, nil
}

// MigrateAgent projects src (already-canonical Agent IR) into target's
// on-disk format, producing a previewable AgentPlan. It does not write
// anything — the Agent-IR analogue of Migrate.
func MigrateAgent(src *Agent, target AgentAdapter, scope Scope) (*AgentPlan, error) {
	if src == nil {
		return nil, fmt.Errorf("source agent is nil")
	}
	if target == nil {
		return nil, fmt.Errorf("target adapter is nil")
	}
	if err := ValidateName(src.Name); err != nil {
		return nil, err
	}

	files, loss, err := target.Project(src)
	if err != nil {
		return nil, fmt.Errorf("project to %s: %w", target.ID(), err)
	}

	targetDir, err := AgentTargetDir(target, scope)
	if err != nil {
		return nil, err
	}

	return &AgentPlan{
		Agent:       src,
		Target:      target,
		Scope:       scope,
		Files:       files,
		Loss:        loss,
		TargetPaths: targetDir,
	}, nil
}

// WriteAgent materializes an AgentPlan's file under plan.TargetPaths and
// records a manifest ledger entry with Kind: KindAgent — the Agent-IR
// analogue of Write.
func WriteAgent(plan *AgentPlan) error {
	if plan == nil {
		return fmt.Errorf("nil plan")
	}
	if err := os.MkdirAll(plan.TargetPaths, 0o755); err != nil {
		return fmt.Errorf("create target dir %s: %w", plan.TargetPaths, err)
	}
	for rel, data := range plan.Files {
		dest := filepath.Join(plan.TargetPaths, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("create dir for %s: %w", dest, err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
	}

	entry := ManifestEntry{
		Name:           plan.Agent.Name,
		Kind:           KindAgent,
		Provider:       plan.Target.ID(),
		Scope:          plan.Scope,
		SourceProvider: plan.Agent.Provenance.SourceProvider,
		TargetPaths:    HashFiles(plan.Files),
		Timestamp:      time.Now().UTC(),
		Provenance:     plan.Agent.Provenance,
	}
	return RecordEntry(entry)
}

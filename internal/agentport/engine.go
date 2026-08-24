package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Plan is the preview of a migration: which files would be written to the
// target provider, the projected fidelity-loss report, and where they'd
// land. Migrate builds a Plan without writing anything, so callers (the CLI)
// can print the loss report and let the user decide before calling Write.
type Plan struct {
	Skill       *Skill
	Target      Adapter
	Scope       Scope
	Files       map[string][]byte // relative path -> content, from Target.Project()
	Loss        []LossItem
	TargetPaths string // absolute directory the files would be written under (<dir>/<name>/)
}

// HasDropped reports whether the plan's loss report contains any
// LossDropped item (used by `--strict`).
func (p *Plan) HasDropped() bool {
	for _, l := range p.Loss {
		if l.Kind == LossDropped {
			return true
		}
	}
	return false
}

// pathAdapter is the minimal directory-resolution surface TargetDir needs —
// the subset both Adapter (Skill) and AgentAdapter already declare
// identically (ID/UserDirs/ProjectDirs). Narrowing TargetDir's parameter to
// this interface (rather than Adapter specifically) is what lets Rollback
// resolve either kind of adapter through the same function — a pure
// widening of what TargetDir accepts; every existing Adapter-typed call
// site keeps compiling and behaving unchanged, since Adapter already
// satisfies pathAdapter.
type pathAdapter interface {
	ID() ProviderID
	UserDirs() []string
	ProjectDirs() []string
}

// TargetDir resolves the absolute directory a skill or agent named name
// would be written to (or read back from) for the given target adapter +
// scope: UserDirs()[0] for ScopeUser (already absolute), or
// ProjectDirs()[0] resolved against the current working directory for
// ScopeProject. Shared by Migrate (to compute Plan.TargetPaths) and
// Rollback (to relocate the files a manifest entry recorded without
// re-deriving this logic) — kind-agnostic since P1.6: target may be a Skill
// Adapter or an AgentAdapter.
func TargetDir(target pathAdapter, scope Scope, name string) (string, error) {
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
	return filepath.Join(base, name), nil
}

// resolveTargetForEntry picks the correctly-kinded adapter for a manifest
// entry's Provider: an AgentAdapter when entry.Kind == KindAgent, a Skill
// Adapter otherwise (KindSkill, or the pre-P1.5 default LoadManifest
// normalizes absent Kind to). This is the "resolver that picks which
// adapter to load-back" relay-standalone design §3e calls out —
// TargetPaths hashing, Write, withManifestLock, atomic save, and
// rollback.go's hash-verify-then-remove are already kind-agnostic (they
// operate on map[relpath]sha256); only adapter SELECTION needed to become
// kind-aware.
func resolveTargetForEntry(entry ManifestEntry) (pathAdapter, error) {
	if entry.Kind == KindAgent {
		a, ok := AgentAdapterByID(entry.Provider)
		if !ok {
			return nil, fmt.Errorf("unknown agent provider %q in manifest entry", entry.Provider)
		}
		return a, nil
	}
	a, ok := AdapterByID(entry.Provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q in manifest entry", entry.Provider)
	}
	return a, nil
}

// Migrate projects src (already-canonical IR) into target's on-disk format,
// producing a previewable Plan. It does not write anything.
func Migrate(src *Skill, target Adapter, scope Scope) (*Plan, error) {
	if src == nil {
		return nil, fmt.Errorf("source skill is nil")
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

	targetDir, err := TargetDir(target, scope, src.Name)
	if err != nil {
		return nil, err
	}

	return &Plan{
		Skill:       src,
		Target:      target,
		Scope:       scope,
		Files:       files,
		Loss:        loss,
		TargetPaths: targetDir,
	}, nil
}

// Write materializes a Plan's files under Plan.TargetPaths and records a
// manifest ledger entry.
func Write(plan *Plan) error {
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
		Name:           plan.Skill.Name,
		Kind:           KindSkill,
		Provider:       plan.Target.ID(),
		Scope:          plan.Scope,
		SourceProvider: plan.Skill.Provenance.SourceProvider,
		TargetPaths:    HashFiles(plan.Files),
		Timestamp:      time.Now().UTC(),
		Provenance:     plan.Skill.Provenance,
	}
	return RecordEntry(entry)
}

package agentport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// lockAcquireTimeout bounds how long Write waits for another `relay`
// process to release its lock on the same (provider, scope, name,
// project) artifact before giving up — this is local disk contention
// between CLI invocations, never a network wait, so a short bound is
// correct; a genuinely stuck holder should be investigated, not waited on
// indefinitely.
const txnLockAcquireTimeout = 30 * time.Second

// skillLockKey identifies the artifact a skill write mutates, for
// txn.Begin's deterministic-order locking and pending-journal detection —
// the same identity Rollback/uninstall use to scope their own operations
// (name+provider+scope[+projectRoot]), so a concurrent write and rollback
// of the same skill can never interleave.
func skillLockKey(provider ProviderID, scope Scope, name, projectRoot string) string {
	return fmt.Sprintf("skill:%s:%s:%s:%s", provider, scope, name, projectRoot)
}

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
// re-deriving this logic) — kind-agnostic: target may be a Skill Adapter
// or an AgentAdapter.
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
// Adapter otherwise (KindSkill, or the default LoadManifest normalizes
// absent Kind to). TargetPaths hashing, Write, withManifestLock, atomic
// save, and rollback.go's hash-verify-then-remove are already
// kind-agnostic (they operate on map[relpath]sha256); only adapter
// SELECTION needs to be kind-aware.
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
// manifest ledger entry, through the durable transaction/recovery engine
// preflight -> staged -> prepared journal ->
// applying -> committed. It refuses to touch the filesystem at all unless
// PreflightWrite passes, both before acquiring the artifact's lock and
// again immediately after (protects against a stale decision if another
// process changed the ledger or destination between planning and
// locking), and refuses to start at all if a prior interrupted write on
// the same artifact left a pending journal (`relay recover` owns that
// case).
func Write(plan *Plan) error {
	if err := PreflightWrite(plan); err != nil {
		return err
	}

	projectRoot := ""
	if plan.Scope == ScopeProject {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve project root: %w", err)
		}
		projectRoot = cwd
	}
	lockKey := skillLockKey(plan.Target.ID(), plan.Scope, plan.Skill.Name, projectRoot)

	ctx, cancel := context.WithTimeout(context.Background(), txnLockAcquireTimeout)
	defer cancel()
	tx, err := txn.Begin(ctx, fmt.Sprintf("skill write %s -> %s", plan.Skill.Name, plan.Target.ID()), []string{lockKey})
	if err != nil {
		return err
	}

	// Re-check preflight now that the lock is held — nothing else can be
	// concurrently mutating this artifact from this point on, but the
	// decision above was made before that guarantee existed.
	if err := PreflightWrite(plan); err != nil {
		return tx.Discard(err)
	}

	if err := os.MkdirAll(plan.TargetPaths, 0o755); err != nil {
		return tx.Discard(fmt.Errorf("create target dir %s: %w", plan.TargetPaths, err))
	}
	for rel, data := range plan.Files {
		mode := os.FileMode(0o644)
		if rf, ok := plan.Skill.Resources[rel]; ok {
			mode = rf.Mode
		}
		if err := tx.Stage(plan.TargetPaths, rel, data, mode); err != nil {
			return tx.Discard(err)
		}
	}
	if err := tx.Persist(); err != nil {
		return tx.Discard(err)
	}
	// Apply re-verifies containment immediately before each write (a
	// path validated at preflight can go stale — an intermediate
	// directory swapped for a symlink between then and now), reusing
	// verifyPathHasNoSymlinks directly rather than duplicating its logic.
	if err := tx.Apply(verifyPathHasNoSymlinks); err != nil {
		return tx.Discard(err)
	}

	entry := ManifestEntry{
		Name:           plan.Skill.Name,
		Kind:           KindSkill,
		Provider:       plan.Target.ID(),
		Scope:          plan.Scope,
		SourceProvider: plan.Skill.Provenance.SourceProvider,
		TargetPaths:    HashFiles(plan.Files),
		FileModes:      fileModes(plan.Files, plan.Skill.Resources),
		Timestamp:      time.Now().UTC(),
		Provenance:     plan.Skill.Provenance,
		ProjectRoot:    projectRoot,
		TransactionID:  tx.ID(),
	}
	return tx.Commit(func() error { return RecordEntry(entry) })
}

// fileModes projects each written file's resolved mode (0644 default,
// or the resource's own mode when tracked) into the ledger-storable
// map[relpath]uint32 shape: a hash alone can't tell a later
// rollback whether a script needs its executable bit restored.
func fileModes(files map[string][]byte, resources map[string]ResourceFile) map[string]uint32 {
	out := make(map[string]uint32, len(files))
	for rel := range files {
		mode := os.FileMode(0o644)
		if rf, ok := resources[rel]; ok {
			mode = rf.Mode
		}
		out[rel] = uint32(mode)
	}
	return out
}

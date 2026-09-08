package agentport

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Sentinel errors identify the class of preflight rejection so callers and
// tests can branch on the failure kind instead of parsing message text.
// These are the P1.3 containment guard for R05-R08: a narrow layer in
// front of the write engines and Rollback that stops damage before it
// happens. The full transactional/recovery engine (staging, journaling,
// snapshot restore) is Phase 3 — these checks only ever refuse a mutation,
// they never repair or roll anything back themselves.
var (
	// ErrWriteConflict means the destination already holds content that
	// differs from what's about to be written — R07: without this, Write/
	// WriteAgent silently overwrite a prior artifact (Relay-managed or
	// not) with no way to recover the original bytes.
	ErrWriteConflict = errors.New("destination already has different content")

	// ErrDestinationSymlink means a path component under a write or
	// rollback target is itself a symlink — R05: a symlinked leaf file or
	// an intermediate symlinked directory lets a write or delete land
	// outside the intended artifact tree.
	ErrDestinationSymlink = errors.New("destination path contains a symlink")

	// ErrLedgerUnavailable means the manifest ledger could not be loaded
	// before a mutation began — R08: without this check, both write
	// engines mutate files first and only discover a broken ledger when
	// RecordEntry runs afterward, leaving changed files with no ledger
	// record.
	ErrLedgerUnavailable = errors.New("manifest ledger is unavailable")

	// ErrAmbiguousProjectRollback means a project-scope manifest entry has
	// no verifiable project identity to check against the current working
	// directory — R06: without this, Rollback re-derives its target
	// purely from CWD and current provider config, so it can delete an
	// unrelated project's files while dropping a different project's
	// ledger entry.
	ErrAmbiguousProjectRollback = errors.New("cannot verify this rollback targets the project it was installed into")
)

// verifyPathHasNoSymlinks lstats every path component from root down to
// filepath.Join(root, rel) inclusive (root itself, each intermediate
// directory, and the leaf), rejecting if any already-existing component is
// a symlink. A component that doesn't exist yet is safe: MkdirAll/
// WriteFile/os.Remove create or act on it fresh, so nothing already there
// could be a symlink. Shared by write preflight (destination) and Rollback
// (deletion target) — both need the identical no-follow guarantee.
func verifyPathHasNoSymlinks(root, rel string) error {
	if info, err := os.Lstat(root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrDestinationSymlink, root)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", root, err)
	}

	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrDestinationSymlink, cur)
		}
	}
	return nil
}

// preflightWrite validates a full write BEFORE any file is touched: the
// ledger must be readable, no destination path may traverse or land on a
// symlink, and no destination file may already exist with content that
// differs from what's about to be written. It performs zero mutation —
// Write/WriteAgent only proceed to the filesystem once this returns nil.
// Shared by both write engines (engine.go's Write, agent_engine.go's
// WriteAgent) and exported (PreflightWrite/PreflightWriteAgent below) so a
// multi-target caller can validate every target before writing any of
// them.
func preflightWrite(targetPaths string, files map[string][]byte) error {
	if _, err := LoadManifest(); err != nil {
		return fmt.Errorf("%w: %v", ErrLedgerUnavailable, err)
	}
	for rel := range files {
		if err := verifyPathHasNoSymlinks(targetPaths, rel); err != nil {
			return err
		}
	}
	for rel, data := range files {
		dest := filepath.Join(targetPaths, filepath.FromSlash(rel))
		existing, err := os.ReadFile(dest)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read existing %s: %w", dest, err)
		}
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("%w: %s (roll back or remove the existing artifact before reinstalling)", ErrWriteConflict, dest)
		}
	}
	return nil
}

// PreflightWrite validates plan's destination without writing anything —
// the exported form of preflightWrite for a skill Plan. A caller writing
// to several targets (e.g. `relay skill migrate --to a --to b`) should
// call this for every target's plan before calling Write for any of them,
// so an invalid later target aborts with zero prior writes rather than a
// partially-applied migration.
func PreflightWrite(plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("nil plan")
	}
	return preflightWrite(plan.TargetPaths, plan.Files)
}

// PreflightWriteAgent is the AgentPlan analogue of PreflightWrite.
func PreflightWriteAgent(plan *AgentPlan) error {
	if plan == nil {
		return fmt.Errorf("nil plan")
	}
	return preflightWrite(plan.TargetPaths, plan.Files)
}

// verifyProjectRollbackTarget rejects a project-scope rollback unless the
// entry recorded the project it was installed into AND the current
// working directory is that same project — R06 containment. Legacy
// entries (recorded before ManifestEntry.ProjectRoot existed, so it
// decodes empty) and CWD mismatches both fail closed rather than letting
// Rollback re-derive a destination purely from CWD, which can delete a
// different project's files while dropping this entry's ledger record
// (D6: legacy project rollback fails closed until an explicit root
// binding verifies the intended files).
func verifyProjectRollbackTarget(entry ManifestEntry) error {
	if entry.ProjectRoot == "" {
		return fmt.Errorf("%w: manifest entry %s has no recorded project root (legacy project-scope entry)", ErrAmbiguousProjectRollback, entry.ID)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve current directory: %w", err)
	}
	if cwd != entry.ProjectRoot {
		return fmt.Errorf("%w: entry %s was installed from %s, current directory is %s", ErrAmbiguousProjectRollback, entry.ID, entry.ProjectRoot, cwd)
	}
	return nil
}

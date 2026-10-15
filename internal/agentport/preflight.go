package agentport

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrWriteConflict = errors.New("destination already has different content")

	ErrDestinationSymlink = errors.New("destination path contains a symlink")

	ErrLedgerUnavailable = errors.New("manifest ledger is unavailable")

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
	// Defense in depth: confirm the joined path actually stays under root
	// even though every caller is expected to pass an already-cleaned rel
	// (see txn.VerifyNoSymlinks, which shares this exact algorithm).
	rootClean := filepath.Clean(root)
	full := filepath.Join(root, filepath.FromSlash(rel))
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s escapes %s", ErrDestinationSymlink, full, root)
	}

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
	for _, loss := range plan.Loss {
		if loss.Security {
			return fmt.Errorf("agent write refused: target cannot preserve security field %s", loss.Field)
		}
	}
	return preflightWrite(plan.TargetPaths, plan.Files)
}

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

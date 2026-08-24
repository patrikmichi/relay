package agentport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Rollback reverses a manifest-recorded install/migrate: it deletes each
// file the entry recorded — verifying the on-disk sha256 still matches what
// was written, refusing to delete a file the user has since changed unless
// force is set — and then drops the manifest entry. Files that no longer
// exist are treated as already rolled back (not an error).
//
// The verify pass runs to completion BEFORE any deletion, so a single
// changed file (without --force) aborts the whole rollback with nothing
// deleted, rather than leaving a half-rolled-back skill on disk.
func Rollback(entry ManifestEntry, force bool) error {
	target, err := resolveTargetForEntry(entry)
	if err != nil {
		return err
	}

	// Kind-aware directory resolution: a skill's recorded TargetPaths are
	// relative to its own "<dir>/<name>/" subdirectory (TargetDir), while an
	// agent's are relative to the provider's directory ITSELF — every
	// shipped agent provider is layout: flat, a single "<name>.md" with no
	// per-agent containing subdirectory (AgentTargetDir; see
	// relay-standalone design §3e).
	var dir string
	if entry.Kind == KindAgent {
		dir, err = AgentTargetDir(target, entry.Scope)
	} else {
		dir, err = TargetDir(target, entry.Scope, entry.Name)
	}
	if err != nil {
		return err
	}

	for rel, wantHash := range entry.TargetPaths {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		data, err := os.ReadFile(abs)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", abs, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != wantHash && !force {
			return fmt.Errorf("file %s has changed since it was written by this entry — refusing to delete without --force", abs)
		}
	}

	for rel := range entry.TargetPaths {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", abs, err)
		}
	}
	// Only skills get their per-artifact subdirectory pruned when empty —
	// dir for a KindAgent entry is the provider's SHARED agents directory
	// (e.g. ~/.claude/agents), not a per-agent subdirectory, so it must
	// never be recursively removed as a side effect of rolling back one
	// agent (it may still hold other agents' files, and even when it
	// doesn't, it isn't this entry's to delete).
	if entry.Kind != KindAgent {
		removeEmptyDirsRecursive(dir)
	}

	return RemoveEntry(entry.ID)
}

// removeEmptyDirsRecursive removes every now-empty subdirectory under dir
// (bottom-up — e.g. a "scripts/" resource subdirectory left empty after its
// one file was deleted), and finally dir itself if it too is now empty.
// Best-effort: errors are ignored. It does NOT walk up past dir to remove
// now-empty parent directories (a provider's shared skills/ root should
// never be deleted as a side effect of rolling back one skill).
func removeEmptyDirsRecursive(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			removeEmptyDirsRecursive(filepath.Join(dir, e.Name()))
		}
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return false
	}
	return os.Remove(dir) == nil
}

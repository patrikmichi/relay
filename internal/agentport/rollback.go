package agentport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// Rollback reverses a manifest-recorded install/migrate through the same
// durable transaction engine Write/WriteAgent use: it restores each
// file's PRIOR bytes/mode from that install's own backed-up transaction
// when one is linked and still available, or removes the file when the
// install created it fresh — never merely deleting-by-hash as if a
// restore and a removal were the same operation. A legacy entry with no
// linked transaction (recorded before this engine existed, or whose
// transaction was later pruned) has no recorded prior bytes to restore
// and falls back to removal only, exactly as Rollback always behaved —
// never fabricating content that was never captured.
//
// Because this runs through txn.Begin/Stage/Persist/Apply/Commit like any
// other mutation, an interrupted rollback is itself resumable via `relay
// recover`, and a concurrent write/rollback of the same artifact is
// refused rather than raced.
//
// The verify pass (hash + symlink checks) runs to completion BEFORE any
// transaction begins, so a single changed file (without --force) aborts
// the whole rollback with nothing touched, rather than leaving a
// half-rolled-back skill on disk.
func Rollback(entry ManifestEntry, force bool) error {
	if entry.Scope == ScopeProject {
		if err := verifyProjectRollbackTarget(entry); err != nil {
			return err
		}
	}

	target, err := resolveTargetForEntry(entry)
	if err != nil {
		return err
	}

	// Kind-aware directory resolution: a skill's recorded TargetPaths are
	// relative to its own "<dir>/<name>/" subdirectory (TargetDir), while an
	// agent's are relative to the provider's directory ITSELF — every
	// shipped agent provider is layout: flat, a single "<name>.md" with no
	// per-agent containing subdirectory (see AgentTargetDir).
	var dir string
	if entry.Kind == KindAgent {
		dir, err = AgentTargetDir(target, entry.Scope)
	} else {
		dir, err = TargetDir(target, entry.Scope, entry.Name)
	}
	if err != nil {
		return err
	}

	// Refuse to act through a symlink anywhere under the resolved target —
	// An owned file, or a directory it lives in, could be replaced
	// with a symlink so this verify/restore pass reads or removes something
	// outside dir instead. Checked before the verify pass so a single
	// symlinked entry aborts the whole rollback with nothing touched.
	for rel := range entry.TargetPaths {
		if err := verifyPathHasNoSymlinks(dir, rel); err != nil {
			return err
		}
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

	priorJournal, err := loadPriorJournal(entry)
	if err != nil {
		return fmt.Errorf("rollback %s: %w", entry.ID, err)
	}

	lockKey := rollbackLockKey(entry)
	ctx, cancel := context.WithTimeout(context.Background(), txnLockAcquireTimeout)
	defer cancel()
	tx, err := txn.Begin(ctx, fmt.Sprintf("rollback %s (%s/%s)", entry.Name, entry.Provider, entry.Scope), []string{lockKey})
	if err != nil {
		return err
	}

	for rel := range entry.TargetPaths {
		if prior, ok := priorFileState(priorJournal, rel); ok && prior.Existed {
			oldBytes, rerr := readJournalBackup(priorJournal, prior)
			if rerr != nil {
				return tx.Discard(fmt.Errorf("read prior content for %s: %w", rel, rerr))
			}
			if serr := tx.Stage(dir, rel, oldBytes, prior.OldMode); serr != nil {
				return tx.Discard(serr)
			}
			continue
		}
		if serr := tx.StageRemoval(dir, rel); serr != nil {
			return tx.Discard(serr)
		}
	}

	if err := tx.Persist(); err != nil {
		return tx.Discard(err)
	}
	if err := tx.Apply(verifyPathHasNoSymlinks); err != nil {
		return tx.Discard(err)
	}

	return tx.Commit(func() error {
		// Only skills get their per-artifact subdirectory pruned when empty
		// — dir for a KindAgent entry is the provider's SHARED agents
		// directory (e.g. ~/.claude/agents), not a per-agent subdirectory,
		// so it must never be recursively removed as a side effect of
		// rolling back one agent. Best-effort and only removes directories
		// actually left empty, so it never touches a restored file.
		if entry.Kind != KindAgent {
			removeEmptyDirsRecursive(dir)
		}
		return RemoveEntry(entry.ID)
	})
}

// rollbackLockKey mirrors the exact identity skillLockKey/agentLockKey use
// at write time, so a write and a rollback of the same artifact can never
// interleave.
func rollbackLockKey(entry ManifestEntry) string {
	if entry.Kind == KindAgent {
		return agentLockKey(entry.Provider, entry.Scope, entry.Name, entry.ProjectRoot)
	}
	return skillLockKey(entry.Provider, entry.Scope, entry.Name, entry.ProjectRoot)
}

// loadPriorJournal resolves the transaction that produced entry, if one is
// linked and still readable. A missing TransactionID (a legacy entry) or a
// journal that's since been pruned both return (nil, nil) — not an error —
// since a legacy/no-longer-available backup
// means "nothing recorded to restore," not "cannot proceed." A journal
// written by a future schema this build can't parse fails closed instead,
// rather than guessing at its shape.
func loadPriorJournal(entry ManifestEntry) (*txn.Journal, error) {
	if entry.TransactionID == "" {
		return nil, nil
	}
	j, err := txn.Load(entry.TransactionID)
	if err != nil {
		if errors.Is(err, txn.ErrFutureSchema) {
			return nil, err
		}
		return nil, nil
	}
	return j, nil
}

// priorFileState finds rel's recorded state within j (nil-safe: a nil
// journal — no linked/available transaction — always reports not found,
// so callers fall back to plain removal).
func priorFileState(j *txn.Journal, rel string) (txn.FileState, bool) {
	if j == nil {
		return txn.FileState{}, false
	}
	for _, fs := range j.Files {
		if fs.Rel == rel {
			return fs, true
		}
	}
	return txn.FileState{}, false
}

// readJournalBackup reads and integrity-checks the backup bytes j recorded
// for prior — a corrupt backup refuses to be used as a restore source
// rather than silently writing back damaged content.
func readJournalBackup(j *txn.Journal, prior txn.FileState) ([]byte, error) {
	dir, err := txn.Dir(j.ID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, prior.BackupPath))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != prior.OldHash {
		return nil, fmt.Errorf("backup for %s is corrupt (hash mismatch)", prior.Dest)
	}
	return data, nil
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

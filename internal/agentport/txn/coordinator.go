package txn

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrPending means a prior operation touching one of the same lock keys
// left a non-terminal journal behind (crash, kill, disk-full mid-apply).
// Begin refuses to start a new, possibly-overlapping operation until that
// journal is resolved via Recover: an interrupted journal must
// be noticed before another conflicting mutation starts, not silently
// raced.
var ErrPending = errors.New("a prior operation on this artifact did not finish and needs recovery")

// Tx is one in-flight transaction: a held LockSet plus the journal
// describing it. Callers Stage every file, Persist the journal (durable,
// before any destination write), Apply the writes, then Commit alongside
// their own ledger update. Discard aborts before any destination write was
// attempted (e.g. a post-lock preflight recheck failed).
type Tx struct {
	j     *Journal
	locks *LockSet
	dir   string
	// verify is the TOCTOU symlink-containment check Apply re-runs before
	// every write, captured here so compensate/restoreOne — invoked later
	// by Discard, a failed Commit, or Recover — apply the exact same check
	// rather than trusting the journal's recorded Dest blindly.
	verify func(root, rel string) error
}

// ID returns the transaction's journal id, for error messages pointing at
// `relay recover <id>`.
func (t *Tx) ID() string { return t.j.ID }

// Begin acquires every lock key (sorted, deterministic order) and refuses
// to proceed if a non-terminal journal already claims one of them. It
// checks for a pending journal both before AND after acquiring the locks:
// the second check is what actually matters for correctness (it runs
// under exclusive possession of every key, so nothing else can be
// concurrently creating a conflicting journal at that point); the first
// check is only a cheap way to fail fast without waiting on a lock that a
// pending-recovery situation will hold indefinitely.
func Begin(ctx context.Context, op string, lockKeys []string) (*Tx, error) {
	sorted := append([]string(nil), lockKeys...)

	if err := checkPending(sorted); err != nil {
		return nil, err
	}

	locks, err := AcquireAll(ctx, sorted)
	if err != nil {
		return nil, err
	}
	if err := checkPending(sorted); err != nil {
		locks.Unlock()
		return nil, err
	}

	id, err := newID()
	if err != nil {
		locks.Unlock()
		return nil, err
	}
	dir, err := Dir(id)
	if err != nil {
		locks.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "backup"), 0o700); err != nil {
		locks.Unlock()
		return nil, fmt.Errorf("create transaction backup dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "staged"), 0o700); err != nil {
		locks.Unlock()
		return nil, fmt.Errorf("create transaction staging dir: %w", err)
	}

	return &Tx{
		j: &Journal{
			Schema:    schemaVersion,
			ID:        id,
			Op:        op,
			LockKeys:  sorted,
			CreatedAt: time.Now().UTC(),
		},
		locks: locks,
		dir:   dir,
	}, nil
}

func checkPending(keys []string) error {
	pending, err := Pending(keys)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	return fmt.Errorf("%w: journal %s (op %q, state %s) — run `relay recover %s`", ErrPending, pending[0].ID, pending[0].Op, pending[0].State, pending[0].ID)
}

// Stage records dest's (root+rel) prior bytes/mode into a private backup
// file and writes newData into a private staged file — both durable on
// disk before Persist writes the journal describing them, and long before
// Apply touches dest itself. It refuses a destination that is itself a
// symlink; Apply re-verifies the full path (including parent components)
// immediately before writing, since time passes between Stage and Apply.
func (t *Tx) Stage(root, rel string, newData []byte, newMode fs.FileMode) error {
	dest := filepath.Join(root, filepath.FromSlash(rel))
	idx := len(t.j.Files)
	fstate := FileState{Root: root, Rel: rel, Dest: dest, NewMode: newMode, NewHash: sha256Hex(newData)}

	info, err := os.Lstat(dest)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("stage %s: destination is a symlink", dest)
	case err == nil && info.Mode().IsRegular():
		old, rerr := os.ReadFile(dest)
		if rerr != nil {
			return fmt.Errorf("read existing %s: %w", dest, rerr)
		}
		backupRel := filepath.Join("backup", fmt.Sprintf("%04d", idx))
		if werr := writeFileDurable(filepath.Join(t.dir, backupRel), old, 0o600); werr != nil {
			return werr
		}
		fstate.Existed = true
		fstate.OldMode = info.Mode().Perm()
		fstate.OldHash = sha256Hex(old)
		fstate.BackupPath = backupRel
	case err == nil:
		return fmt.Errorf("stage %s: existing destination is not a regular file", dest)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("stat %s: %w", dest, err)
	}

	stagedRel := filepath.Join("staged", fmt.Sprintf("%04d", idx))
	if err := writeFileDurable(filepath.Join(t.dir, stagedRel), newData, 0o600); err != nil {
		return err
	}
	fstate.StagedPath = stagedRel
	t.j.Files = append(t.j.Files, fstate)
	return nil
}

// StageRemoval records that root/rel should be deleted rather than
// replaced — Rollback's mechanism for undoing a fresh install (there is
// no "new content," only whatever was there before this transaction,
// which may be nothing). Backing up the current bytes first (when the
// destination exists) means an interrupted or ledger-failed removal
// compensates exactly like a replacement's would: restore the backup.
// A destination that doesn't exist yet is a valid no-op target — Apply
// will find nothing to remove and mark it applied anyway.
func (t *Tx) StageRemoval(root, rel string) error {
	dest := filepath.Join(root, filepath.FromSlash(rel))
	idx := len(t.j.Files)
	fstate := FileState{Root: root, Rel: rel, Dest: dest, Remove: true}

	info, err := os.Lstat(dest)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("stage removal %s: destination is a symlink", dest)
	case err == nil && info.Mode().IsRegular():
		old, rerr := os.ReadFile(dest)
		if rerr != nil {
			return fmt.Errorf("read existing %s: %w", dest, rerr)
		}
		backupRel := filepath.Join("backup", fmt.Sprintf("%04d", idx))
		if werr := writeFileDurable(filepath.Join(t.dir, backupRel), old, 0o600); werr != nil {
			return werr
		}
		fstate.Existed = true
		fstate.OldMode = info.Mode().Perm()
		fstate.OldHash = sha256Hex(old)
		fstate.BackupPath = backupRel
	case err == nil:
		return fmt.Errorf("stage removal %s: existing destination is not a regular file", dest)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("stat %s: %w", dest, err)
	}

	t.j.Files = append(t.j.Files, fstate)
	return nil
}

// Persist durably writes the prepared journal — the point after which an
// interrupted process leaves something `relay recover` can find and act
// on. Must be called after every Stage and before Apply.
func (t *Tx) Persist() error {
	if t.j.State == "" {
		t.j.State = StatePrepared
	}
	return t.j.save()
}

// Apply writes every not-yet-applied staged file to its destination,
// re-verifying the destination path (via verify, normally
// agentport.verifyPathHasNoSymlinks) immediately before each write — the
// same TOCTOU guard the write engines already apply, invoked here rather
// than duplicated. Each file's Applied flag is persisted immediately after
// it lands, so a crash mid-loop leaves an accurate record of exactly which
// destinations were actually touched. On error, the caller must call
// Discard to compensate whatever was applied before returning.
func (t *Tx) Apply(verify func(root, rel string) error) error {
	t.verify = verify
	t.j.State = StateApplying
	if err := t.j.save(); err != nil {
		return err
	}
	for i := range t.j.Files {
		fstate := &t.j.Files[i]
		if fstate.Applied {
			continue
		}
		if err := verify(fstate.Root, fstate.Rel); err != nil {
			return fmt.Errorf("apply %s: %w", fstate.Dest, err)
		}
		if fstate.Remove {
			if err := os.Remove(fstate.Dest); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", fstate.Dest, err)
			}
			fstate.Applied = true
			if err := t.j.save(); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(t.dir, fstate.StagedPath))
		if err != nil {
			return fmt.Errorf("read staged content for %s: %w", fstate.Dest, err)
		}
		if err := os.MkdirAll(filepath.Dir(fstate.Dest), 0o755); err != nil {
			return fmt.Errorf("create dir for %s: %w", fstate.Dest, err)
		}
		if err := atomicWrite(fstate.Dest, data, fstate.NewMode); err != nil {
			return fmt.Errorf("write %s: %w", fstate.Dest, err)
		}
		fstate.Applied = true
		if err := t.j.save(); err != nil {
			return err
		}
	}
	return nil
}

// Commit runs ledgerFn (the caller's manifest/ledger update) and only
// reports success once it too has succeeded — a mutation is never
// considered done until the destinations AND the ledger agree.
// If ledgerFn fails, Commit compensates every applied file back to its
// prior state, exactly like Discard would after a failed Apply, so a
// ledger failure never leaves committed-looking files with no record of
// them.
func (t *Tx) Commit(ledgerFn func() error) error {
	defer t.locks.Unlock()

	if err := ledgerFn(); err != nil {
		if cerr := t.compensate(); cerr != nil {
			t.j.State = StateRecovery
			t.j.Err = fmt.Sprintf("ledger update failed (%v) and compensation also failed (%v)", err, cerr)
			_ = t.j.save()
			return fmt.Errorf("ledger update failed and files could not be fully restored — run `relay recover %s`: %w", t.j.ID, err)
		}
		t.j.State = StateRestored
		_ = t.j.save()
		return fmt.Errorf("ledger update failed, all files restored to their prior state: %w", err)
	}

	t.j.State = StateCommitted
	return t.j.save()
}

// Discard compensates whatever Apply managed to write (if anything — a
// no-op if Apply was never called or wrote nothing) and releases locks.
// Use this after Persist/Apply fails, or after a post-lock preflight
// recheck rejects the operation before Stage/Apply ever ran.
func (t *Tx) Discard(applyErr error) error {
	defer t.locks.Unlock()

	if t.j.State == "" {
		// Persist was never called — nothing durable exists to clean up.
		return applyErr
	}

	if cerr := t.compensate(); cerr != nil {
		t.j.State = StateRecovery
		t.j.Err = fmt.Sprintf("%v (compensation also failed: %v)", applyErr, cerr)
		_ = t.j.save()
		return fmt.Errorf("%w: run `relay recover %s`: %v", ErrPending, t.j.ID, applyErr)
	}
	t.j.State = StateRestored
	_ = t.j.save()
	return applyErr
}

// compensate restores every Applied file to its pre-transaction state
// (removing it if it didn't exist before, restoring backup bytes/mode if
// it did) and clears each Applied flag as it succeeds, so a retried
// compensation after a partial failure only touches what's still
// outstanding. It keeps going after one file's compensation fails so a
// single stubborn file doesn't block restoring the rest; it returns a
// combined error only if at least one file could not be restored.
func (t *Tx) compensate() error {
	var errs []error
	for i := range t.j.Files {
		fstate := &t.j.Files[i]
		if !fstate.Applied {
			// A crash between atomicWrite landing and the Applied=true save's
			// rename completing leaves this flag false even though the
			// destination already holds the new bytes — reconcile against
			// reality before trusting the persisted flag, or compensation
			// would silently skip a file that actually needs restoring.
			if !reconcileApplied(fstate) {
				continue
			}
		}
		if err := restoreOne(t.dir, fstate, t.verify); err != nil {
			errs = append(errs, err)
			continue
		}
		fstate.Applied = false
	}
	_ = t.j.save()
	return errors.Join(errs...)
}

// reconcileApplied reports whether dest's current content already matches
// the staged write's NewHash despite Applied being false — the crash
// window between atomicWrite completing and the following journal save
// landing. Removal entries have no "new content" to compare against, so
// they're left to the ordinary Applied bookkeeping.
func reconcileApplied(fstate *FileState) bool {
	if fstate.Remove || fstate.NewHash == "" {
		return false
	}
	data, err := os.ReadFile(fstate.Dest)
	if err != nil {
		return false
	}
	return sha256Hex(data) == fstate.NewHash
}

// restoreOne re-verifies symlink containment immediately before touching
// fstate.Dest — the same TOCTOU guard Apply applies before every write,
// applied here too so a symlink swapped in during the window between a
// successful Apply and a later Discard/Commit-failure/Recover can't turn
// compensation into an arbitrary write/delete outside the artifact tree.
func restoreOne(dir string, fstate *FileState, verify func(root, rel string) error) error {
	if verify != nil {
		if err := verify(fstate.Root, fstate.Rel); err != nil {
			return fmt.Errorf("compensate %s: %w", fstate.Dest, err)
		}
	}
	if !fstate.Existed {
		if err := os.Remove(fstate.Dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", fstate.Dest, err)
		}
		return nil
	}
	backup, err := os.ReadFile(filepath.Join(dir, fstate.BackupPath))
	if err != nil {
		return fmt.Errorf("read backup for %s: %w", fstate.Dest, err)
	}
	if sha256Hex(backup) != fstate.OldHash {
		return fmt.Errorf("backup for %s is corrupt (hash mismatch)", fstate.Dest)
	}
	if err := atomicWrite(fstate.Dest, backup, fstate.OldMode); err != nil {
		return fmt.Errorf("restore %s: %w", fstate.Dest, err)
	}
	return nil
}

// Recover inspects one journal by id and completes its recovery: a
// Prepared journal (nothing ever applied) is simply marked Restored; an
// Applying/Recovery journal is compensated the same way Discard/Commit
// would. It is idempotent — recovering an already-terminal journal is a
// no-op that returns its current state, and re-running Recover after a
// partially-successful compensation only retries what's still Applied.
func Recover(ctx context.Context, id string) (Journal, error) {
	if err := validateID(id); err != nil {
		return Journal{}, err
	}
	j, err := Load(id)
	if err != nil {
		return Journal{}, err
	}
	if j.State.terminal() {
		return *j, nil
	}

	dir, err := Dir(id)
	if err != nil {
		return Journal{}, err
	}

	locks, err := AcquireAll(ctx, j.LockKeys)
	if err != nil {
		return Journal{}, fmt.Errorf("recover %s: %w", id, err)
	}
	defer locks.Unlock()

	// Reload after acquiring locks in case a concurrent recover attempt
	// (or the original process, if it was merely slow rather than dead)
	// changed the journal while we waited.
	j, err = Load(id)
	if err != nil {
		return Journal{}, err
	}
	if j.State.terminal() {
		return *j, nil
	}

	// VerifyNoSymlinks is the same containment algorithm every caller's own
	// verify function (e.g. agentport.verifyPathHasNoSymlinks) implements —
	// Recover has no caller-supplied verify to thread through, so it uses
	// this package's own copy directly rather than skipping the check.
	t := &Tx{j: j, dir: dir, verify: VerifyNoSymlinks}
	if err := t.compensate(); err != nil {
		j.State = StateRecovery
		j.Err = err.Error()
		_ = j.save()
		return *j, fmt.Errorf("recover %s: %w", id, err)
	}
	j.State = StateRestored
	if err := j.save(); err != nil {
		return *j, err
	}
	return *j, nil
}

// RecoverAll recovers every non-terminal journal on disk — `relay
// recover` with no id argument.
func RecoverAll(ctx context.Context) ([]Journal, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var out []Journal
	var errs []error
	for _, j := range all {
		if j.State.terminal() {
			continue
		}
		result, err := Recover(ctx, j.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, result)
	}
	return out, errors.Join(errs...)
}

// atomicWrite writes data to a unique temp file in dest's own directory
// (never a shared predictable name) with mode already applied,
// fsyncs it, then renames over dest. Rename within the same directory is
// atomic on every filesystem relay supports, so a reader never observes a
// partially-written destination file.
func atomicWrite(dest string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".relay-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(name)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(name, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, dest); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", name, dest, err)
	}
	cleanup = false
	return nil
}

// writeFileDurable writes data to path and fsyncs before returning —
// backup/staged bytes must survive a crash immediately after Stage
// returns, since Persist (and therefore crash-safety of the whole
// journal) depends on them already being on disk.
func writeFileDurable(path string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return f.Close()
}

func newID() (string, error) {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate transaction id: %w", err)
	}
	return fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(b[:])), nil
}

// Package txn implements the durable transaction/recovery primitive
// underneath agentport's write engines: preflight -> staged -> prepared
// journal -> applying -> committed, with a recovery path back to
// restored/committed if a process dies mid-operation. It knows nothing about
// skills, agents, or providers — only destination file paths, bytes, and
// modes — so no secret ever has a reason to pass through it.
package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// State is one node of the transaction state machine. It is stored on disk, so
// renaming a value would break in-flight recovery across a binary
// upgrade — treat these as a wire format.
type State string

const (
	StatePrepared  State = "prepared"  // journal durably written; no destination touched yet
	StateApplying  State = "applying"  // at least one destination write has been attempted
	StateCommitted State = "committed" // every destination write and the ledger update finished
	StateRecovery  State = "recovery"  // interrupted mid-apply and compensation did not (yet) succeed
	StateRestored  State = "restored"  // interrupted, then successfully compensated back to prior state
)

// terminal reports whether state needs no further action from `relay
// recover` — it's either fully done (Committed) or fully undone (Restored).
func (s State) terminal() bool { return s == StateCommitted || s == StateRestored }

// schemaVersion is bumped only if the on-disk Journal shape changes
// incompatibly. A journal written by a newer relay binary must never be
// silently misinterpreted by an older one — CurrentSchema refuses to load
// anything newer than itself (the same downgrade-safety rule the manifest
// ledger follows).
const schemaVersion = 1

// ErrFutureSchema means the journal was written by a newer relay version.
var ErrFutureSchema = errors.New("transaction journal uses a newer schema than this relay build supports")

// ErrInvalidID means a transaction id doesn't match newID()'s own output
// format. Every id-taking entry point in this package rejects anything
// else before it touches a path — newID() produces "<unixnano>-<16 hex
// chars>", never anything containing a path separator, so a caller-
// supplied id (CLI argument, or round-tripped through a manifest entry's
// TransactionID) that doesn't match can only be an attempt to traverse
// outside the transactions directory.
var ErrInvalidID = errors.New("invalid transaction id")

var idPattern = regexp.MustCompile(`^[0-9]+-[0-9a-f]{16}$`)

// validateID rejects any id that doesn't match newID()'s exact output
// shape — the containment check for every id-taking function in this
// package (Dir, journalPath, Load, Prune, Recover).
func validateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

// FileState is the recorded fate of one destination file within a
// transaction: what was there before (for compensation), what's staged to
// replace it, and whether that replacement has actually landed yet.
type FileState struct {
	Root    string      `json:"root"`    // the artifact's owning directory (matches verifyPathHasNoSymlinks' root arg)
	Rel     string      `json:"rel"`     // path relative to Root
	Dest    string      `json:"dest"`    // absolute destination path — filepath.Join(Root, Rel)
	Existed bool        `json:"existed"` // did dest already exist before this operation
	OldMode fs.FileMode `json:"oldMode,omitempty"`
	OldHash string      `json:"oldHash,omitempty"` // sha256 of prior bytes, verified before restoring
	// BackupPath and StagedPath are relative to the transaction's own
	// directory (Dir()) — never the destination tree — so recovery never
	// needs anything but the journal id to find them.
	BackupPath string      `json:"backupPath,omitempty"` // prior bytes; empty when !Existed
	StagedPath string      `json:"stagedPath,omitempty"` // new bytes, persisted before any write; empty when Remove
	NewMode    fs.FileMode `json:"newMode,omitempty"`
	NewHash    string      `json:"newHash,omitempty"`
	Applied    bool        `json:"applied"` // set once this specific file's write/removal has landed
	// Remove marks this entry as a deletion rather than a replacement — the
	// mechanism Rollback uses to undo a fresh install's files (there is no
	// "new content" to stage, only a prior state to either delete down to
	// or restore back to). Compensating an applied removal restores the
	// same backup bytes a replacement's compensation would.
	Remove bool `json:"remove,omitempty"`
}

// Journal is the durable record of one mutation. Op is a short human label
// ("skill install", "agent migrate to codex") for `relay history`/`relay
// recover` output — never a secret, never file content.
type Journal struct {
	Schema    int         `json:"schema"`
	ID        string      `json:"id"`
	Op        string      `json:"op"`
	LockKeys  []string    `json:"lockKeys"`
	State     State       `json:"state"`
	Files     []FileState `json:"files"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
	// Err carries the last failure's message once State is Recovery, so
	// `relay recover`/`relay history` can explain why an operation needed
	// manual attention without re-deriving it.
	Err string `json:"err,omitempty"`
}

// baseDir returns ~/.config/relay/transactions, creating it if needed.
// Backup/staged bytes under <baseDir>/<id>/{backup,staged}/* persist at
// 0600 indefinitely until an operator runs `relay history prune` — an
// accepted tradeoff (recovery data must outlive the process that wrote it),
// not an oversight.
func baseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "relay", "transactions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create transactions dir %s: %w", dir, err)
	}
	return dir, nil
}

// Dir returns the private directory holding id's backup/staged file
// bytes.
func Dir(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, id), nil
}

func journalPath(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, id+".json"), nil
}

// save persists j atomically: unique temp file + rename, mirroring
// agentport's manifest save discipline (never a shared predictable
// "<path>.tmp" two writers could both open).
func (j *Journal) save() error {
	j.UpdatedAt = time.Now().UTC()
	path, err := journalPath(j.ID)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal journal: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "journal-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp journal file: %w", err)
	}
	name := tmp.Name()
	if _, werr := tmp.Write(raw); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write temp journal file: %w", werr)
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("sync temp journal file: %w", serr)
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close temp journal file: %w", cerr)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("rename journal file: %w", err)
	}
	return nil
}

// Load reads one journal by id and refuses to interpret an unknown future
// schema rather than best-effort parsing it.
func Load(id string) (*Journal, error) {
	path, err := journalPath(id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read journal %s: %w", id, err)
	}
	var j Journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("parse journal %s: %w", id, err)
	}
	if j.Schema > schemaVersion {
		return nil, fmt.Errorf("%w: journal %s has schema %d, this build supports up to %d", ErrFutureSchema, id, j.Schema, schemaVersion)
	}
	return &j, nil
}

// List returns every journal on disk, oldest first, for `relay history`.
// A journal with a schema this build doesn't understand is still listed
// (id, and an error note) rather than silently omitted — an operator must
// be able to see that something is pending even if this binary can't act
// on it.
func List() ([]Journal, error) {
	base, err := baseDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read transactions dir: %w", err)
	}
	var out []Journal
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		j, err := Load(id)
		if err != nil {
			out = append(out, Journal{ID: id, Op: fmt.Sprintf("(unreadable: %v)", err)})
			continue
		}
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })
	return out, nil
}

// Pending returns every non-terminal journal whose LockKeys intersect any
// of keys — used to refuse starting a new operation that overlaps one a
// prior process left interrupted, independent of whether that
// process's flock was released by its crash.
func Pending(keys []string) ([]Journal, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	var out []Journal
	for _, j := range all {
		if j.State.terminal() {
			continue
		}
		for _, k := range j.LockKeys {
			if want[k] {
				out = append(out, j)
				break
			}
		}
	}
	return out, nil
}

// Prune permanently deletes a terminal journal and its backup/staged
// bytes. It refuses a Prepared/Applying/Recovery journal outright — that
// is the one copy of a pending operation's recovery data, and pruning it
// would make an interrupted operation permanently unrecoverable.
func Prune(id string) error {
	j, err := Load(id)
	if err != nil {
		return err
	}
	if !j.State.terminal() {
		return fmt.Errorf("refusing to prune journal %s: state %q is not committed or restored (run `relay recover %s` first)", id, j.State, id)
	}
	dir, err := Dir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove transaction data %s: %w", dir, err)
	}
	path, err := journalPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove journal %s: %w", path, err)
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

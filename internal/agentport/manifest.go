package agentport

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ArtifactKind discriminates which canonical IR a ManifestEntry records —
// Skill (the only kind that existed before this field) or Agent
// (relay-standalone design §3e). A single manifest file holds entries of
// both kinds, discriminated by this field, rather than a parallel ledger —
// simpler and preserves the one-lock invariant (withManifestLock).
type ArtifactKind string

const (
	KindSkill ArtifactKind = "skill"
	KindAgent ArtifactKind = "agent"
)

// ManifestEntry records one migrate/install operation for later
// uninstall/rollback. ID is assigned automatically by RecordEntry if left
// empty — it's how `relay skill rollback <id>` (and RemoveEntry/FindEntry)
// address a specific entry.
type ManifestEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind defaults to KindSkill when absent from on-disk JSON — every
	// manifest entry recorded before this field existed is a skill entry,
	// and LoadManifest normalizes an empty Kind to KindSkill on read so
	// those pre-existing agentport-manifest.json files keep decoding and
	// resolving exactly as before (back-compat; see normalizeManifestKinds).
	Kind           ArtifactKind      `json:"kind,omitempty"`
	Provider       ProviderID        `json:"provider"` // target provider
	Scope          Scope             `json:"scope"`
	SourceProvider ProviderID        `json:"sourceProvider"`
	TargetPaths    map[string]string `json:"targetPaths"` // relative file path -> sha256 hex digest
	Timestamp      time.Time         `json:"timestamp"`
	Provenance     Provenance        `json:"provenance"`
}

// Manifest is the on-disk ledger shape: a flat, append-only list of entries.
type Manifest struct {
	Entries []ManifestEntry `json:"entries"`
}

// normalizeManifestKinds sets Kind to KindSkill on every entry whose Kind
// is empty — the default-skill decode rule for manifest entries recorded
// before Kind existed. Called once, right after unmarshaling, so every
// other function in this package can rely on Kind always being populated.
func normalizeManifestKinds(m *Manifest) {
	for i := range m.Entries {
		if m.Entries[i].Kind == "" {
			m.Entries[i].Kind = KindSkill
		}
	}
}

// manifestPath returns ~/.config/relay/agentport-manifest.json, creating the
// parent directory if necessary.
func manifestPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "relay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create config dir %s: %w", dir, err)
	}
	return filepath.Join(dir, "agentport-manifest.json"), nil
}

// LoadManifest reads the ledger, returning an empty Manifest (not an error)
// if the file doesn't exist yet.
func LoadManifest() (Manifest, error) {
	path, err := manifestPath()
	if err != nil {
		return Manifest{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, nil
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	normalizeManifestKinds(&m)
	return m, nil
}

// SaveManifest writes the ledger atomically: marshal to a uniquely-named
// temp file in the manifest's directory (os.CreateTemp — never a shared
// "<path>.tmp" name, which two racing writers could both open/truncate),
// then rename it over the real path. Rename is atomic on the same
// filesystem, so a reader never observes a partially-written manifest.
//
// This alone does not make concurrent RecordEntry/RemoveEntry/
// RemoveEntriesFor calls race-safe end to end — see withManifestLock, which
// wraps their whole load-mutate-save cycle.
func SaveManifest(m Manifest) error {
	path, err := manifestPath()
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, "manifest-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp manifest file: %w", err)
	}
	tmp := tmpFile.Name()
	if _, werr := tmpFile.Write(raw); werr != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp manifest file: %w", werr)
	}
	if cerr := tmpFile.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp manifest file: %w", cerr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename manifest file: %w", err)
	}
	return nil
}

// manifestLockPath returns the advisory lock file path sitting alongside the
// manifest (e.g. ~/.config/relay/agentport-manifest.json.lock).
func manifestLockPath() (string, error) {
	path, err := manifestPath()
	if err != nil {
		return "", err
	}
	return path + ".lock", nil
}

const (
	lockRetryInterval  = 25 * time.Millisecond
	lockStaleTimeout   = 10 * time.Second
	lockAcquireTimeout = 5 * time.Second
)

// withManifestLock runs fn while holding an exclusive, advisory, cross-
// platform file lock on the manifest, serializing concurrent
// load-mutate-save cycles (RecordEntry/RemoveEntry/RemoveEntriesFor all do
// LoadManifest -> mutate -> SaveManifest with no lock otherwise) so two
// racing callers can't silently clobber each other's writes — the second
// SaveManifest to finish would otherwise overwrite the first writer's
// change with a Manifest snapshot that never saw it, losing an entry.
//
// The lock is a plain file created with O_CREATE|O_EXCL: whichever caller
// creates it first holds the lock; others retry on a short interval until
// it's released (removed) or judged stale (a crashed process never removes
// its lock file, so without a staleness check a single crash would wedge
// every future manifest write forever) and steal it. This is intentionally
// NOT flock/unix-specific — a lock *file*, not an OS-level advisory lock —
// so it needs no build tags and works identically on every platform relay
// ships to.
func withManifestLock(fn func() error) error {
	lockPath, err := manifestLockPath()
	if err != nil {
		return err
	}

	deadline := time.Now().Add(lockAcquireTimeout)
	for {
		f, openErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr == nil {
			_ = f.Close()
			break
		}
		if !errors.Is(openErr, os.ErrExist) {
			return fmt.Errorf("create manifest lock %s: %w", lockPath, openErr)
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > lockStaleTimeout {
			// The lock's holder crashed (or otherwise never released it) —
			// steal it rather than wedging every future manifest write.
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for manifest lock %s", lockPath)
		}
		time.Sleep(lockRetryInterval)
	}
	defer os.Remove(lockPath)

	return fn()
}

// RecordEntry appends entry to the ledger and persists it. If entry.ID is
// empty, a unique one is assigned first. If entry.Kind is empty, it
// defaults to KindSkill — mirrors normalizeManifestKinds' read-side default
// so a caller that predates the Kind field (every skill call site today)
// keeps recording exactly the entries it always has.
func RecordEntry(entry ManifestEntry) error {
	if entry.ID == "" {
		entry.ID = newEntryID()
	}
	if entry.Kind == "" {
		entry.Kind = KindSkill
	}
	return withManifestLock(func() error {
		m, err := LoadManifest()
		if err != nil {
			return err
		}
		m.Entries = append(m.Entries, entry)
		return SaveManifest(m)
	})
}

// newEntryID returns a unique manifest entry id: a nanosecond timestamp
// plus 4 random bytes (collision-proof enough for a local, single-machine,
// append-only ledger; no need for a full UUID dependency).
func newEntryID() string {
	var b [4]byte
	_, _ = crand.Read(b[:])
	return fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(b[:]))
}

// FindEntry returns the entry with the given id, or ok=false if none matches.
func FindEntry(m Manifest, id string) (ManifestEntry, bool) {
	for _, e := range m.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return ManifestEntry{}, false
}

// LastEntry returns the most recently recorded entry (the ledger is
// append-only, so this is simply the last element), or ok=false if the
// manifest has no entries.
func LastEntry(m Manifest) (ManifestEntry, bool) {
	if len(m.Entries) == 0 {
		return ManifestEntry{}, false
	}
	return m.Entries[len(m.Entries)-1], true
}

// LastEntryOfKind returns the most recently recorded entry whose Kind
// matches kind (scanning from the end), or ok=false if none match. Used by
// `relay agent rollback --last` so it rolls back the most recent AGENT
// entry specifically, never a skill entry that happens to sort later in
// the single shared ledger (relay-standalone design §3e: one manifest file
// holds both kinds).
func LastEntryOfKind(m Manifest, kind ArtifactKind) (ManifestEntry, bool) {
	for i := len(m.Entries) - 1; i >= 0; i-- {
		if m.Entries[i].Kind == kind {
			return m.Entries[i], true
		}
	}
	return ManifestEntry{}, false
}

// LastEntryFor returns the most recent entry matching name+provider+scope+
// kind (scanning from the end), or ok=false if none match. Used by
// `relay skill list --provenance` to join manifest provenance onto
// discovered skills. kind-aware since P1.5: a skill entry and an agent
// entry can legitimately share name+provider+scope (e.g. a "reviewer"
// skill and a "reviewer" agent both targeting claude/user) without one
// masking the other.
func LastEntryFor(m Manifest, name string, provider ProviderID, scope Scope, kind ArtifactKind) (ManifestEntry, bool) {
	for i := len(m.Entries) - 1; i >= 0; i-- {
		e := m.Entries[i]
		if e.Name == name && e.Provider == provider && e.Scope == scope && e.Kind == kind {
			return e, true
		}
	}
	return ManifestEntry{}, false
}

// RemoveEntry deletes the entry with the given id from the ledger. Returns
// an error if no entry has that id.
func RemoveEntry(id string) error {
	return withManifestLock(func() error {
		m, err := LoadManifest()
		if err != nil {
			return err
		}
		out := make([]ManifestEntry, 0, len(m.Entries))
		found := false
		for _, e := range m.Entries {
			if e.ID == id {
				found = true
				continue
			}
			out = append(out, e)
		}
		if !found {
			return fmt.Errorf("no manifest entry with id %q", id)
		}
		m.Entries = out
		return SaveManifest(m)
	})
}

// RemoveEntriesFor deletes every entry matching name+provider+scope+kind,
// returning the count removed (0 if none matched — not an error, so
// callers like `skill uninstall` can treat "no manifest record" as a
// harmless no-op rather than a failure). kind-aware since P1.5, for the
// same reason as LastEntryFor: uninstalling a skill must never remove an
// agent entry (or vice versa) that happens to share name+provider+scope.
func RemoveEntriesFor(name string, provider ProviderID, scope Scope, kind ArtifactKind) (int, error) {
	var removed int
	err := withManifestLock(func() error {
		m, err := LoadManifest()
		if err != nil {
			return err
		}
		out := make([]ManifestEntry, 0, len(m.Entries))
		for _, e := range m.Entries {
			if e.Name == name && e.Provider == provider && e.Scope == scope && e.Kind == kind {
				removed++
				continue
			}
			out = append(out, e)
		}
		if removed == 0 {
			return nil
		}
		m.Entries = out
		return SaveManifest(m)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// HashFiles computes sha256 hex digests for a set of projected files, keyed
// by their relative path — used to populate ManifestEntry.TargetPaths.
func HashFiles(files map[string][]byte) map[string]string {
	out := make(map[string]string, len(files))
	for rel, data := range files {
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
	}
	return out
}

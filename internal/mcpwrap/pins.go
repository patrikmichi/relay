package mcpwrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Pin states of a definition.
const (
	StatePinned      = "pinned"
	StatePending     = "pending"
	StateQuarantined = "quarantined"
	StateRemoved     = "removed"
	StateRejected    = "rejected"
)

// Pin is what was approved for one tool.
type Pin struct {
	Name  string     `json:"name"`
	Hash  string     `json:"hash"`
	State string     `json:"state"`
	Def   Definition `json:"definition"`
	// Seen is the hash of the definition the server last showed, when it
	// differs from the pinned one.
	Seen    string     `json:"seen,omitempty"`
	SeenDef Definition `json:"seenDefinition,omitempty"`
	At      string     `json:"at"`
}

// PinSet is the pins of one local server.
type PinSet struct {
	Name  string          `json:"name"`
	Tools map[string]*Pin `json:"tools"`
}

// ChangeRecord is a difference found between what was pinned and what the
// server showed.
type ChangeRecord struct {
	Tool     string `json:"tool"`
	Kind     string `json:"kind"` // modified, added, removed
	OldHash  string `json:"oldHash,omitempty"`
	NewHash  string `json:"newHash,omitempty"`
	Severity string `json:"severity"`
	At       string `json:"at"`
}

var serverName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName reports a name usable as a local server's name.
func ValidName(name string) bool { return serverName.MatchString(name) }

// PinStore keeps pin sets in a directory, one file per server.
type PinStore struct{ Dir string }

func (s PinStore) path(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("%q is not a usable server name", name)
	}
	return filepath.Join(s.Dir, name+".json"), nil
}

// Load reads a server's pins; a server never seen has none.
func (s PinStore) Load(name string) (*PinSet, error) {
	p, err := s.path(name)
	if err != nil {
		return nil, err
	}
	set := &PinSet{Name: name, Tools: map[string]*Pin{}}
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return set, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, set); err != nil {
		return nil, fmt.Errorf("%s is not a pin file: %w", p, err)
	}
	if set.Tools == nil {
		set.Tools = map[string]*Pin{}
	}
	return set, nil
}

// Save writes a server's pins.
func (s PinStore) Save(set *PinSet) error {
	p, err := s.path(set.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".pins-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Names lists the servers that have pins.
func (s PinStore) Names() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && ValidName(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Observe compares what the server listed with the pins, updates the set and
// returns the differences. A tool seen for the first time is pending; a pinned
// tool whose definition changed is quarantined with the new definition kept
// for review; with final set, a pinned tool the server no longer lists is
// removed.
func (set *PinSet) Observe(live []Definition, seen map[string]bool, final bool) []ChangeRecord {
	var changes []ChangeRecord
	for _, def := range live {
		name := nameOf(def)
		hash, err := DefHash(def)
		if name == "" || err != nil {
			continue
		}
		seen[name] = true
		sev := MaxSeverity(ScreenDefinition(def))
		if sev == "" {
			sev = SevInfo
		}
		pin := set.Tools[name]
		switch {
		case pin == nil || pin.State == StateRemoved:
			set.Tools[name] = &Pin{Name: name, Hash: hash, State: StatePending, Def: def, At: now()}
			changes = append(changes, ChangeRecord{Tool: name, Kind: "added", NewHash: hash, Severity: sev, At: now()})
		case pin.Hash == hash:
			pin.Seen, pin.SeenDef = "", nil
			if pin.State == StateQuarantined {
				pin.State = StatePinned
			}
		case pin.Seen == hash:
			// Already reported.
		default:
			pin.Seen, pin.SeenDef = hash, def
			if pin.State == StatePinned {
				pin.State = StateQuarantined
			}
			changes = append(changes, ChangeRecord{Tool: name, Kind: "modified", OldHash: pin.Hash, NewHash: hash, Severity: sev, At: now()})
		}
	}
	if final {
		for name, pin := range set.Tools {
			if !seen[name] && pin.State != StateRemoved && pin.State != StatePending && pin.State != StateRejected {
				pin.State = StateRemoved
				changes = append(changes, ChangeRecord{Tool: name, Kind: "removed", OldHash: pin.Hash, Severity: SevInfo, At: now()})
			}
		}
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Tool < changes[j].Tool })
	return changes
}

// Approve pins the definition the server last showed (or the pending one).
func (set *PinSet) Approve(name string) error {
	pin := set.Tools[name]
	if pin == nil {
		return fmt.Errorf("no tool %q is known", name)
	}
	if pin.SeenDef != nil {
		pin.Def, pin.Hash = pin.SeenDef, pin.Seen
		pin.Seen, pin.SeenDef = "", nil
	}
	pin.State, pin.At = StatePinned, now()
	return nil
}

// Reject keeps a tool out: a new tool is rejected, a changed one stays
// quarantined at its old pin.
func (set *PinSet) Reject(name string) error {
	pin := set.Tools[name]
	if pin == nil {
		return fmt.Errorf("no tool %q is known", name)
	}
	if pin.Seen != "" {
		pin.State = StateQuarantined
		return nil
	}
	pin.State, pin.At = StateRejected, now()
	return nil
}

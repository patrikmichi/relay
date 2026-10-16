package packs

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/patrikmichi/zuano/internal/config"
)

// AssignmentRule is a rule as delivered to a member.
type AssignmentRule struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	AlwaysApply bool     `json:"alwaysApply"`
	Globs       []string `json:"globs"`
	Agents      []string `json:"agents"`
	Body        string   `json:"body"`
	SHA256      string   `json:"sha256"`
}

// AssignmentSkill is a catalog skill pinned to a version.
type AssignmentSkill struct {
	ResourceID string `json:"resourceId"`
	VersionID  string `json:"versionId"`
	Name       string `json:"name"`
	Version    string `json:"version"`
}

// AssignmentBundle is a plugin bundle pinned to a version.
type AssignmentBundle struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Channel string `json:"channel"`
}

// Profile is what one installed pack gives a member.
type Profile struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Name   string `json:"name"`
	Pack   struct {
		Slug    string `json:"slug"`
		Version string `json:"version"`
	} `json:"pack"`
	Rules   []AssignmentRule   `json:"rules"`
	Skills  []AssignmentSkill  `json:"skills"`
	Bundles []AssignmentBundle `json:"bundles"`
}

// ConnectRequired is a connection the member still has to make.
type ConnectRequired struct {
	Slug   string `json:"slug"`
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// Assignments is the answer of the member assignments route.
type Assignments struct {
	Revision        int               `json:"revision"`
	Profiles        []Profile         `json:"profiles"`
	ConnectRequired []ConnectRequired `json:"connectRequired"`
}

// State is the last assignments a member received, kept so an unchanged
// revision costs a 304 and so removals can be told from never-delivered.
type State struct {
	Gateway     string      `json:"gateway"`
	Revision    int         `json:"revision"`
	Assignments Assignments `json:"assignments"`
}

// StatePath is where the last assignments are kept.
func StatePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync", "assignments.json"), nil
}

// LoadState reads the saved assignments for a gateway; a missing file or one
// for another gateway is an empty state.
func LoadState(path, gateway string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil || s.Gateway != gateway {
		return State{}, nil
	}
	return s, nil
}

// Save writes the state atomically.
func (s State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".assignments-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

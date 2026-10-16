package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Install is what this installation of the CLI remembers about itself. The
// id is an identifier the gateway shows next to the sessions of this machine;
// it is never a credential and grants nothing.
type Install struct {
	InstallID string `json:"installId"`
	// HostLabel overrides the machine name in device reports; HostLabelSet
	// separates an empty label chosen on purpose from none chosen.
	HostLabel      string `json:"hostLabel,omitempty"`
	HostLabelSet   bool   `json:"hostLabelSet,omitempty"`
	LastReportAt   string `json:"lastReportAt,omitempty"`
	LastReportSent int    `json:"lastReportAgents,omitempty"`
}

func installPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "install.json"), nil
}

// LoadInstall reads install.json, creating it with a new random id the first
// time.
func LoadInstall() (Install, error) {
	path, err := installPath()
	if err != nil {
		return Install{}, err
	}
	var in Install
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(raw, &in) != nil {
			in = Install{}
		}
	case !errors.Is(err, os.ErrNotExist):
		return Install{}, err
	}
	if in.InstallID != "" {
		return in, nil
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Install{}, err
	}
	in.InstallID = hex.EncodeToString(id)
	return in, SaveInstall(in)
}

// InstallID is this installation's id.
func InstallID() (string, error) {
	in, err := LoadInstall()
	return in.InstallID, err
}

// SaveInstall writes install.json.
func SaveInstall(in Install) error {
	path, err := installPath()
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// RecordReport notes that a device report was sent.
func RecordReport(agents int) error {
	in, err := LoadInstall()
	if err != nil {
		return err
	}
	in.LastReportAt, in.LastReportSent = time.Now().UTC().Format(time.RFC3339), agents
	return SaveInstall(in)
}

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const syncStateDir = ".relay-sync"

func ownedFilesPath(localDir, pluginID string) string {
	return filepath.Join(localDir, syncStateDir, pluginID+".json")
}

// loadOwnedFiles returns the relative paths the last successful sync
// wrote for pluginID, or nil (not an error) if this plugin has never been
// synced under ownership tracking — a plugin installed before this
// mechanism existed is treated as having no recorded owned set, so
// reconciliation makes no removal for it rather than fabricating a
// history that was never captured.
func loadOwnedFiles(localDir, pluginID string) ([]string, error) {
	raw, err := os.ReadFile(ownedFilesPath(localDir, pluginID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read owned-files record for %s: %w", pluginID, err)
	}
	var files []string
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("parse owned-files record for %s: %w", pluginID, err)
	}
	return files, nil
}

func saveOwnedFiles(localDir, pluginID string, files []string) error {
	dir := filepath.Join(localDir, syncStateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create sync state dir: %w", err)
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("marshal owned-files record for %s: %w", pluginID, err)
	}
	return writeFile(ownedFilesPath(localDir, pluginID), raw)
}

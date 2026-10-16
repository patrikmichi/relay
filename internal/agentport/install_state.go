package agentport

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"

	"github.com/patrikmichi/zuano/internal/brand"
)

// InstallState says who owns an artifact already present at a destination.
type InstallState string

const (
	// InstallNone: nothing is installed there.
	InstallNone InstallState = "none"
	// InstallZuano: zuano installed it and every file still matches the
	// manifest, so replacing it loses nothing the user wrote.
	InstallZuano InstallState = brand.Name
	// InstallEdited: zuano installed it but a file was edited or removed.
	InstallEdited InstallState = "edited"
	// InstallForeign: files exist that no manifest entry accounts for.
	InstallForeign InstallState = "foreign"
)

// latestEntry returns the newest manifest entry for the artifact, honouring
// the project root for project scope.
func latestEntry(name string, provider ProviderID, scope Scope, kind ArtifactKind) (ManifestEntry, bool) {
	m, err := LoadManifest()
	if err != nil {
		return ManifestEntry{}, false
	}
	for i := len(m.Entries) - 1; i >= 0; i-- {
		e := m.Entries[i]
		if e.Name != name || e.Provider != provider || e.Scope != scope || e.Kind != kind {
			continue
		}
		if scope == ScopeProject {
			if cwd, err := os.Getwd(); err != nil || e.ProjectRoot != cwd {
				continue
			}
		}
		return e, true
	}
	return ManifestEntry{}, false
}

// ClassifyInstalled compares the files in dir with the manifest. files are
// the relative paths the new projection would write; they decide whether
// anything is installed at all.
func ClassifyInstalled(name string, provider ProviderID, scope Scope, kind ArtifactKind, dir string, files map[string][]byte) InstallState {
	entry, recorded := latestEntry(name, provider, scope, kind)
	present := false
	for rel := range files {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			present = true
		}
	}
	if !recorded {
		if present {
			return InstallForeign
		}
		return InstallNone
	}
	for rel, want := range entry.TargetPaths {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return InstallEdited
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != want {
			return InstallEdited
		}
	}
	return InstallZuano
}

// droppedFiles lists files the last installation wrote that the new
// projection no longer includes.
func droppedFiles(name string, provider ProviderID, scope Scope, kind ArtifactKind, files map[string][]byte) []string {
	entry, ok := latestEntry(name, provider, scope, kind)
	if !ok {
		return nil
	}
	var out []string
	for rel := range entry.TargetPaths {
		if _, keep := files[rel]; !keep {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

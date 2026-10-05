package agentport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/patrikmichi/relay/internal/nofollow"
)

type IntegrityResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// InspectManifest checks only the newest installation for each artifact identity.
// It never rewrites artifacts or traverses a recorded path through a symlink.
func InspectManifest(m Manifest) []IntegrityResult {
	results := []IntegrityResult{}
	type identity struct {
		Kind       ArtifactKind
		Provider   ProviderID
		Scope      Scope
		Root, Name string
	}
	seen := map[identity]bool{}
	for i := len(m.Entries) - 1; i >= 0; i-- {
		entry := m.Entries[i]
		key := identity{entry.Kind, entry.Provider, entry.Scope, entry.ProjectRoot, entry.Name}
		if seen[key] {
			continue
		}
		seen[key] = true
		result := IntegrityResult{ID: entry.ID, Name: entry.Name, OK: true}
		if entry.Scope == ScopeProject {
			if err := verifyProjectRollbackTarget(entry); err != nil {
				result.Skipped = true
				result.Reason = "project installation cannot be checked from this directory"
				results = append(results, result)
				continue
			}
		}
		if err := inspectEntry(entry); err != nil {
			result.OK = false
			result.Reason = err.Error()
		}
		results = append(results, result)
	}
	return results
}

func inspectEntry(entry ManifestEntry) error {
	if (entry.Kind != KindSkill && entry.Kind != KindAgent) || len(entry.TargetPaths) == 0 {
		return fmt.Errorf("invalid installation manifest entry")
	}
	target, err := resolveTargetForEntry(entry)
	if err != nil {
		return fmt.Errorf("provider unavailable")
	}
	var dir string
	if entry.Kind == KindAgent {
		dir, err = AgentTargetDir(target, entry.Scope)
	} else {
		dir, err = TargetDir(target, entry.Scope, entry.Name)
	}
	if err != nil {
		return fmt.Errorf("invalid installation destination")
	}
	for rel, expected := range entry.TargetPaths {
		if err := verifyPathHasNoSymlinks(dir, rel); err != nil {
			return fmt.Errorf("unsafe installation path")
		}
		file, err := os.OpenFile(filepath.Join(dir, filepath.FromSlash(rel)), nofollow.ReadFlags, 0)
		if err != nil {
			return fmt.Errorf("installed file missing or unreadable: %s", rel)
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 50<<20 {
			_ = file.Close()
			return fmt.Errorf("installed file is not a bounded regular file: %s", rel)
		}
		if mode, ok := entry.FileModes[rel]; ok && uint32(info.Mode().Perm()) != mode {
			_ = file.Close()
			return fmt.Errorf("installed file permissions differ from manifest: %s", rel)
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, io.LimitReader(file, (50<<20)+1))
		_ = file.Close()
		if readErr != nil || n > 50<<20 || hex.EncodeToString(hash.Sum(nil)) != expected {
			return fmt.Errorf("installed file differs from manifest: %s", rel)
		}
	}
	return nil
}

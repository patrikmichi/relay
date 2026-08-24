package agentport

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SkillRef identifies one on-disk skill instance discovered by List.
type SkillRef struct {
	Name     string
	Provider ProviderID
	Scope    Scope
	Path     string // skill directory (containing SKILL.md), or a legacy flat <name>.md file
}

// recursiveDiscoverer is an additive, OPTIONAL capability interface (§2.4
// of the config-driven-adapters design) List uses instead of a hard-coded
// `a.(cursorAdapter)` type assertion — the one coupling to a concrete
// adapter type this package had. configAdapter implements it from its
// `discovery` config field; the Adapter interface itself is unchanged.
type recursiveDiscoverer interface {
	// DiscoversRecursively reports whether this provider's discovery for
	// the given scope should walk the full subtree for any SKILL.md,
	// rather than only looking at direct children of each search dir.
	DiscoversRecursively(scope Scope) bool
}

// List enumerates skills present for adapter a in the given scope, across
// every directory the adapter searches (UserDirs()/ProjectDirs(), in
// priority order — including any other providers' directories the adapter
// also reads for compatibility, mirroring ResolveSkillPath's search order).
// Duplicate names across directories are resolved by priority order (the
// first directory that has the name wins), same as
// ResolveSkillPath/findSkillInDirs. Returns an empty (non-nil-error) slice
// if nothing is found — not an error.
//
// ScopeProject directories are resolved relative to the current working
// directory only (unlike ResolveSkillPath, List does not walk up to a repo
// root — listing is a "what's here" operation, not a single-skill lookup).
//
// Cursor's project scope is discovered recursively — skills can live
// nested anywhere under .cursor/skills/ (foo/bar/<name>/SKILL.md); identity
// is the SKILL.md's own directory name, matching how the Cursor adapter's
// Load validates a skill's name against its directory (see
// recursiveDiscoverer above). Every other adapter/scope combination only
// looks at direct children of each base directory (plus, for Claude, legacy
// flat <name>.md files).
func List(a Adapter, scope Scope) ([]SkillRef, error) {
	dirs := a.UserDirs()
	if scope == ScopeProject {
		rel := a.ProjectDirs()
		dirs = make([]string, len(rel))
		for i, r := range rel {
			abs, err := filepath.Abs(r)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", r, err)
			}
			dirs[i] = abs
		}
	}

	recursive := false
	if rd, ok := a.(recursiveDiscoverer); ok {
		recursive = rd.DiscoversRecursively(scope)
	}

	seen := map[string]bool{}
	var out []SkillRef
	for _, d := range dirs {
		if !dirExists(d) {
			continue
		}
		hits, err := discoverSkillHits(d, recursive)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", d, err)
		}
		for _, h := range hits {
			if seen[h.Name] {
				continue
			}
			seen[h.Name] = true
			out = append(out, SkillRef{Name: h.Name, Provider: a.ID(), Scope: scope, Path: h.Path})
		}
	}
	return out, nil
}

// skillHit is one discovered skill location before it's wrapped into a
// SkillRef (which additionally needs the adapter/scope context List has).
type skillHit struct {
	Name string
	Path string
}

// discoverSkillHits finds skills under base: either only its direct
// children (a subdirectory containing SKILL.md, or a top-level *.md flat
// file), or — if recursive — every SKILL.md anywhere in the subtree.
func discoverSkillHits(base string, recursive bool) ([]skillHit, error) {
	if recursive {
		var hits []skillHit
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			dir := filepath.Dir(path)
			hits = append(hits, skillHit{Name: filepath.Base(dir), Path: dir})
			return nil
		})
		return hits, err
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}

	var hits []skillHit
	for _, e := range entries {
		if e.IsDir() {
			skillMd := filepath.Join(base, e.Name(), "SKILL.md")
			if fi, statErr := os.Stat(skillMd); statErr == nil && !fi.IsDir() {
				hits = append(hits, skillHit{Name: e.Name(), Path: filepath.Join(base, e.Name())})
			}
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			hits = append(hits, skillHit{Name: strings.TrimSuffix(e.Name(), ".md"), Path: filepath.Join(base, e.Name())})
		}
	}
	return hits, nil
}

package agentport

import (
	"io/fs"
	"os"
	"path/filepath"
)

// execBits are the only mode bits a resource's safe stored mode ever
// carries beyond ordinary read/write permissions: a source
// filesystem may report setuid/setgid/sticky bits or non-regular-file
// semantics, none of which a skill resource ever needs and none of which
// this package ever passes through, regardless of what the source
// reports.
const execBits = 0o111

// safeMode reduces an arbitrary source os.FileMode down to one of exactly
// two values: 0644 (ordinary file) or 0755 (any owner/group/other execute
// bit present). Setuid/setgid/sticky bits and any non-permission mode bits
// (device, socket, etc. — already refused earlier by the symlink/
// non-regular check in loadResources) are never preserved, by construction
// — there is no code path that copies the raw mode through.
func safeMode(m fs.FileMode) fs.FileMode {
	if m.Perm()&execBits != 0 {
		return 0o755
	}
	return 0o644
}

// ResourceFile is the canonical representation of one skill resource file:
// its content plus safe executable-mode metadata. Skill.Resources
// maps a relative path to one ResourceFile.
type ResourceFile struct {
	Data []byte
	Mode fs.FileMode // always safeMode's output: 0644 or 0755
}

// ExcludedResource records one file loadResources found under a skill root
// but did not load into Resources, together with why: a skipped
// or excluded file must always be reported, never silently absent from the
// artifact with no trace.
type ExcludedResource struct {
	Path   string
	Reason string
}

// controlMetadataNames is the fixed, hand-reviewed set of file/directory
// basenames treated as generated or version-control state rather than
// artifact content — never inferred or pattern-matched, matching this
// package's existing style for small reviewed tables (see
// agentSecurityFrontmatterKeys). Deliberately conservative: only names that
// are unambiguously tooling/VCS state, never a plausible skill resource
// name a real skill author would choose on purpose.
var controlMetadataNames = map[string]bool{
	".git":              true,
	".svn":              true,
	".hg":               true,
	".DS_Store":         true,
	"Thumbs.db":         true,
	"node_modules":      true,
	"__pycache__":       true,
	".venv":             true,
	"package-lock.json": true,
	"yarn.lock":         true,
	"pnpm-lock.yaml":    true,
	"Cargo.lock":        true,
	"poetry.lock":       true,
	"Gemfile.lock":      true,
}

// resolveSkillRoot resolves ONE level of symlink at path — the skill's own
// root directory or flat file, as explicitly named by a caller (a provider
// directory listing, or a user-supplied install path) — while leaving any
// symlink nested INSIDE that root untouched (those remain rejected/skipped
// by loadResources' per-entry check). filepath.WalkDir/fs.WalkDir
// never descends into a symlinked root (it Lstats the root and stops if
// that isn't a real directory), so without this resolution a symlinked
// skill's resources silently vanish from a load.
// Returns path unchanged if it isn't a symlink.
func resolveSkillRoot(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return path, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	return filepath.EvalSymlinks(path)
}

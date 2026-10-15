package agentport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LossKind categorizes how a target provider handled one IR field during
// Project().
type LossKind string

const (
	// LossPreserved means the field round-trips exactly.
	LossPreserved LossKind = "preserved"
	// LossDegraded means the field is represented, but not exactly (e.g. a
	// best-effort synthesized mapping).
	LossDegraded LossKind = "degraded"
	// LossDropped means the target format has no equivalent for the field.
	LossDropped LossKind = "dropped"
	// LossSynthesized means the target added something the source didn't
	// have (e.g. a required field filled with a placeholder).
	LossSynthesized LossKind = "synthesized"
)

// LossItem records the fidelity outcome for one IR field during a Project()
// call.
type LossItem struct {
	Field string
	Kind  LossKind
	Note  string
	// Security marks a LossItem whose field carries permission/restriction
	// semantics (tool allow/deny lists, permission modes, lifecycle hooks,
	// model constraints). Unlike ordinary fidelity loss, this
	// is never waved through by --strict, --dry-run, or an interactive
	// prompt: see HasSecurityLoss.
	Security bool
}

// HasSecurityLoss reports whether loss contains any Security-flagged item.
// Callers must check this before any --strict/--dry-run/interactive-
// confirmation handling and treat true as an unconditional refusal —
// security-relevant restrictions have no generic bypass.
func HasSecurityLoss(loss []LossItem) bool {
	for _, l := range loss {
		if l.Security {
			return true
		}
	}
	return false
}

// CapSet declares which typed-optional/extension IR fields a provider's
// on-disk format can represent. Used by computeLoss to decide which
// populated Skill fields must be reported as dropped for a given target.
type CapSet struct {
	AllowedTools            bool
	Paths                   bool
	DisableModelInvocation  bool
	AllowImplicitInvocation bool
	CodexInterface          bool
	CodexTools              bool
	Compatibility           bool
	License                 bool
	Metadata                bool
}

// Adapter converts between one provider's on-disk skill format and the
// canonical Skill IR.
type Adapter interface {
	// ID returns the provider identifier (claude|codex|opencode|cursor).
	ID() ProviderID

	// UserDirs returns the per-user (global) directories this provider
	// searches for skills, in priority order (some providers — opencode,
	// Cursor — also read other providers' directories for compatibility;
	// those appear after the provider's own primary directory).
	UserDirs() []string

	// ProjectDirs returns the per-project directories this provider
	// searches for skills, in priority order, relative to a project root.
	ProjectDirs() []string

	// Detect reports whether this provider appears to be installed on this
	// machine — i.e. whether any of its UserDirs() exist on disk.
	Detect() bool

	// Load reads a skill from skillDir (a directory containing SKILL.md,
	// or — for Claude Code only — a legacy flat `<name>.md` command file)
	// and returns its canonical IR.
	Load(skillDir string) (*Skill, error)

	// Project serializes a Skill into this provider's on-disk file layout.
	// files is keyed by path relative to the skill's target directory
	// (always includes "SKILL.md", plus any resources and sidecar files).
	// loss reports, per IR field the target format can't fully represent,
	// what happened to it.
	Project(s *Skill) (files map[string][]byte, loss []LossItem, err error)

	// Capabilities declares which IR extension fields this provider's
	// format can represent.
	Capabilities() CapSet

	// OwnUserDirCount reports how many LEADING entries of UserDirs() are
	// this provider's own writable directories, as opposed to another
	// provider's directory read for read-only compatibility (opencode,
	// Cursor) or a read-only admin directory (Codex's /etc/codex/skills).
	// Used by ResolveOwnSkillPath to scope destructive operations (skill
	// uninstall) so they can never touch a directory the provider doesn't
	// own. Claude Code returns 2 (skills/ and the legacy commands/ dir are
	// both genuinely Claude's own); every other provider returns 1.
	OwnUserDirCount() int

	// OwnProjectDirCount is the ProjectDirs() analogue of OwnUserDirCount.
	OwnProjectDirCount() int
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// computeLoss inspects a Skill for populated fields the target adapter's
// CapSet cannot represent, returning a LossDropped LossItem for each.
// Adapters call this from Project() and may layer their own additional
// LossItems (e.g. LossDegraded for a best-effort synthesized mapping) on
// top of what this returns.
func computeLoss(s *Skill, caps CapSet) []LossItem {
	var loss []LossItem
	add := func(field, note string) {
		loss = append(loss, LossItem{Field: field, Kind: LossDropped, Note: note})
	}
	if len(s.AllowedTools) > 0 && !caps.AllowedTools {
		loss = append(loss, LossItem{
			Field:    "AllowedTools",
			Kind:     LossDropped,
			Note:     "target provider has no allowed-tools equivalent — a tool restriction cannot silently disappear",
			Security: true,
		})
	}
	if len(s.Paths) > 0 && !caps.Paths {
		add("Paths", "target provider has no path-glob activation equivalent")
	}
	if s.DisableModelInvocation != nil && !caps.DisableModelInvocation {
		add("DisableModelInvocation", "target provider has no disable-model-invocation equivalent")
	}
	if s.AllowImplicitInvocation != nil && !caps.AllowImplicitInvocation {
		add("AllowImplicitInvocation", "target provider has no allow_implicit_invocation policy equivalent")
	}
	if s.CodexInterface != nil && !caps.CodexInterface {
		add("CodexInterface", "target provider has no openai.yaml interface sidecar equivalent")
	}
	if s.CodexTools != nil && !caps.CodexTools {
		add("CodexTools", "target provider has no MCP tool dependency declaration equivalent")
	}
	if s.Compatibility != "" && !caps.Compatibility {
		add("Compatibility", "target provider has no compatibility field equivalent")
	}
	if s.License != "" && !caps.License {
		add("License", "target provider has no license field equivalent")
	}
	if len(s.Metadata) > 0 && !caps.Metadata {
		add("Metadata", "target provider has no metadata map equivalent")
	}
	for _, ex := range s.ExcludedResources {
		loss = append(loss, LossItem{
			Field: "Resources",
			Kind:  LossDropped,
			Note:  "excluded resource " + ex.Path + ": " + ex.Reason,
		})
	}
	return loss
}

// ResolveSkillPath finds the on-disk location of a named skill for the given
// adapter+scope. For ScopeUser, it searches each of the adapter's UserDirs()
// in order. For ScopeProject, it searches each of the adapter's
// ProjectDirs() starting at the current working directory and walking up to
// the filesystem root (mirrors Codex/opencode/Cursor's "search parents to
// repo root" discovery). Both the standard `<dir>/<name>/SKILL.md` layout
// and (Claude Code only) legacy flat `<dir>/<name>.md` command files are
// recognized.
func ResolveSkillPath(a Adapter, scope Scope, name string) (string, error) {
	if err := validateNameFor(a, name); err != nil {
		return "", err
	}
	recursive := adapterDiscoversRecursively(a, scope)
	if scope == ScopeProject {
		path, err := resolveProjectSkillPath(a.ProjectDirs(), recursive, name)
		if err != nil {
			return "", fmt.Errorf("skill %q not found for provider %s (project scope): %w", name, a.ID(), err)
		}
		return path, nil
	}
	path, err := findSkillByName(a.UserDirs(), recursive, name)
	if err != nil {
		return "", fmt.Errorf("skill %q not found for provider %s (user scope): %w", name, a.ID(), err)
	}
	return path, nil
}

// nameValidator is an additive capability interface (mirrors
// recursiveDiscoverer, list.go) letting the name-based resolvers honor a
// provider's configured custom name_regex instead of always validating
// against the package-default nameRegex: a provider with a custom
// name_regex has Load/Project already accept names the public
// ResolveSkillPath/ResolveAgentPath would otherwise wrongly reject (or,
// for a looser custom regex, wrongly accept) by checking the wrong rule.
type nameValidator interface {
	validateName(name string) error
}

// validateNameFor validates name against a's own configured name_regex when
// a implements nameValidator, falling back to the package-default
// ValidateName otherwise (every shipped provider today, none of which set
// a custom name_regex, so their behavior is unchanged).
func validateNameFor(a interface{}, name string) error {
	if v, ok := a.(nameValidator); ok {
		return v.validateName(name)
	}
	return ValidateName(name)
}

// adapterDiscoversRecursively reports whether a's discovery for scope
// should walk the full subtree (list.go's recursiveDiscoverer) — shared by
// List and every name-based resolver so "what list shows" and "what a
// subsequent lookup by that name finds" can never drift apart.
func adapterDiscoversRecursively(a interface{}, scope Scope) bool {
	if rd, ok := a.(recursiveDiscoverer); ok {
		return rd.DiscoversRecursively(scope)
	}
	return false
}

// ResolveOwnSkillPath finds the on-disk location of a named skill within
// ONLY the given adapter's own writable directories (the leading
// OwnUserDirCount()/OwnProjectDirCount() entries of UserDirs()/
// ProjectDirs()) — it never searches another provider's compatibility-read
// directory, and never resolves into a read-only admin directory (e.g.
// Codex's /etc/codex/skills).
//
// This is the resolver DESTRUCTIVE operations (skill uninstall) must use.
// ResolveSkillPath, by contrast, is a READ-path resolver used by
// migrate/scan/diff and deliberately searches every documented
// compatibility-read fallback directory too — using it for a destructive
// delete would let e.g. `relay skill uninstall foo --from cursor` remove a
// Claude-owned skill it merely reads for compatibility.
func ResolveOwnSkillPath(a Adapter, scope Scope, name string) (string, error) {
	if err := validateNameFor(a, name); err != nil {
		return "", err
	}
	recursive := adapterDiscoversRecursively(a, scope)

	if scope == ScopeProject {
		dirs := ownDirs(a.ProjectDirs(), a.OwnProjectDirCount())
		path, err := resolveProjectSkillPath(dirs, recursive, name)
		if err != nil {
			return "", fmt.Errorf("skill %q not found for provider %s (project scope, own dirs only): %w", name, a.ID(), err)
		}
		return path, nil
	}

	dirs := ownDirs(a.UserDirs(), a.OwnUserDirCount())
	path, err := findSkillByName(dirs, recursive, name)
	if err != nil {
		return "", fmt.Errorf("skill %q not found for provider %s (user scope, own dirs only): %w", name, a.ID(), err)
	}
	return path, nil
}

// ownDirs returns the leading n entries of dirs (clamped to len(dirs)),
// guarding against a misconfigured adapter reporting an out-of-range own-dir
// count.
func ownDirs(dirs []string, n int) []string {
	if n < 0 {
		return nil
	}
	if n > len(dirs) {
		n = len(dirs)
	}
	return dirs[:n]
}

// findSkillByName searches each candidate base directory (in order) for
// name, using the SAME discovery walk List uses for the same adapter+scope
// (discoverSkillHits, list.go) — recursively when recursive is true — so a
// resolver's notion of "found" can never drift from what `skill list`
// already showed (a nested Cursor skill was listable but unusable by
// name because this function used to only check the non-recursive
// <dir>/<name>/SKILL.md and <dir>/<name>.md shapes). A directory whose scan
// finds more than one entry matching name is an explicit, deterministic
// ambiguity error rather than a silent first-match pick.
func findSkillByName(dirs []string, recursive bool, name string) (string, error) {
	for _, d := range dirs {
		if !dirExists(d) {
			continue
		}
		hits, err := discoverSkillHits(d, recursive)
		if err != nil {
			return "", fmt.Errorf("scan %s: %w", d, err)
		}
		var matches []string
		for _, h := range hits {
			if h.Name == name {
				matches = append(matches, h.Path)
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			return matches[0], nil
		default:
			sort.Strings(matches)
			return "", fmt.Errorf("ambiguous: %d entries named %q under %s: %s", len(matches), name, d, strings.Join(matches, ", "))
		}
	}
	return "", errNotFoundInDirs
}

// errNotFoundInDirs is findSkillByName/findAgentByName's "checked every
// candidate, nothing matched" sentinel — distinct from an ambiguity or scan
// error, so resolveProjectSkillPath/resolveProjectAgentPath know to keep
// walking up to the next parent directory rather than propagating a real
// error immediately.
var errNotFoundInDirs = errors.New("not found in the searched directories")

func resolveProjectSkillPath(relDirs []string, recursive bool, name string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	dir := cwd
	for {
		bases := make([]string, len(relDirs))
		for i, rel := range relDirs {
			bases[i] = filepath.Join(dir, rel)
		}
		if path, err := findSkillByName(bases, recursive, name); err == nil {
			return path, nil
		} else if !errors.Is(err, errNotFoundInDirs) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("not found in project dirs (searched from %s upward): %w", cwd, errNotFoundInDirs)
}

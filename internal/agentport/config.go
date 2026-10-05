package agentport

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type DirRole string

const (
	// DirRoleOwn marks a directory this provider owns and may write to (if
	// it's the leading entry) or otherwise treat as fully its own.
	DirRoleOwn DirRole = "own"
	// DirRoleAdmin marks a read-only org/admin scope belonging to this
	// provider (e.g. Codex's /etc/codex/skills) — counted by Detect() but
	// never a destructive-operation target.
	DirRoleAdmin DirRole = "admin"
	// DirRoleCompat marks another provider's directory, read here only for
	// read compatibility — never counted by Detect() or OwnDirCount.
	DirRoleCompat DirRole = "compat"
)

// DirEntry is one search-path entry in a provider's dirs.user or dirs.project
// list. Path may use "~" for the home directory (expanded at call time, not
// load time, so tests that override $HOME per-testcase still work).
type DirEntry struct {
	Path string  `yaml:"path"`
	Role DirRole `yaml:"role"`
}

// DirsConfig is a provider's ordered user/project search-path lists.
type DirsConfig struct {
	User    []DirEntry `yaml:"user"`
	Project []DirEntry `yaml:"project"`
}

// FieldType is the recognized set of frontmatter/sidecar value shapes a
// config can declare for one IR field mapping.
type FieldType string

const (
	FieldString       FieldType = "string"
	FieldList         FieldType = "list"
	FieldBool         FieldType = "bool"
	FieldStringOrList FieldType = "string-or-list"
	FieldMap          FieldType = "map"
	// FieldFloat is a *float64-typed field (nil/unset vs a set value) —
	// added for the Agent IR's Temperature (opencode-only; no skill IR
	// field uses it).
	FieldFloat FieldType = "float"
)

var validFieldTypes = map[FieldType]bool{
	FieldString:       true,
	FieldList:         true,
	FieldBool:         true,
	FieldStringOrList: true,
	FieldMap:          true,
	FieldFloat:        true,
}

type Presence string

const (
	PresenceRequired Presence = "required"
	PresenceOptional Presence = "optional"
)

type FrontmatterField struct {
	IR       string    `yaml:"ir"`
	Key      string    `yaml:"key"`
	Type     FieldType `yaml:"type"`
	Presence Presence  `yaml:"presence"`
}

// SidecarFieldSpec declaratively documents one field of a sidecar's nested
// mapping. The actual load/serialize logic for a sidecar is owned by a
// named Go codec (see hooks.go) because the shape is nested, not flat —
// these entries are documentation + capability/lossiness bookkeeping, not
// something the generic codec walks itself.
type SidecarFieldSpec struct {
	IR   string    `yaml:"ir"`
	Key  string    `yaml:"key"`
	Type FieldType `yaml:"type"`
}

type SidecarConfig struct {
	Path   string             `yaml:"path"`
	Format string             `yaml:"format"`
	Codec  string             `yaml:"codec"`
	Fields []SidecarFieldSpec `yaml:"fields"`
}

type DiscoveryMode string

const (
	DiscoveryDirect    DiscoveryMode = "direct"
	DiscoveryRecursive DiscoveryMode = "recursive"
)

type Layout string

const (
	LayoutDir  Layout = "dir"
	LayoutFlat Layout = "flat"
)

// AgentFormat selects the on-disk ENCODING for a layout: flat agent
// provider's primary file — orthogonal to Layout, which only distinguishes
// dir-vs-flat shape. "" (default, AgentFormatMarkdown) is the existing
// shape every skill provider and the claude/opencode agent providers use:
// YAML frontmatter + a free-text markdown body after a "---" separator.
// "toml" (AgentFormatTOML) is a flat TOML document with NO frontmatter/body
// split at all — added for Codex's custom-agent format
// (`~/.codex/agents/<name>.toml`), which encodes the agent's instructions
// via a designated IR field (canonical IR name "body", mapped to Codex's
// `developer_instructions` TOML key) rather than a markdown body. Only
// meaningful for agent providers today; no shipped skill config sets it.
// Implemented entirely with the stdlib (agent_toml.go) — this package's
// stdlib+yaml.v3-only dependency invariant is unaffected.
type AgentFormat string

const (
	AgentFormatMarkdown AgentFormat = "markdown"
	AgentFormatTOML     AgentFormat = "toml"
)

// ProviderConfig is the parsed+validated shape of one providers/<id>.yml —
// everything the generic configAdapter needs to implement the Adapter
// interface for one platform.
type ProviderConfig struct {
	ID        string `yaml:"id"`
	SkillFile string `yaml:"skill_file"`
	// ResourceDirs scopes loadResources (frontmatter.go) to only these
	// top-level subdirectories of the skill directory — see its doc comment.
	ResourceDirs []string      `yaml:"resource_dirs"`
	NameRegex    string        `yaml:"name_regex"`
	Discovery    DiscoveryMode `yaml:"discovery"`
	// Layout selects between the standard resource-bearing directory shape
	// ("dir", the default) and a single flat `<name>.md` file with no
	// resources ("flat") — see the Layout doc comment. Defaults to
	// LayoutDir when omitted.
	Layout Layout `yaml:"layout"`
	// Format selects the on-disk encoding for a layout: flat provider's
	// primary file — see the AgentFormat doc comment. Defaults to
	// AgentFormatMarkdown when omitted.
	Format       AgentFormat        `yaml:"format"`
	Dirs         DirsConfig         `yaml:"dirs"`
	Frontmatter  []FrontmatterField `yaml:"frontmatter"`
	Sidecar      *SidecarConfig     `yaml:"sidecar"`
	Capabilities []string           `yaml:"capabilities"`
	LoadHooks    []string           `yaml:"load_hooks"`

	Extends string `yaml:"extends"`

	// nameRe is the compiled NameRegex (or the package default), set by
	// validate().
	nameRe *regexp.Regexp
}

// parseProviderConfig unmarshals and validates one provider config document.
// registeredHooks/registeredCodecs are the load-hook/sidecar-codec registry
// key sets, used to validate load_hooks/sidecar.codec references.
func parseProviderConfig(raw []byte, registeredHooks, registeredCodecs map[string]bool, kind ArtifactKind) (*ProviderConfig, error) {
	var cfg ProviderConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse provider config: %w", err)
	}
	if err := cfg.validate(registeredHooks, registeredCodecs, kind); err != nil {
		return nil, fmt.Errorf("provider config %q: %w", cfg.ID, err)
	}
	return &cfg, nil
}

// validate enforces the config schema's rules: required fields present,
// dirs[scope][0].role == own, every dir path is traversal-safe and
// scope/role-appropriate (own/compat dirs are "~/..." for user scope or
// relative for project scope; only role admin may be absolute — see the
// dirs-path-safety block below), frontmatter types recognized AND matching
// each ir name's fixed canonical type, capability names resolve to CapSet
// fields, and every load_hooks/sidecar.codec name is registered. Never
// panics — every failure is a returned error naming the offending field.
func (c *ProviderConfig) validate(registeredHooks, registeredCodecs map[string]bool, kind ArtifactKind) error {
	if c.ID == "" {
		return fmt.Errorf("missing required field \"id\"")
	}
	if c.SkillFile == "" {
		c.SkillFile = "SKILL.md"
	}

	if pathHasDotDotSegment(c.SkillFile) || strings.HasPrefix(c.SkillFile, "/") {
		return fmt.Errorf("skill_file: must be a relative path with no \"..\" segment, got %q", c.SkillFile)
	}
	if c.Discovery == "" {
		c.Discovery = DiscoveryDirect
	}
	if c.Discovery != DiscoveryDirect && c.Discovery != DiscoveryRecursive {
		return fmt.Errorf("discovery: must be %q or %q, got %q", DiscoveryDirect, DiscoveryRecursive, c.Discovery)
	}

	if c.Layout == "" {
		c.Layout = LayoutDir
	}
	if c.Layout != LayoutDir && c.Layout != LayoutFlat {
		return fmt.Errorf("layout: must be %q or %q, got %q", LayoutDir, LayoutFlat, c.Layout)
	}

	if c.Format == "" {
		c.Format = AgentFormatMarkdown
	}
	if c.Format != AgentFormatMarkdown && c.Format != AgentFormatTOML {
		return fmt.Errorf("format: must be %q or %q, got %q", AgentFormatMarkdown, AgentFormatTOML, c.Format)
	}

	if c.NameRegex == "" {
		c.nameRe = nameRegex
	} else {
		re, err := regexp.Compile(c.NameRegex)
		if err != nil {
			return fmt.Errorf("name_regex: invalid regexp %q: %w", c.NameRegex, err)
		}
		c.nameRe = re
	}

	if len(c.Dirs.User) == 0 {
		return fmt.Errorf("dirs.user: must have at least one entry")
	}
	if c.Dirs.User[0].Role != DirRoleOwn {
		return fmt.Errorf("dirs.user[0].role: must be %q (the writable primary), got %q", DirRoleOwn, c.Dirs.User[0].Role)
	}
	if len(c.Dirs.Project) == 0 {
		return fmt.Errorf("dirs.project: must have at least one entry")
	}
	if c.Dirs.Project[0].Role != DirRoleOwn {
		return fmt.Errorf("dirs.project[0].role: must be %q (the writable primary), got %q", DirRoleOwn, c.Dirs.Project[0].Role)
	}
	for scope, entries := range map[string][]DirEntry{"user": c.Dirs.User, "project": c.Dirs.Project} {
		for i, d := range entries {
			if d.Path == "" {
				return fmt.Errorf("dirs.%s[%d].path: must not be empty", scope, i)
			}
			switch d.Role {
			case DirRoleOwn, DirRoleAdmin, DirRoleCompat:
			default:
				return fmt.Errorf("dirs.%s[%d].role: invalid role %q", scope, i, d.Role)
			}

			// Path safety (security-critical): these dirs feed destructive operations downstream — engine.go TargetDir/Write, adapter.go ResolveOwnSkillPath -> skill_uninstall.go os.RemoveAll, and rollback.go os.Remove.
			if pathHasDotDotSegment(d.Path) {
				return fmt.Errorf("dirs.%s[%d].path: must not contain \"..\"", scope, i)
			}
			switch d.Role {
			case DirRoleAdmin:
				// Admin scope is the documented read-only org case (e.g.
				// Codex's /etc/codex/skills) and is the ONLY role allowed
				// an absolute filesystem path.
			default: // DirRoleOwn, DirRoleCompat
				if scope == "user" {
					if !strings.HasPrefix(d.Path, "~/") || d.Path == "~/" {
						return fmt.Errorf("dirs.%s[%d].path: must start with \"~/\"", scope, i)
					}
				} else {
					if strings.HasPrefix(d.Path, "/") {
						return fmt.Errorf("dirs.%s[%d].path: must be relative", scope, i)
					}
				}
			}
		}
	}

	if len(c.Frontmatter) == 0 {
		return fmt.Errorf("frontmatter: must declare at least one field")
	}
	seenKeys := map[string]bool{}
	for i, f := range c.Frontmatter {
		if f.IR == "" {
			return fmt.Errorf("frontmatter[%d].ir: must not be empty", i)
		}

		expectedType, ok := canonicalIRFieldTypeForKind(f.IR, kind)
		if !ok {
			return fmt.Errorf("frontmatter[%d].ir: unknown %s IR field %q", i, kind, f.IR)
		}
		if f.Key == "" {
			return fmt.Errorf("frontmatter[%d].key: must not be empty", i)
		}
		if seenKeys[f.Key] {
			return fmt.Errorf("frontmatter[%d].key: duplicate key %q", i, f.Key)
		}
		seenKeys[f.Key] = true
		if !validFieldTypes[f.Type] {
			return fmt.Errorf("frontmatter[%d].type: invalid type %q", i, f.Type)
		}
		// irFieldDescriptors is the fixed Go-side type for this IR name
		// (decodeIRField/irFieldValue in ir_fields.go hard-switch on the
		// NAME and ignore the declared type entirely) — a config that
		// declares a mismatched type here (e.g. {ir: metadata, type:
		// string}) would otherwise be silently accepted and then decoded
		// using the wrong shape at load time.
		if f.Type != expectedType {
			return fmt.Errorf("frontmatter[%d].type: %q does not match canonical type %q for ir %q", i, f.Type, expectedType, f.IR)
		}
		if f.Presence == "" {
			f.Presence = PresenceOptional
		}
		if f.Presence != PresenceRequired && f.Presence != PresenceOptional {
			return fmt.Errorf("frontmatter[%d].presence: invalid presence %q", i, f.Presence)
		}
		c.Frontmatter[i] = f
	}

	if c.Sidecar != nil {
		if c.Sidecar.Path == "" {
			return fmt.Errorf("sidecar.path: must not be empty")
		}

		if pathHasDotDotSegment(c.Sidecar.Path) || strings.HasPrefix(c.Sidecar.Path, "/") {
			return fmt.Errorf("sidecar.path: must be a relative path with no \"..\" segment, got %q", c.Sidecar.Path)
		}
		if c.Sidecar.Codec == "" {
			return fmt.Errorf("sidecar.codec: must not be empty")
		}
		if !registeredCodecs[c.Sidecar.Codec] {
			return fmt.Errorf("sidecar.codec: unknown codec %q (registered: %v)", c.Sidecar.Codec, sortedKeys(registeredCodecs))
		}
	}

	for i, capName := range c.Capabilities {
		if !validCapNames[capName] {
			return fmt.Errorf("capabilities[%d]: unknown capability %q (valid: %v)", i, capName, sortedKeys(validCapNames))
		}
	}

	for i, h := range c.LoadHooks {
		if !registeredHooks[h] {
			return fmt.Errorf("load_hooks[%d]: unknown hook %q (registered: %v)", i, h, sortedKeys(registeredHooks))
		}
	}

	return nil
}

// pathHasDotDotSegment reports whether path contains a literal ".." path segment.
func pathHasDotDotSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var validCapNames = map[string]bool{
	"AllowedTools":            true,
	"Paths":                   true,
	"DisableModelInvocation":  true,
	"AllowImplicitInvocation": true,
	"CodexInterface":          true,
	"CodexTools":              true,
	"Compatibility":           true,
	"License":                 true,
	"Metadata":                true,
}

// buildCapSet turns a validated capabilities name list into a CapSet.
func buildCapSet(names []string) CapSet {
	var c CapSet
	for _, n := range names {
		switch n {
		case "AllowedTools":
			c.AllowedTools = true
		case "Paths":
			c.Paths = true
		case "DisableModelInvocation":
			c.DisableModelInvocation = true
		case "AllowImplicitInvocation":
			c.AllowImplicitInvocation = true
		case "CodexInterface":
			c.CodexInterface = true
		case "CodexTools":
			c.CodexTools = true
		case "Compatibility":
			c.Compatibility = true
		case "License":
			c.License = true
		case "Metadata":
			c.Metadata = true
		}
	}
	return c
}

func ownDirCount(entries []DirEntry) int {
	n := 0
	for _, d := range entries {
		if d.Role != DirRoleOwn {
			break
		}
		n++
	}
	return n
}

// FileExt returns the on-disk file extension for this config's primary
// file: ".toml" for format: toml (Codex custom agents), ".md" for the
// default markdown format — every skill provider and the claude/opencode
// agent providers. Used by the layout: flat agent path
// (agentConfigAdapter.Project, agent_resolve.go, agent_list.go,
// agent_scan.go) instead of hardcoding ".md", so a non-markdown agent
// provider's file extension is data, not a recompile.
func (c *ProviderConfig) FileExt() string {
	if c.Format == AgentFormatTOML {
		return ".toml"
	}
	return ".md"
}

// detectDirs returns the paths (still containing "~", unexpanded — callers
// expand at call time) of every entry whose role is own or admin — the
// derivation backing Detect(): "any dir with role in {own, admin}
// exists".
func detectDirs(entries []DirEntry) []DirEntry {
	var out []DirEntry
	for _, d := range entries {
		if d.Role == DirRoleOwn || d.Role == DirRoleAdmin {
			out = append(out, d)
		}
	}
	return out
}

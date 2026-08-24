package agentport

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file defines the provider-config YAML schema (§1 of the
// config-driven-adapters design): the data shape that replaces the 4
// hard-coded adapter_{claude,codex,cursor,opencode}.go implementations.
// See providers/*.yml for the shipped configs and config_adapter.go for the
// generic Adapter implementation built from a parsed ProviderConfig.

// DirRole classifies one entry of a provider's dirs list — see §1.2.
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

// Presence declares whether a frontmatter field is required or optional —
// see §1.3. (Load never hard-fails on an absent key purely because of this
// declaration; it documents intent and is validated for shape. The one
// exception, "name", falls back to the skill directory's base name when
// absent from frontmatter — a fixed IR-level rule, identical across every
// provider, reproducing the shipped adapters' behavior exactly.)
type Presence string

const (
	PresenceRequired Presence = "required"
	PresenceOptional Presence = "optional"
)

// FrontmatterField maps one canonical-IR field to a platform frontmatter
// key — see §1.3. Frontmatter is an ORDERED list: order = serialized key
// order, which is what makes Project() byte-exact with the pre-refactor
// per-provider structs.
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

// SidecarConfig declares a provider's optional secondary file (Codex's
// agents/openai.yaml) — see §1.4.
type SidecarConfig struct {
	Path   string             `yaml:"path"`
	Format string             `yaml:"format"`
	Codec  string             `yaml:"codec"`
	Fields []SidecarFieldSpec `yaml:"fields"`
}

// DiscoveryMode selects how project-scope List()/list.go discovery walks a
// provider's directories — see §2.4. User scope is always direct,
// regardless of this setting.
type DiscoveryMode string

const (
	DiscoveryDirect    DiscoveryMode = "direct"
	DiscoveryRecursive DiscoveryMode = "recursive"
)

// Layout classifies a provider's on-disk artifact shape (relay-standalone
// design §3a). "dir" is the standard Agent Skills shape — a resource-
// bearing directory `<name>/<skill_file>` — and is every shipped skill
// provider's layout today. "flat" is a single markdown file `<name>.md`
// with no containing directory and no resources; this is the native shape
// of agent providers (Claude/opencode agent definitions) and is promoted
// here from what was previously only reachable as a load-hook special case
// (Claude's legacy `commands/<name>.md` files, which remain layout: dir +
// the claude-legacy-commands hook, unchanged). Defaults to "dir" when
// omitted, so every existing provider config is unaffected.
type Layout string

const (
	LayoutDir  Layout = "dir"
	LayoutFlat Layout = "flat"
)

// ProviderConfig is the parsed+validated shape of one providers/<id>.yml —
// everything the generic configAdapter needs to implement the Adapter
// interface for one platform. See §1.1.
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
	Layout       Layout             `yaml:"layout"`
	Dirs         DirsConfig         `yaml:"dirs"`
	Frontmatter  []FrontmatterField `yaml:"frontmatter"`
	Sidecar      *SidecarConfig     `yaml:"sidecar"`
	Capabilities []string           `yaml:"capabilities"`
	LoadHooks    []string           `yaml:"load_hooks"`

	// Extends names another provider config (already loaded at a lower
	// tier) whose fields this config shallow-overlays on top of, for
	// partial-patch overrides (§4.2). Only meaningful for override tiers,
	// not the embedded defaults.
	Extends string `yaml:"extends"`

	// nameRe is the compiled NameRegex (or the package default), set by
	// validate().
	nameRe *regexp.Regexp
}

// parseProviderConfig unmarshals and validates one provider config document.
// registeredHooks/registeredCodecs are the load-hook/sidecar-codec registry
// key sets, used to validate load_hooks/sidecar.codec references — see §4.4.
func parseProviderConfig(raw []byte, registeredHooks, registeredCodecs map[string]bool) (*ProviderConfig, error) {
	var cfg ProviderConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse provider config: %w", err)
	}
	if err := cfg.validate(registeredHooks, registeredCodecs); err != nil {
		return nil, fmt.Errorf("provider config %q: %w", cfg.ID, err)
	}
	return &cfg, nil
}

// validate enforces §4.4's schema rules: required fields present,
// dirs[scope][0].role == own, every dir path is traversal-safe and
// scope/role-appropriate (own/compat dirs are "~/..." for user scope or
// relative for project scope; only role admin may be absolute — see the
// dirs-path-safety block below), frontmatter types recognized AND matching
// each ir name's fixed canonical type, capability names resolve to CapSet
// fields, and every load_hooks/sidecar.codec name is registered. Never
// panics — every failure is a returned error naming the offending field.
func (c *ProviderConfig) validate(registeredHooks, registeredCodecs map[string]bool) error {
	if c.ID == "" {
		return fmt.Errorf("missing required field \"id\"")
	}
	if c.SkillFile == "" {
		c.SkillFile = "SKILL.md"
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

			// Path safety (security-critical): these dirs feed destructive
			// operations downstream — engine.go TargetDir/Write,
			// adapter.go ResolveOwnSkillPath -> skill_uninstall.go
			// os.RemoveAll, and rollback.go os.Remove. A path-traversal
			// segment or an unexpectedly-absolute override (especially
			// from a user-tier config in ~/.config/relay/providers/*.yml,
			// loaded unconditionally by embed.go) would let a provider
			// config point a writable/removable dir anywhere on disk.
			// Checked for EVERY role (own/compat/admin) in BOTH scopes,
			// since Detect()/List() also walk compat/admin dirs.
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
		expectedType, ok := canonicalIRFieldType(f.IR)
		if !ok {
			return fmt.Errorf("frontmatter[%d].ir: unknown canonical IR field %q", i, f.IR)
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

// pathHasDotDotSegment reports whether path contains a literal ".."
// path segment. Intentionally checked by splitting on "/" rather than
// filepath.Clean: "~" is a special home-directory token expanded later, at
// call time (config_adapter.go's expandHome), not a real path component
// filepath understands — running Clean on the raw config string would
// silently resolve (and hide) an escape like "~/../etc". Any ".." segment
// is rejected outright, even one that would algebraically cancel out
// (e.g. "a/../b"), since none of the shipped configs need one and a
// conservative rule is simplest to reason about for a security check.
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

// validCapNames is the set of CapSet field names a config's "capabilities"
// list may reference — see §1.5.
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

// ownDirCount returns the count of leading contiguous DirRoleOwn entries —
// the derivation backing OwnUserDirCount()/OwnProjectDirCount() (§1.2).
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

// detectDirs returns the paths (still containing "~", unexpanded — callers
// expand at call time) of every entry whose role is own or admin — the
// derivation backing Detect() (§1.2): "any dir with role in {own, admin}
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

// Package agentport is a provider-agnostic Agent Skills manager engine.
//
// It is intentionally gateway-agnostic: no imports of relay's gateway
// client, auth, or config packages. This keeps it dependency-clean
// (stdlib + gopkg.in/yaml.v3 only) so it can be extracted verbatim into a
// standalone OSS module (github.com/agentport/agentport) later.
//
// The package implements the Agent Skills open standard
// (https://agentskills.io): a skill is a directory `<name>/SKILL.md` (YAML
// frontmatter + markdown body) with optional `scripts/`, `references/`, and
// `assets/` resource directories. Providers include Claude Code, Codex,
// opencode, and Cursor — see adapter_*.go.
package agentport

import (
	"fmt"
	"regexp"
)

// nameRegex enforces the Agent Skills standard skill-name format:
// lowercase alphanumerics and single hyphens, 1-64 characters, no leading/
// trailing/doubled hyphens.
var nameRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ProviderID identifies one of the four supported agent-skill providers.
type ProviderID string

const (
	ProviderClaude   ProviderID = "claude"
	ProviderCodex    ProviderID = "codex"
	ProviderOpencode ProviderID = "opencode"
	ProviderCursor   ProviderID = "cursor"
)

// Scope selects between per-user (global) and per-project skill locations.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// Provenance records where a Skill's IR came from. CatalogID and Version are
// set only for catalog-sourced installs (see internal/catalog) and empty for
// local migrations.
type Provenance struct {
	SourceProvider ProviderID
	SourcePath     string
	CatalogID      string // catalog resource id
	Version        string // catalog semver
}

// CodexInterface mirrors the `interface` block of a Codex `agents/openai.yaml`
// sidecar file — display/branding metadata for the Codex UI.
type CodexInterface struct {
	DisplayName      string
	ShortDescription string
	IconSmall        string
	IconLarge        string
	BrandColor       string
	DefaultPrompt    string
}

// CodexToolDependency mirrors one entry of a Codex `agents/openai.yaml`
// `dependencies.tools` list — a structured tool/MCP-server dependency
// declaration. Per the current vendor documentation
// (https://learn.chatgpt.com/docs/build-skills), every field is an
// optional string; there is no documented bare-string-list form.
type CodexToolDependency struct {
	Type        string
	Value       string
	Description string
	Transport   string
	URL         string
}

// CodexTools mirrors the `dependencies.tools` block of a Codex
// `agents/openai.yaml` sidecar file — declared tool/MCP dependencies.
type CodexTools struct {
	Tools []CodexToolDependency
}

// Skill is the canonical, provider-agnostic in-memory representation of an
// Agent Skill. Common fields (Name, Description, Body, License, Metadata,
// Resources) are understood by more than one provider; the typed-optional
// extension fields below belong to a single provider's format and are
// preserved on the IR only so a later migrate back to that same provider
// (or an explicit provider that also understands the field) doesn't lose
// them — see adapter Capabilities()/computeLoss for how Project() reports
// fields a target can't represent.
type Skill struct {
	Name        string
	Description string
	Body        string // markdown body after the frontmatter block
	License     string
	Metadata    map[string]string
	Resources   map[string]ResourceFile // relative path (scripts/, references/, assets/, ...) -> file content + mode

	// ExcludedResources records every file found under the skill root that
	// was NOT loaded into Resources (a symlink/non-regular entry, or
	// recognized control/generated metadata) together with why.
	// computeLoss turns each into a LossDropped LossItem so a skipped file
	// can never look like a clean "no loss" migration.
	ExcludedResources []ExcludedResource

	// UnmappedFields retains frontmatter keys this provider's configured
	// field set has no binding for. Same-provider
	// Project() re-emits them; a different target reports their loss
	// instead of guessing a foreign-schema equivalent.
	UnmappedFields []UnmappedField

	// --- typed-optional platform extensions (nil/zero = not set) ---
	AllowedTools            []string // Claude Code: allowed-tools
	Paths                   []string // Cursor: paths (glob activation patterns)
	DisableModelInvocation  *bool    // Claude Code + Cursor: disable-model-invocation
	AllowImplicitInvocation *bool    // Codex: policy.allow_implicit_invocation (openai.yaml sidecar)
	CodexInterface          *CodexInterface
	CodexTools              *CodexTools
	Compatibility           string // opencode: compatibility

	Provenance Provenance
}

// ValidateName checks a skill name against the Agent Skills standard format:
// ^[a-z0-9]+(-[a-z0-9]+)*$, 1-64 characters.
func ValidateName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("skill name %q must be 1-64 characters", name)
	}
	if !nameRegex.MatchString(name) {
		return fmt.Errorf("skill name %q must match ^[a-z0-9]+(-[a-z0-9]+)*$", name)
	}
	return nil
}

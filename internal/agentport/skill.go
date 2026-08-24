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
// `assets/` resource directories. Four providers are supported in Wave 1:
// Claude Code, Codex, opencode, and Cursor — see adapter_*.go.
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
// unused in Wave 1 (no marketplace/catalog integration for agentport yet)
// but are present so Wave 2 (catalog-sourced installs) doesn't need an IR
// shape change.
type Provenance struct {
	SourceProvider ProviderID
	SourcePath     string
	CatalogID      string // Wave 2: catalog resource id
	Version        string // Wave 2: catalog semver
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

// CodexTools mirrors the `dependencies.tools` block of a Codex
// `agents/openai.yaml` sidecar file — declared MCP server dependencies.
type CodexTools struct {
	MCPServers []string
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
	Resources   map[string][]byte // relative path (scripts/, references/, assets/, ...) -> file bytes

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

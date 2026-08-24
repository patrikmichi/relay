package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// genericFrontmatter is the union of every recognized Agent-Skills
// frontmatter field across the 4 built-in providers, used by
// LoadGenericSkill to parse an arbitrary local skill directory whose origin
// provider isn't known (or doesn't matter) — e.g. a hand-authored skill, or
// one fetched from somewhere that isn't one of the 4 canonical provider
// dirs. Unrecognized fields in the frontmatter are silently ignored (same
// policy as the provider-specific adapters).
type genericFrontmatter struct {
	Name                   string            `yaml:"name,omitempty"`
	Description            string            `yaml:"description,omitempty"`
	License                string            `yaml:"license,omitempty"`
	AllowedTools           flexStringList    `yaml:"allowed-tools,omitempty"`
	Paths                  flexStringList    `yaml:"paths,omitempty"`
	DisableModelInvocation *bool             `yaml:"disable-model-invocation,omitempty"`
	Compatibility          string            `yaml:"compatibility,omitempty"`
	Metadata               map[string]string `yaml:"metadata,omitempty"`
}

// LoadGenericSkill loads a skill from an arbitrary LOCAL directory (or a
// direct path to a SKILL.md file) as a generic Agent-Skills skill — the
// engine behind `relay skill install <path>`'s local-source path. It
// recognizes the full union of frontmatter extension fields any of the 4
// providers understand, plus the optional Codex agents/openai.yaml sidecar
// (via loadCodexSidecar, shared with the codex-openai SidecarCodec — see
// hooks.go), so a subsequent Migrate to any target only drops what that
// target's format genuinely can't represent.
//
// If the frontmatter has no `name`, the directory name is used instead (and
// validated via ValidateName). Unlike the provider adapters' Load, this
// does NOT require the directory name to match the frontmatter name — an
// arbitrary source directory isn't a provider's canonical skill root, so
// there's no round-trip invariant to enforce there.
//
// A gateway-sourced install (catalog resource id / semver — see
// Provenance.CatalogID/Version) is a later phase and is NOT implemented
// here; this function only ever reads from the local filesystem.
func LoadGenericSkill(path string) (*Skill, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	var skillDir, mdPath string
	if fi.IsDir() {
		skillDir = path
		mdPath = filepath.Join(path, "SKILL.md")
	} else {
		mdPath = path
		skillDir = filepath.Dir(path)
	}

	raw, err := os.ReadFile(mdPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", mdPath, err)
	}

	fmBytes, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", mdPath, err)
	}

	var fm genericFrontmatter
	if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter in %s: %w", mdPath, err)
	}

	name := strings.TrimSpace(fm.Name)
	if name == "" {
		name = filepath.Base(skillDir)
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// No ProviderConfig here — LoadGenericSkill reads an arbitrary local
	// source with no declared resource_dirs, so nil preserves the original
	// unscoped walk (see loadResources' doc comment).
	resources, skipped, err := loadResources(skillDir, map[string]bool{"SKILL.md": true, codexSidecarRelPath: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("load resources for %s: %w", skillDir, err)
	}
	warnSkippedResources(skillDir, skipped)

	s := &Skill{
		Name:          name,
		Description:   fm.Description,
		Body:          body,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		Metadata:      fm.Metadata,
		Resources:     resources,
		// SourceProvider is deliberately left empty: this is an arbitrary
		// local source, not one of the 4 canonical providers.
		Provenance: Provenance{SourcePath: skillDir},
	}
	if len(fm.AllowedTools) > 0 {
		s.AllowedTools = []string(fm.AllowedTools)
	}
	if len(fm.Paths) > 0 {
		s.Paths = []string(fm.Paths)
	}
	s.DisableModelInvocation = fm.DisableModelInvocation

	// Optional Codex-style sidecar, if the source directory happens to have
	// one (e.g. installing from a directory that was itself exported from
	// Codex). Shares the codex-openai SidecarCodec (hooks.go) with the
	// Codex provider adapter — a missing sidecar file is not an error.
	sidecarPath := filepath.Join(skillDir, filepath.FromSlash(codexSidecarRelPath))
	if err := loadCodexSidecar(sidecarPath, s); err != nil {
		return nil, err
	}

	return s, nil
}

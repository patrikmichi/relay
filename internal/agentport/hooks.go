package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file is the named-transform-hook registry (§3 of the
// config-driven-adapters design): the bounded, reviewed set of Go functions
// a provider config may reference by name for the ~10% of behavior that
// isn't a flat frontmatter field map. Exactly 2 hooks exist for the 4
// shipped platforms — codex-openai (a SidecarCodec) and
// claude-legacy-commands (a LoadHook) — see §3.1. Adding a hook is a
// deliberate, reviewed action (§3.2); config `type`/`discovery` enums are
// always preferred over a new hook.

// HookCtx is the context passed to every hook invocation.
type HookCtx struct {
	Cfg      ProviderConfig
	SkillDir string
}

// LoadHook is a named Go load-transform. It receives the raw bytes of a
// non-directory skill path (ctx.SkillDir was NOT a directory) and either
// claims it (handled=true, populating s) or declines (handled=false, no
// error) so the caller can try the next hook / fall through to an error.
type LoadHook func(ctx HookCtx, raw []byte, s *Skill) (handled bool, err error)

// SidecarCodec owns the load/serialize logic for a provider's secondary
// file (e.g. Codex's agents/openai.yaml) whose shape is nested, not a flat
// frontmatter field map.
type SidecarCodec interface {
	// Load reads ctx.SkillDir's sidecar file (if present) and populates the
	// relevant IR extension fields on s. A missing sidecar file is not an
	// error — the sidecar is always optional.
	Load(ctx HookCtx, s *Skill) error
	// Project computes the sidecar file's bytes from s's IR extension
	// fields and, if at least one relevant field is set, adds it to files
	// keyed by the sidecar's path (relative to the skill's target
	// directory). It must not add anything to files when no relevant field
	// is set (mirrors adapter_codex.go's haveSidecar guard).
	Project(ctx HookCtx, s *Skill, files map[string][]byte) error
}

// loadHookRegistry is the fixed set of named LoadHooks a config's
// load_hooks list may reference.
var loadHookRegistry = map[string]LoadHook{
	"claude-legacy-commands": claudeLegacyCommandsLoadHook,
}

// sidecarCodecRegistry is the fixed set of named SidecarCodecs a config's
// sidecar.codec may reference.
var sidecarCodecRegistry = map[string]SidecarCodec{
	"codex-openai": codexOpenAICodec{},
}

// registeredHookNames/registeredCodecNames are the name sets config.go's
// validate() checks load_hooks/sidecar.codec references against.
func registeredHookNames() map[string]bool {
	out := make(map[string]bool, len(loadHookRegistry))
	for k := range loadHookRegistry {
		out[k] = true
	}
	return out
}

func registeredCodecNames() map[string]bool {
	out := make(map[string]bool, len(sidecarCodecRegistry))
	for k := range sidecarCodecRegistry {
		out[k] = true
	}
	return out
}

// --- claude-legacy-commands: flat `<name>.md` command-file load ---
//
// Reproduces adapter_claude.go's isLegacyCommand branch exactly: a bare
// markdown file (not a SKILL.md-containing directory) is read directly,
// its frontmatter parsed with the SAME flat field map as the directory
// form (name/description/allowed-tools/disable-model-invocation/license),
// its name inferred from the filename (".md" trimmed) when frontmatter
// omits it, and — unlike the directory form — the inferred/parsed name is
// NOT required to match anything (there's no directory to compare against).
// It never has resources.
func claudeLegacyCommandsLoadHook(ctx HookCtx, raw []byte, s *Skill) (bool, error) {
	fmBytes, body, err := splitFrontmatter(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %w", ctx.SkillDir, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(fmBytes, &root); err != nil {
		return false, fmt.Errorf("parse frontmatter in %s: %w", ctx.SkillDir, err)
	}

	for _, f := range ctx.Cfg.Frontmatter {
		node := mappingLookup(&root, f.Key)
		if node == nil {
			continue
		}
		if err := decodeIRField(s, f.IR, node); err != nil {
			return false, fmt.Errorf("%s: field %q: %w", ctx.SkillDir, f.Key, err)
		}
	}

	name := s.Name
	if name == "" {
		base := filepath.Base(ctx.SkillDir)
		name = strings.TrimSuffix(base, ".md")
	}
	if err := ValidateName(name); err != nil {
		return false, err
	}
	s.Name = name
	s.Body = body
	s.Provenance = Provenance{SourceProvider: ProviderID(ctx.Cfg.ID), SourcePath: ctx.SkillDir}
	return true, nil
}

// --- codex-openai: agents/openai.yaml sidecar codec ---
//
// Reproduces adapter_codex.go's sidecar Load/Project exactly. Also used by
// LoadGenericSkill (install.go) — the two call sites that previously
// duplicated this nested-struct mapping now share this single codec.
type codexOpenAICodec struct{}

type codexInterfaceYAML struct {
	DisplayName      string `yaml:"display_name,omitempty"`
	ShortDescription string `yaml:"short_description,omitempty"`
	IconSmall        string `yaml:"icon_small,omitempty"`
	IconLarge        string `yaml:"icon_large,omitempty"`
	BrandColor       string `yaml:"brand_color,omitempty"`
	DefaultPrompt    string `yaml:"default_prompt,omitempty"`
}

type codexPolicyYAML struct {
	AllowImplicitInvocation *bool `yaml:"allow_implicit_invocation,omitempty"`
}

type codexDependenciesYAML struct {
	Tools []string `yaml:"tools,omitempty"`
}

type codexOpenAIYAML struct {
	Interface    *codexInterfaceYAML    `yaml:"interface,omitempty"`
	Policy       *codexPolicyYAML       `yaml:"policy,omitempty"`
	Dependencies *codexDependenciesYAML `yaml:"dependencies,omitempty"`
}

// codexSidecarRelPath is the fixed relative path of the openai.yaml sidecar
// within a skill directory — matches ProviderConfig.Sidecar.Path for the
// codex provider, and is also used directly by LoadGenericSkill (which has
// no ProviderConfig of its own, since it loads from an arbitrary local
// directory of unknown provider).
const codexSidecarRelPath = "agents/openai.yaml"

// codexAdminSkillsDir is Codex's admin (org-wide) skill scope per the Codex
// docs — providers/codex.yml's dirs.user[1] (role: admin). Kept as a
// package constant (rather than only living in the YAML) because it's
// referenced directly by tests asserting the admin scope is present in
// UserDirs() but never resolves as an "own" (writable/destructible)
// directory — see resolve_test.go and adapter_codex_test.go.
const codexAdminSkillsDir = "/etc/codex/skills"

func (codexOpenAICodec) Load(ctx HookCtx, s *Skill) error {
	relPath := codexSidecarRelPath
	if ctx.Cfg.Sidecar != nil && ctx.Cfg.Sidecar.Path != "" {
		relPath = ctx.Cfg.Sidecar.Path
	}
	return loadCodexSidecar(filepath.Join(ctx.SkillDir, filepath.FromSlash(relPath)), s)
}

// loadCodexSidecar reads and parses one codex openai.yaml sidecar file at
// path into s's IR extension fields. A missing file is not an error.
func loadCodexSidecar(path string, s *Skill) error {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// fall through
	case os.IsNotExist(err):
		return nil
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}

	var sc codexOpenAIYAML
	if err := yaml.Unmarshal(raw, &sc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if sc.Interface != nil {
		s.CodexInterface = &CodexInterface{
			DisplayName:      sc.Interface.DisplayName,
			ShortDescription: sc.Interface.ShortDescription,
			IconSmall:        sc.Interface.IconSmall,
			IconLarge:        sc.Interface.IconLarge,
			BrandColor:       sc.Interface.BrandColor,
			DefaultPrompt:    sc.Interface.DefaultPrompt,
		}
	}
	if sc.Policy != nil && sc.Policy.AllowImplicitInvocation != nil {
		v := *sc.Policy.AllowImplicitInvocation
		s.AllowImplicitInvocation = &v
	}
	if sc.Dependencies != nil && len(sc.Dependencies.Tools) > 0 {
		s.CodexTools = &CodexTools{MCPServers: sc.Dependencies.Tools}
	}
	return nil
}

func (codexOpenAICodec) Project(ctx HookCtx, s *Skill, files map[string][]byte) error {
	relPath := codexSidecarRelPath
	if ctx.Cfg.Sidecar != nil && ctx.Cfg.Sidecar.Path != "" {
		relPath = ctx.Cfg.Sidecar.Path
	}

	var sc codexOpenAIYAML
	haveSidecar := false
	if s.CodexInterface != nil {
		haveSidecar = true
		sc.Interface = &codexInterfaceYAML{
			DisplayName:      s.CodexInterface.DisplayName,
			ShortDescription: s.CodexInterface.ShortDescription,
			IconSmall:        s.CodexInterface.IconSmall,
			IconLarge:        s.CodexInterface.IconLarge,
			BrandColor:       s.CodexInterface.BrandColor,
			DefaultPrompt:    s.CodexInterface.DefaultPrompt,
		}
	}
	if s.AllowImplicitInvocation != nil {
		haveSidecar = true
		v := *s.AllowImplicitInvocation
		sc.Policy = &codexPolicyYAML{AllowImplicitInvocation: &v}
	}
	if s.CodexTools != nil && len(s.CodexTools.MCPServers) > 0 {
		haveSidecar = true
		sc.Dependencies = &codexDependenciesYAML{Tools: s.CodexTools.MCPServers}
	}
	if !haveSidecar {
		return nil
	}

	scBytes, err := yaml.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", relPath, err)
	}
	files[relPath] = scBytes
	return nil
}

// mappingLookup finds the value node for key in a parsed frontmatter
// document/mapping node (root may be a DocumentNode wrapping a
// MappingNode, or the mapping itself).
func mappingLookup(root *yaml.Node, key string) *yaml.Node {
	mapping := root
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		mapping = root.Content[0]
	}
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

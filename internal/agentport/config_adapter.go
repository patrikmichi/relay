package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// configAdapter is the ONE generic Adapter implementation, entirely driven
// by a validated ProviderConfig (config.go) plus whatever named hooks it
// references (hooks.go). Every provider — the 4 originally hard-coded
// platforms and any new platform added purely via a providers/<id>.yml file
// — is one configAdapter instance. See design §2.
type configAdapter struct {
	cfg          ProviderConfig
	caps         CapSet
	loadHooks    []LoadHook
	sidecarCodec SidecarCodec
}

// newConfigAdapter builds a configAdapter from an already-validated
// ProviderConfig — config.go's validate() guarantees every load_hooks/
// sidecar.codec name resolves in the registry, so the lookups below never
// silently drop a configured hook.
func newConfigAdapter(cfg ProviderConfig) *configAdapter {
	a := &configAdapter{cfg: cfg, caps: buildCapSet(cfg.Capabilities)}
	for _, name := range cfg.LoadHooks {
		if h, ok := loadHookRegistry[name]; ok {
			a.loadHooks = append(a.loadHooks, h)
		}
	}
	if cfg.Sidecar != nil {
		if codec, ok := sidecarCodecRegistry[cfg.Sidecar.Codec]; ok {
			a.sidecarCodec = codec
		}
	}
	return a
}

func (a *configAdapter) ID() ProviderID { return ProviderID(a.cfg.ID) }

// UserDirs expands each configured user dir's "~" at CALL time (not
// construction time) via os.UserHomeDir(), so a test that does
// t.Setenv("HOME", tmpdir) per test case sees the right directory on every
// call — matching the shipped adapters, which all called os.UserHomeDir()
// fresh from inside UserDirs() itself.
func (a *configAdapter) UserDirs() []string {
	return expandDirs(a.cfg.Dirs.User)
}

func (a *configAdapter) ProjectDirs() []string {
	out := make([]string, len(a.cfg.Dirs.Project))
	for i, d := range a.cfg.Dirs.Project {
		out[i] = d.Path
	}
	return out
}

// Detect reports whether any dir with role own|admin exists — the
// derivation replacing each shipped adapter's hand-written Detect() (§1.2).
func (a *configAdapter) Detect() bool {
	for _, d := range detectDirs(a.cfg.Dirs.User) {
		if dirExists(expandHome(d.Path)) {
			return true
		}
	}
	return false
}

func (a *configAdapter) Capabilities() CapSet { return a.caps }

// OwnUserDirCount/OwnProjectDirCount are the leading-contiguous-"own"-count
// derivation (§1.2): 2 for Claude (skills/ + legacy commands/, both own), 1
// for everyone else.
func (a *configAdapter) OwnUserDirCount() int    { return ownDirCount(a.cfg.Dirs.User) }
func (a *configAdapter) OwnProjectDirCount() int { return ownDirCount(a.cfg.Dirs.Project) }

// DiscoversRecursively implements the additive recursiveDiscoverer
// interface (§2.4) — list.go uses this instead of a hard-coded
// `a.(cursorAdapter)` type assertion. User scope is always direct,
// regardless of config, matching the shipped behavior exactly.
func (a *configAdapter) DiscoversRecursively(scope Scope) bool {
	return a.cfg.Discovery == DiscoveryRecursive && scope == ScopeProject
}

func expandDirs(entries []DirEntry) []string {
	out := make([]string, len(entries))
	for i, d := range entries {
		out[i] = expandHome(d.Path)
	}
	return out
}

// expandHome expands a leading "~" (as "~" or "~/...") to the current
// user's home directory.
func expandHome(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, filepath.FromSlash(path[2:]))
	}
	return path
}

// validateName validates name against this provider's configured
// name_regex — which defaults to the package-standard nameRegex (skill.go),
// in which case this delegates to the exported ValidateName so error text
// matches exactly for every shipped/new provider (none of which set a
// custom name_regex today).
func (a *configAdapter) validateName(name string) error {
	if a.cfg.NameRegex == "" {
		return ValidateName(name)
	}
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("skill name %q must be 1-64 characters", name)
	}
	if !a.cfg.nameRe.MatchString(name) {
		return fmt.Errorf("skill name %q must match %s", name, a.cfg.nameRe.String())
	}
	return nil
}

// Load reads a skill from skillDir — a directory containing cfg.SkillFile
// (the standard case for every provider), or, if skillDir is NOT a
// directory, a flat-file form recognized only if one of this provider's
// load_hooks claims it (Claude's legacy `commands/<name>.md` form; see
// hooks.go). See design §2.2.
//
// A provider config declaring `layout: flat` takes a distinct, first-class
// path (loadFlatFile) instead: skillDir is treated as the flat file itself
// unconditionally, with no os.Stat/IsDir branch and no load-hook fallback.
// This is the promoted, kind-agnostic generalization of the ad-hoc
// claude-legacy-commands hook (which remains layout: dir + a load hook,
// unchanged, for full back-compat) — it exists so a provider whose ENTIRE
// format is flat files (no directory shape at all, e.g. agent configs) never
// needs a bespoke load hook just to read its own primary layout.
func (a *configAdapter) Load(skillDir string) (*Skill, error) {
	if a.cfg.Layout == LayoutFlat {
		return a.loadFlatFile(skillDir)
	}

	fi, err := os.Stat(skillDir)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", skillDir, err)
	}

	ctx := HookCtx{Cfg: a.cfg, SkillDir: skillDir}

	if !fi.IsDir() {
		raw, rerr := os.ReadFile(skillDir)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", skillDir, rerr)
		}
		for _, h := range a.loadHooks {
			s := &Skill{}
			handled, herr := h(ctx, raw, s)
			if herr != nil {
				return nil, herr
			}
			if handled {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%s: not a directory, and no load hook for provider %s recognizes a flat-file skill", skillDir, a.cfg.ID)
	}

	mdPath := filepath.Join(skillDir, a.cfg.SkillFile)
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", mdPath, err)
	}

	fmBytes, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", skillDir, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(fmBytes, &root); err != nil {
		return nil, fmt.Errorf("parse frontmatter in %s: %w", skillDir, err)
	}

	s := &Skill{}
	for _, f := range a.cfg.Frontmatter {
		node := mappingLookup(&root, f.Key)
		if node == nil {
			continue
		}
		if err := decodeIRField(s, f.IR, node); err != nil {
			return nil, fmt.Errorf("%s: field %q: %w", skillDir, f.Key, err)
		}
	}

	// A skill with no explicit "name" in frontmatter infers it from the
	// directory name — a fixed IR-level rule, identical across every
	// provider (see ir_fields.go / config.go's Presence doc comment).
	if s.Name == "" {
		s.Name = filepath.Base(skillDir)
	}
	if err := a.validateName(s.Name); err != nil {
		return nil, err
	}
	if filepath.Base(skillDir) != s.Name {
		return nil, fmt.Errorf("skill name %q does not match directory name %q", s.Name, filepath.Base(skillDir))
	}

	excludes := map[string]bool{a.cfg.SkillFile: true}
	if a.cfg.Sidecar != nil {
		excludes[a.cfg.Sidecar.Path] = true
	}
	resources, skipped, err := loadResources(skillDir, excludes, a.cfg.ResourceDirs)
	if err != nil {
		return nil, fmt.Errorf("load resources for %s: %w", skillDir, err)
	}
	warnSkippedResources(skillDir, skipped)

	s.Body = body
	s.Resources = resources

	if a.sidecarCodec != nil {
		if err := a.sidecarCodec.Load(ctx, s); err != nil {
			return nil, err
		}
	}

	s.Provenance = Provenance{SourceProvider: a.ID(), SourcePath: skillDir}
	return s, nil
}

// loadFlatFile reads a flat `<name>.md` file directly at path — no
// containing directory, no SkillFile, no resources, no sidecar. It mirrors
// claudeLegacyCommandsLoadHook's frontmatter-loop and name-inference
// exactly, generalized to any layout: flat provider config: name falls
// back to the filename (".md" trimmed) when frontmatter omits it, and —
// unlike the directory form — the inferred/parsed name is never required
// to match anything (there's no directory to compare against).
func (a *configAdapter) loadFlatFile(path string) (*Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	fmBytes, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(fmBytes, &root); err != nil {
		return nil, fmt.Errorf("parse frontmatter in %s: %w", path, err)
	}

	s := &Skill{}
	for _, f := range a.cfg.Frontmatter {
		node := mappingLookup(&root, f.Key)
		if node == nil {
			continue
		}
		if err := decodeIRField(s, f.IR, node); err != nil {
			return nil, fmt.Errorf("%s: field %q: %w", path, f.Key, err)
		}
	}

	if s.Name == "" {
		s.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if err := a.validateName(s.Name); err != nil {
		return nil, err
	}

	s.Body = body
	s.Provenance = Provenance{SourceProvider: a.ID(), SourcePath: path}
	return s, nil
}

// Project serializes s into this provider's on-disk file layout. The
// frontmatter is built as an ORDERED yaml mapping node — walking
// cfg.Frontmatter in declaration order and skipping any field whose IR
// value is the Go zero value — which reproduces the pre-refactor per-
// provider structs' field order + universal `,omitempty` tag byte-for-byte
// (verified in config_adapter_parity_test.go against captured golden
// output from the original hard-coded adapters). See design §2.3.
func (a *configAdapter) Project(s *Skill) (map[string][]byte, []LossItem, error) {
	if err := a.validateName(s.Name); err != nil {
		return nil, nil, err
	}

	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, f := range a.cfg.Frontmatter {
		val, isZero := irFieldValue(s, f.IR)
		if isZero {
			continue
		}
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.Key}
		valNode := &yaml.Node{}
		if err := valNode.Encode(val); err != nil {
			return nil, nil, fmt.Errorf("encode field %q: %w", f.Key, err)
		}
		mapping.Content = append(mapping.Content, keyNode, valNode)
	}

	fmBytes, err := yaml.Marshal(mapping)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal frontmatter: %w", err)
	}
	content := joinFrontmatter(fmBytes, s.Body)

	files := map[string][]byte{a.cfg.SkillFile: content}
	for rel, data := range s.Resources {
		files[rel] = data
	}

	if a.sidecarCodec != nil {
		ctx := HookCtx{Cfg: a.cfg}
		if err := a.sidecarCodec.Project(ctx, s, files); err != nil {
			return nil, nil, err
		}
	}

	loss := computeLoss(s, a.caps)
	return files, loss, nil
}

package agentport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type agentConfigAdapter struct {
	cfg  ProviderConfig
	caps AgentCapSet
}

// newAgentConfigAdapter builds an agentConfigAdapter from an
// already-validated ProviderConfig plus its fixed, Go-code capability table
// (agent_caps.go — NOT config-driven, unlike skills' cfg.Capabilities; see
// agent_caps.go's doc comment for why).
func newAgentConfigAdapter(cfg ProviderConfig) *agentConfigAdapter {
	return &agentConfigAdapter{cfg: cfg, caps: agentCapSetFor(ProviderID(cfg.ID))}
}

func (a *agentConfigAdapter) ID() ProviderID { return ProviderID(a.cfg.ID) }

func (a *agentConfigAdapter) UserDirs() []string {
	return expandDirs(a.cfg.Dirs.User)
}

func (a *agentConfigAdapter) ProjectDirs() []string {
	out := make([]string, len(a.cfg.Dirs.Project))
	for i, d := range a.cfg.Dirs.Project {
		out[i] = d.Path
	}
	return out
}

func (a *agentConfigAdapter) Detect() bool {
	for _, d := range detectDirs(a.cfg.Dirs.User) {
		if dirExists(expandHome(d.Path)) {
			return true
		}
	}
	return false
}

func (a *agentConfigAdapter) Capabilities() AgentCapSet { return a.caps }

func (a *agentConfigAdapter) OwnUserDirCount() int    { return ownDirCount(a.cfg.Dirs.User) }
func (a *agentConfigAdapter) OwnProjectDirCount() int { return ownDirCount(a.cfg.Dirs.Project) }

// FileExt implements AgentAdapter.FileExt — delegates to the config's
// format-derived extension (config.go).
func (a *agentConfigAdapter) FileExt() string { return a.cfg.FileExt() }

// validateName validates name against this provider's configured
// name_regex — mirrors configAdapter.validateName. Neither shipped agent
// config sets a custom name_regex today, so this always delegates to the
// package-standard ValidateName; the field is still honored (not dropped)
// so a future agent provider config could set one, exactly like skills.
func (a *agentConfigAdapter) validateName(name string) error {
	if a.cfg.NameRegex == "" || a.cfg.nameRe == nil {
		return ValidateName(name)
	}
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("agent name %q must be 1-64 characters", name)
	}
	if !a.cfg.nameRe.MatchString(name) {
		return fmt.Errorf("agent name %q must match %s", name, a.cfg.nameRe.String())
	}
	return nil
}

// Load reads an agent from path — every shipped agent provider config is
// layout: flat (agents/*.yml), so this always reads path as the flat file
// itself: no directory Stat, no resources, no sidecar. Mirrors
// configAdapter.loadFlatFile exactly, but decodes into *Agent via
// decodeAgentIRField instead of *Skill via decodeIRField.
func (a *agentConfigAdapter) Load(path string) (*Agent, error) {
	if a.cfg.Format == AgentFormatTOML {
		return a.loadTOML(path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	fmBytes, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid agent document: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(fmBytes, &root); err != nil {
		return nil, fmt.Errorf("parse frontmatter in %s: %w", path, err)
	}

	var unique map[string]any
	if err := root.Decode(&unique); err != nil {
		return nil, fmt.Errorf("invalid or duplicate agent fields")
	}
	ag := &Agent{}
	configured := make(map[string]bool, len(a.cfg.Frontmatter))
	for _, f := range a.cfg.Frontmatter {
		configured[strings.ToLower(f.Key)] = true
		node := mappingLookup(&root, f.Key)
		if node == nil {
			continue
		}
		// opencode's on-disk "tools" shape is a {tool: bool} map, not the
		// canonical Agent IR's CSV/list shape decodeAgentIRField's "tools"
		// case assumes (see agents/opencode.yml's doc comment) — reshape it
		// via toolsMapToList (agent_caps.go) instead of the generic codec.
		// Explicitly-denied (`tool: false`) entries are captured separately
		// onto ag.DeniedTools (deniedTools, agent_caps.go) so Project() can
		// still surface/preserve that information even though the Tools
		// list itself can only carry the allowed subset.
		if f.IR == "tools" && a.ID() == ProviderOpencode {
			var m map[string]bool
			if err := node.Decode(&m); err != nil {
				return nil, fmt.Errorf("%s: field %q: %w", path, f.Key, err)
			}
			if v := toolsMapToList(m); len(v) > 0 {
				ag.Tools = v
			}
			if d := deniedTools(m); len(d) > 0 {
				ag.DeniedTools = d
			}
			continue
		}
		if f.IR == "tools" && a.ID() == ProviderID("gemini-cli") {
			if node.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("gemini tools must be an array")
			}
			if len(node.Content) == 0 {
				ag.UnmappedSecurityFields = append(ag.UnmappedSecurityFields, UnmappedSecurityField{Key: "tools", Raw: "empty explicit allowlist"})
			}
		}
		if err := decodeAgentIRField(ag, f.IR, node); err != nil {
			return nil, fmt.Errorf("%s: field %q: %w", path, f.Key, err)
		}
	}

	ag.UnmappedSecurityFields = append(ag.UnmappedSecurityFields, collectUnmappedSecurityFields(&root, configured)...)
	ag.UnmappedFields = collectUnmappedFields(&root, configured, agentSecurityFrontmatterKeys, a.ID())
	// These formats can embed session config and execution controls. Until each
	// extra field has a reviewed mapping, refuse it instead of guessing fidelity.
	if a.ID() == ProviderCodex || a.ID() == ProviderCursor || a.ID() == ProviderID("gemini-cli") {
		for _, field := range ag.UnmappedFields {
			ag.UnmappedSecurityFields = append(ag.UnmappedSecurityFields, UnmappedSecurityField{Key: field.Key, Raw: "unmapped execution setting"})
		}
		ag.UnmappedFields = nil
		for i := range ag.UnmappedSecurityFields {
			ag.UnmappedSecurityFields[i].Raw = "unmapped execution setting"
		}
	}

	// No on-disk "name" field is mapped for every shipped agent provider
	// (e.g. opencode); when frontmatter omits it, infer it from the
	// filename — the same fixed IR-level rule the flat skill-layout Load
	// path uses (config_adapter.go's loadFlatFile).
	if ag.Name == "" {
		base := filepath.Base(path)
		ag.Name = base[:len(base)-len(filepath.Ext(base))]
	}
	if err := a.validateName(ag.Name); err != nil {
		return nil, err
	}

	ag.Body = body
	ag.Provenance = Provenance{SourceProvider: a.ID(), SourcePath: path}
	return ag, nil
}

// Project serializes ag into this provider's on-disk flat-file layout: a
// single "<name>.md" entry (no resources). Model is re-encoded through
// modelLossForTarget's mapping (ag.Provenance.SourceProvider -> a.ID()) so
// the written file carries the TARGET provider's own model vocabulary, not
// a foreign one; loss is computeAgentLoss's dropped-field report plus any
// Model/Tools degraded-mapping notes layered on top — mirroring how
// codexOpenAICodec.Project layers its own LossItems on top of computeLoss's
// (adapter.go's documented pattern).
func (a *agentConfigAdapter) Project(ag *Agent) (map[string][]byte, []LossItem, error) {
	if err := a.validateName(ag.Name); err != nil {
		return nil, nil, err
	}

	if a.cfg.Format == AgentFormatTOML {
		return a.projectTOML(ag)
	}

	var modelLoss *LossItem
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, f := range a.cfg.Frontmatter {
		val, isZero := agentFieldValue(ag, f.IR)
		if f.IR == "model" && !isZero {
			mapped, l := modelLossForTarget(ag.Model, ag.Provenance.SourceProvider, a.ID())
			val = mapped
			isZero = mapped == ""
			modelLoss = l
		}

		if f.IR == "tools" {
			translatedTools := translateToolNames(ag.Tools, ag.Provenance.SourceProvider, a.ID())
			if a.ID() == ProviderOpencode {
				translatedDenied := translateToolNames(ag.DeniedTools, ag.Provenance.SourceProvider, a.ID())
				restrictive := claudeAllowlistIsRestrictive(ag.Tools, ag.Provenance.SourceProvider)
				if len(translatedTools) == 0 && len(translatedDenied) == 0 && !restrictive {
					continue
				}
				m := toolsListToMap(translatedTools, translatedDenied)
				if restrictive {
					// A Claude allowlist implies "everything else denied" —
					// opencode has no such implicit default, so make it
					// explicit rather than let the restriction evaporate.
					if m == nil {
						m = make(map[string]bool, 1)
					}
					if _, exists := m["*"]; !exists {
						m["*"] = false
					}
				}
				val = m
				isZero = false
			} else {
				if len(translatedTools) == 0 {
					continue
				}
				val = flexStringList(translatedTools)
				if a.ID() == ProviderID("gemini-cli") {
					val = translatedTools
				}
				isZero = false
			}
		}
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
	unmappedLoss := projectUnmappedFields(mapping, ag.UnmappedFields, ag.Provenance.SourceProvider, a.ID())

	fmBytes, err := yaml.Marshal(mapping)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal frontmatter: %w", err)
	}
	content := joinFrontmatter(fmBytes, ag.Body)
	if a.ID() == ProviderCursor || a.ID() == ProviderID("gemini-cli") {
		content = []byte("---\n" + string(fmBytes) + "---\n" + ag.Body)
	}

	files := map[string][]byte{ag.Name + a.FileExt(): content}

	loss := computeAgentLoss(ag, a.caps)
	source := ag.Provenance.SourceProvider
	if (len(ag.Tools) > 0 || len(ag.DeniedTools) > 0) && (!a.caps.Tools || (source != a.ID() && (source == ProviderID("gemini-cli") || a.ID() == ProviderID("gemini-cli")))) {
		loss = append(loss, LossItem{Field: "Tools", Kind: LossDropped, Security: true, Note: "target cannot preserve this source tool restriction; migration refused"})
	}
	loss = append(loss, unmappedLoss...)
	if modelLoss != nil {
		loss = append(loss, *modelLoss)
	}
	if l := toolsLossForTarget(ag.Tools, ag.DeniedTools, a.ID()); l != nil {
		loss = append(loss, *l)
	}

	return files, loss, nil
}

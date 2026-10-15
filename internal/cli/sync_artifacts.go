package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/patrikmichi/relay/internal/agentport"
	"github.com/patrikmichi/relay/internal/agentsync"
)

type skillSyncOpts struct {
	from       string
	targets    []agentsync.AgentID
	scope      agentsync.Scope
	skills     bool
	subagents  bool
	acceptLoss bool
	dryRun     bool
	force      bool
}

// syncSourceProvider picks the provider whose skills and subagents are
// copied to the other agents: --from, else Claude Code.
func syncSourceProvider(from string) agentport.ProviderID {
	if from == "" {
		return agentport.ProviderID(agentsync.Claude)
	}
	return agentport.ProviderID(from)
}

// compareFiles reports whether every projected file already exists with
// identical bytes (same), or some exist with different bytes (conflict).
func compareFiles(dir string, files map[string][]byte) (same, conflict bool, err error) {
	same = true
	for rel, data := range files {
		existing, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		switch {
		case errors.Is(rerr, fs.ErrNotExist):
			same = false
		case rerr != nil:
			return false, false, rerr
		case !bytes.Equal(existing, data):
			return false, true, nil
		}
	}
	return same, false, nil
}

// installDecision maps the destination's ownership to a sync outcome:
// identical content is unchanged; a missing or relay-installed and unedited
// artifact is added or updated; anything the user owns or edited is a
// conflict that only --force replaces.
func installDecision(same bool, state agentport.InstallState, force bool) (status, detail string, overwrite bool) {
	switch {
	case same:
		return agentsync.StatusUnchanged, "", false
	case state == agentport.InstallNone:
		return agentsync.StatusAdded, "", false
	case state == agentport.InstallRelay:
		return agentsync.StatusUpdated, "", true
	case force:
		return agentsync.StatusUpdated, "replaced with --force", true
	case state == agentport.InstallEdited:
		return agentsync.StatusConflict, "edited since relay installed it; --force overwrites", false
	}
	return agentsync.StatusConflict, "exists and was not installed by relay; --force overwrites", false
}

func lossSummary(loss []agentport.LossItem) string {
	var names []string
	for _, l := range loss {
		if l.Kind == agentport.LossDropped {
			names = append(names, l.Field)
		}
	}
	return strings.Join(names, ", ")
}

// planSkillsAndSubagents copies skills and subagents installed for the
// source provider into every other target agent through the agentport
// engine, so its fidelity reports and refusals apply unchanged. Targets
// that already hold different bytes are reported, never overwritten.
func planSkillsAndSubagents(o skillSyncOpts) ([]agentsync.Item, []func() error, error) {
	var items []agentsync.Item
	var writes []func() error
	scope := agentport.Scope(o.scope)
	srcID := syncSourceProvider(o.from)
	item := func(target agentsync.AgentID, kind agentsync.Kind, name, path, status, detail string) {
		items = append(items, agentsync.Item{Agent: target, Kind: kind, Scope: o.scope, Name: name, Path: path, Status: status, Detail: detail})
	}

	if o.skills {
		src, ok := agentport.AdapterByID(srcID)
		if !ok {
			return nil, nil, fmt.Errorf("unknown --from provider %q", srcID)
		}
		refs, err := agentport.List(src, scope)
		if err != nil {
			return nil, nil, err
		}
		for _, target := range o.targets {
			if string(target) == string(srcID) || len(refs) == 0 {
				continue
			}
			dst, ok := agentport.AdapterByID(agentport.ProviderID(target))
			if !ok {
				item(target, agentsync.KindSkills, "*", "", agentsync.StatusUnsupported, "no skill directory published")
				continue
			}
			for _, ref := range refs {
				// Flat files are Claude's legacy commands, which the commands kind
				// already covers.
				if info, err := os.Stat(ref.Path); err != nil || !info.IsDir() {
					continue
				}
				skill, err := src.Load(ref.Path)
				if err != nil {
					item(target, agentsync.KindSkills, ref.Name, ref.Path, agentsync.StatusSkipped, "cannot load: "+err.Error())
					continue
				}
				plan, err := agentport.Migrate(skill, dst, scope)
				if err != nil {
					item(target, agentsync.KindSkills, ref.Name, "", agentsync.StatusSkipped, err.Error())
					continue
				}
				switch {
				case agentport.HasSecurityLoss(plan.Loss):
					item(target, agentsync.KindSkills, ref.Name, plan.TargetPaths, agentsync.StatusSkipped, "refused: a permission field cannot be represented on "+string(target))
					continue
				case plan.HasDropped() && !o.acceptLoss:
					item(target, agentsync.KindSkills, ref.Name, plan.TargetPaths, agentsync.StatusSkipped, "would drop "+lossSummary(plan.Loss)+"; rerun with --accept-loss")
					continue
				}
				same, _, err := compareFiles(plan.TargetPaths, plan.Files)
				if err != nil {
					return nil, nil, err
				}
				state := agentport.ClassifyInstalled(skill.Name, dst.ID(), scope, agentport.KindSkill, plan.TargetPaths, plan.Files)
				status, detail, overwrite := installDecision(same, state, o.force)
				item(target, agentsync.KindSkills, ref.Name, plan.TargetPaths, status, detail)
				plan.Overwrite = overwrite
				if status == agentsync.StatusAdded || status == agentsync.StatusUpdated {
					if !o.dryRun {
						writes = append(writes, func() error {
							if err := agentport.PreflightWrite(plan); err != nil {
								return err
							}
							return agentport.Write(plan)
						})
					}
				}
			}
		}
	}

	if o.subagents {
		src, ok := agentport.AgentAdapterByID(srcID)
		if !ok {
			item(agentsync.AgentID(srcID), agentsync.KindSubagents, "*", "", agentsync.StatusUnsupported, "source provider has no subagent format")
			return items, writes, nil
		}
		refs, err := agentport.AgentList(src, scope)
		if err != nil {
			return nil, nil, err
		}
		for _, target := range o.targets {
			if string(target) == string(srcID) || len(refs) == 0 {
				continue
			}
			dst, ok := agentport.AgentAdapterByID(agentport.ProviderID(target))
			if !ok {
				item(target, agentsync.KindSubagents, "*", "", agentsync.StatusUnsupported, "no subagent file format published")
				continue
			}
			for _, ref := range refs {
				agent, err := src.Load(ref.Path)
				if err != nil {
					item(target, agentsync.KindSubagents, ref.Name, ref.Path, agentsync.StatusSkipped, "cannot load: "+err.Error())
					continue
				}
				plan, err := agentport.MigrateAgent(agent, dst, scope)
				if err != nil {
					item(target, agentsync.KindSubagents, ref.Name, "", agentsync.StatusSkipped, err.Error())
					continue
				}
				switch {
				case agentport.HasSecurityLoss(plan.Loss):
					item(target, agentsync.KindSubagents, ref.Name, plan.TargetPaths, agentsync.StatusSkipped, "refused: a permission or tool restriction cannot be represented on "+string(target))
					continue
				case plan.HasDropped() && !o.acceptLoss:
					item(target, agentsync.KindSubagents, ref.Name, plan.TargetPaths, agentsync.StatusSkipped, "would drop "+lossSummary(plan.Loss)+"; rerun with --accept-loss")
					continue
				}
				same, _, err := compareFiles(plan.TargetPaths, plan.Files)
				if err != nil {
					return nil, nil, err
				}
				state := agentport.ClassifyInstalled(agent.Name, dst.ID(), scope, agentport.KindAgent, plan.TargetPaths, plan.Files)
				status, detail, overwrite := installDecision(same, state, o.force)
				item(target, agentsync.KindSubagents, ref.Name, plan.TargetPaths, status, detail)
				plan.Overwrite = overwrite
				if status == agentsync.StatusAdded || status == agentsync.StatusUpdated {
					if !o.dryRun {
						writes = append(writes, func() error {
							if err := agentport.PreflightWriteAgent(plan); err != nil {
								return err
							}
							return agentport.WriteAgent(plan)
						})
					}
				}
			}
		}
	}
	return items, writes, nil
}

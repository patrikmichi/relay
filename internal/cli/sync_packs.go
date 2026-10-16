package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentport"
	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/packs"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// Installing a pack's skills and bundles goes through the same code as the
// commands that do it by hand; tests stand in for them.
var (
	installPackSkill  = installPackSkillFromGateway
	installPackBundle = installPackBundleFromGateway
)

// packSyncLoader fetches the pack part of a sync; a variable so tests that
// configure an unreachable gateway for other reasons never dial it.
var packSyncLoader = loadPackSync

// packSync is the pack part of one sync: what the gateway assigns this
// member, and what was assigned last time.
type packSync struct {
	gatewayURL string
	state      packs.State
	statePath  string
	assigned   packs.Assignments
	rules      []agentsync.Rule
	// unchanged is true when the gateway answered 304.
	unchanged bool
}

// loadPackSync fetches the member's assignments. It returns nil, with a
// warning on stderr, when the gateway cannot be asked: syncing local files
// never depends on packs. A response that fails its signature check is an
// error, since writing unverified rules into agent configuration is what the
// signature exists to prevent.
func loadPackSync(cmd *cobra.Command, gatewayURL string) (*packSync, error) {
	if Offline() {
		return nil, nil
	}
	gw, err := config.GatewayURL()
	if err != nil || gw == "" {
		return nil, nil
	}
	if gatewayURL != "" {
		if gw, err = config.NormalizeGatewayURL(gatewayURL); err != nil {
			return nil, err
		}
	}
	warn := func(format string, args ...any) (*packSync, error) {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: packs were not synced: "+format+"\n", args...)
		return nil, nil
	}
	c, err := client.Resolve(gw)
	if errors.Is(err, client.ErrNotLoggedIn) {
		return nil, nil
	}
	if err != nil {
		return warn("%v", err)
	}
	ps := &packSync{gatewayURL: gw}
	if ps.statePath, err = packs.StatePath(); err != nil {
		return warn("%v", err)
	}
	if ps.state, err = packs.LoadState(ps.statePath, gw); err != nil {
		return warn("%v", err)
	}
	headers := map[string]string{}
	if ps.state.Revision > 0 {
		headers["If-None-Match"] = `"` + strconv.Itoa(ps.state.Revision) + `"`
	}
	r, err := gatewayExchange(cmd.Context(), c, http.MethodGet, "/api/cli/sync/assignments", nil, headers)
	if err != nil {
		return warn("%v", err)
	}
	switch {
	case r.Status == http.StatusNotModified:
		ps.assigned, ps.unchanged = ps.state.Assignments, true
	case !r.ok():
		return warn("%v", r.failure())
	default:
		if err := r.verify(cmd.Context(), c); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(r.Body, &ps.assigned); err != nil {
			return warn("the assignments could not be read: %v", err)
		}
	}
	ps.rules = packRules(ps.assigned.Profiles)
	return ps, nil
}

// packRules turns delivered rules into sync rules named after their pack, so
// the sync's ledger tracks them: an edited block is a conflict, and a rule
// that is no longer assigned is removed.
func packRules(profiles []packs.Profile) []agentsync.Rule {
	var rules []agentsync.Rule
	for _, p := range profiles {
		for _, r := range p.Rules {
			rule := agentsync.Rule{
				Name:        brand.Name + "-pack-" + p.Pack.Slug + "-" + r.Name,
				Description: r.Description,
				Globs:       r.Globs,
				Always:      r.AlwaysApply,
				Body:        r.Body,
			}
			for _, a := range r.Agents {
				if ids, err := agentsync.ParseAgents([]string{a}); err == nil && a != "all" {
					rule.Agents = append(rule.Agents, ids...)
				}
			}
			rules = append(rules, rule)
		}
	}
	return rules
}

type packItem struct {
	kind, name, version string
	resourceID          string
	channel             string
}

// pending lists the skills and bundles that are new or changed since the last
// sync, and the ones that are no longer assigned.
func (ps *packSync) pending() (install []packItem, removed []string) {
	have := map[string]bool{}
	for _, p := range ps.state.Assignments.Profiles {
		for _, s := range p.Skills {
			have["skill|"+s.ResourceID+"|"+s.Version] = true
		}
		for _, b := range p.Bundles {
			have["bundle|"+b.Name+"|"+b.Version] = true
		}
	}
	now := map[string]bool{}
	for _, p := range ps.assigned.Profiles {
		for _, s := range p.Skills {
			now["skill|"+s.ResourceID] = true
			if !have["skill|"+s.ResourceID+"|"+s.Version] {
				install = append(install, packItem{kind: "skill", name: s.Name, version: s.Version, resourceID: s.ResourceID})
			}
		}
		for _, b := range p.Bundles {
			now["bundle|"+b.Name] = true
			if !have["bundle|"+b.Name+"|"+b.Version] {
				install = append(install, packItem{kind: "bundle", name: b.Name, version: b.Version, channel: b.Channel})
			}
		}
	}
	for _, p := range ps.state.Assignments.Profiles {
		for _, s := range p.Skills {
			if !now["skill|"+s.ResourceID] {
				removed = append(removed, fmt.Sprintf("skill %s is no longer assigned: remove it with `%s`", textsafe.Clean(s.Name), brand.Cmd("skill uninstall "+s.Name)))
			}
		}
		for _, b := range p.Bundles {
			if !now["bundle|"+b.Name] {
				removed = append(removed, fmt.Sprintf("bundle %s is no longer assigned: remove it with `%s`", textsafe.Clean(b.Name), brand.Cmd("plugin remove "+b.Name)))
			}
		}
	}
	return install, removed
}

// preview says what a dry run would install.
func (ps *packSync) preview(out io.Writer) {
	install, removed := ps.pending()
	for _, it := range install {
		fmt.Fprintf(out, "[dry-run] would install %s %s %s from a pack\n", it.kind, textsafe.Clean(it.name), textsafe.Clean(it.version))
	}
	for _, r := range removed {
		fmt.Fprintln(out, r)
	}
}

// apply installs the pack skills and bundles, remembers what was delivered
// and prints the connections the member still needs. One install failing
// does not stop the others; the state is saved only when all succeeded, so
// the next sync tries again.
func (ps *packSync) apply(cmd *cobra.Command) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	install, removed := ps.pending()
	failed := 0
	for _, it := range install {
		var err error
		if it.kind == "skill" {
			err = installPackSkill(cmd, ps.gatewayURL, it)
		} else {
			err = installPackBundle(cmd, ps.gatewayURL, it)
		}
		if err != nil {
			failed++
			fmt.Fprintf(errOut, "warning: pack %s %s was not installed: %v\n", it.kind, textsafe.Clean(it.name), err)
			continue
		}
		fmt.Fprintf(out, "Installed pack %s %s %s\n", it.kind, textsafe.Clean(it.name), textsafe.Clean(it.version))
	}
	for _, r := range removed {
		fmt.Fprintln(out, r)
	}
	for _, cr := range ps.assigned.ConnectRequired {
		fmt.Fprintf(out, "Connect %s: `%s` (%s)\n", textsafe.Clean(cr.Slug), brand.Cmd("connect "+cr.Slug), textsafe.Clean(cr.Reason))
	}
	if failed > 0 {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("%d pack item(s) were not installed; sync again to retry", failed)}
	}
	if ps.unchanged {
		return nil
	}
	ps.state = packs.State{Gateway: ps.gatewayURL, Revision: ps.assigned.Revision, Assignments: ps.assigned}
	return ps.state.Save(ps.statePath)
}

func installPackSkillFromGateway(cmd *cobra.Command, gatewayURL string, it packItem) error {
	src, _, err := fetchFromGateway(it.resourceID, skillInstallOpts{version: it.version, gatewayURL: gatewayURL})
	if err != nil {
		return err
	}
	targets := agentport.DetectedProviders()
	if len(targets) == 0 {
		return errors.New("no coding agent that takes skills was detected")
	}
	return applyMigration(io.Discard, &artifactReport{Command: "sync"}, src, "gateway", targets, agentport.ScopeUser, false, false, true)
}

func installPackBundleFromGateway(cmd *cobra.Command, gatewayURL string, it packItem) error {
	channel := it.channel
	if channel == "" {
		channel = "stable"
	}
	s, err := newPluginSession(cmd, gatewayURL, channel, false)
	if err != nil {
		return err
	}
	if err := s.connect(); err != nil {
		return err
	}
	agents, err := s.pluginAgents(nil)
	if err != nil {
		return err
	}
	_, err = s.installAll(cmd, agents, it.name, it.version, channel, installOpts{})
	return err
}

// doctorPacks reports whether the member's pack assignments can be read and
// whether the last delivered revision is still current.
func doctorPacks(ctx context.Context, c *client.Client, add func(string, bool, string)) {
	path, err := packs.StatePath()
	if err != nil {
		return
	}
	state, err := packs.LoadState(path, c.GatewayURL)
	if err != nil {
		add("packs", false, "the saved pack assignments cannot be read: "+err.Error())
		return
	}
	r, err := gatewayExchange(ctx, c, http.MethodGet, "/api/cli/sync/assignments", nil, nil)
	if err != nil || !r.ok() {
		// A gateway without packs is not a fault in this installation.
		add("packs", true, "pack assignments are not available from this gateway")
		return
	}
	var now packs.Assignments
	if json.Unmarshal(r.Body, &now) != nil {
		add("packs", false, "the pack assignments cannot be read")
		return
	}
	switch {
	case len(now.Profiles) == 0 && state.Revision == 0:
		add("packs", true, "no packs assigned")
	case now.Revision != state.Revision:
		add("packs", false, fmt.Sprintf("revision %d is available and %d was last synced; run `%s`", now.Revision, state.Revision, brand.Cmd("sync")))
	default:
		add("packs", true, fmt.Sprintf("%d pack(s), revision %d is synced", len(now.Profiles), now.Revision))
	}
}

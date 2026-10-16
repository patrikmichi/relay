package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/plugins"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const maxPackageBytes = 64 << 20

// verifiedPackage is a package that passed verification.
type verifiedPackage struct {
	entry    plugins.Entry
	platform string
	pkg      *plugins.Package
	archive  []byte
	// commands lists what the package would run on this machine.
	commands []string
}

func (v *verifiedPackage) commandsDigest() string {
	if len(v.commands) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(v.commands, "\n")))
	return hex.EncodeToString(sum[:])
}

// verified downloads and verifies the package of a plugin for a platform.
func (s *pluginSession) verified(name, version, channel, platform string) (*verifiedPackage, error) {
	key := strings.Join([]string{name, version, channel, platform}, "|")
	if vp, ok := s.packages[key]; ok {
		return vp, nil
	}
	var entry plugins.Entry
	switch platform {
	case platformClaude, platformAgentPlugins:
		m, err := s.marketplace(platform, channel)
		if err != nil {
			return nil, err
		}
		if entry, err = m.Find(name, version); err != nil {
			return nil, err
		}
	default:
		m, err := s.marketplace(platformClaude, channel)
		if err != nil {
			return nil, err
		}
		base, err := m.Find(name, version)
		if err != nil {
			return nil, err
		}
		if entry, err = s.packageEntry(base, platform); err != nil {
			return nil, err
		}
	}
	data, err := s.fetch.Get(s.ctx, entry.Source.URL, maxPackageBytes)
	if err != nil {
		return nil, feedError(err)
	}
	pkg, err := plugins.Verify(data, entry.Ext(), plugins.Expect{
		Plugin: entry.Name, Version: entry.Version, Platform: platform, SHA256: entry.Source.SHA256, Keys: s.keys,
	})
	if err != nil {
		return nil, err
	}
	vp := &verifiedPackage{entry: entry, platform: platform, pkg: pkg, archive: data, commands: packageCommands(pkg, platform)}
	s.packages[key] = vp
	return vp, nil
}

// packageEntry finds a platform's package in the gateway's package list.
func (s *pluginSession) packageEntry(base plugins.Entry, platform string) (plugins.Entry, error) {
	var doc struct {
		Packages []struct {
			Platform string `json:"platform"`
			State    string `json:"state"`
			SHA256   string `json:"sha256"`
			URL      string `json:"url"`
		} `json:"packages"`
	}
	path := "/api/plugins/" + url.PathEscape(base.Name) + "/packages?version=" + url.QueryEscape(base.Version)
	if _, err := gatewayJSON(s.ctx, s.client, http.MethodGet, path, nil, &doc); err != nil {
		return plugins.Entry{}, err
	}
	for _, p := range doc.Packages {
		if p.Platform == platform {
			if p.State != "ready" {
				return plugins.Entry{}, fmt.Errorf("the %s package of %s %s is %s on the gateway", platform, base.Name, base.Version, textsafe.Clean(p.State))
			}
			return plugins.Entry{Name: base.Name, Version: base.Version, Source: plugins.Source{Source: "url", URL: p.URL, SHA256: p.SHA256}}, nil
		}
	}
	return plugins.Entry{}, fmt.Errorf("%s %s has no %s package", base.Name, base.Version, platform)
}

// packageCommands lists what a package runs on this machine: hook commands,
// local MCP servers and the code of an agent-side plugin.
func packageCommands(pkg *plugins.Package, platform string) []string {
	var out []string
	for _, name := range pkg.Paths() {
		data := pkg.Files[name]
		base := path.Base(name)
		switch {
		case name == "hooks/hooks.json" || name == "hooks.json" || (strings.HasPrefix(name, "hooks/") && strings.HasSuffix(name, ".json")):
			out = append(out, jsonCommands(data, "hook")...)
		case base == ".mcp.json" || base == "mcp.json" || base == "gemini-extension.json":
			out = append(out, jsonCommands(data, "mcp server")...)
		case strings.HasPrefix(name, "hooks/"):
			out = append(out, "hook script "+name)
		}
	}
	switch platform {
	case platformOpencode:
		out = append(out, "OpenCode plugin code (index.js) that runs inside the agent")
	case platformCline:
		out = append(out, "Cline plugin code that runs inside the agent")
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func jsonCommands(data []byte, label string) []string {
	var doc any
	if json.Unmarshal(data, &doc) != nil {
		return []string{label + ": " + "an unreadable file that may name commands"}
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if cmd, ok := t["command"].(string); ok && cmd != "" {
				line := cmd
				if args, ok := t["args"].([]any); ok {
					for _, a := range args {
						line += " " + fmt.Sprint(a)
					}
				}
				out = append(out, label+": "+line)
			}
			for _, e := range t {
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

type installOpts struct {
	allowHooks bool
	jsonOut    bool
}

type pluginResult struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version,omitempty"`
	Agent   string `json:"agent"`
	Method  string `json:"method,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

type pluginTarget struct {
	name, version, channel string
	vp                     *verifiedPackage
	prior                  *plugins.Install
	opts                   installOpts
}

func pluginInstallCmd() *cobra.Command {
	var gatewayURL, channel string
	var to []string
	var opts installOpts
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "install <name>[@version]",
		Short: "Install a plugin bundle into coding agents",
		Long: `Installs a bundle from your organisation's plugin feed into each agent named by
--to (default: every supported agent found on this machine). The package is
verified first; see 'zuano plugin --help'. A bundle whose hooks or local MCP
servers run commands on your machine is listed and needs --allow-hooks (or a yes
at the prompt).`,
		Example: `  zuano plugin install toolkit --to claude,cursor
  zuano plugin install toolkit@1.2.0 --channel beta --allow-hooks --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, version, err := parsePluginRef(args[0])
			if err != nil {
				return err
			}
			s, err := newPluginSession(cmd, gatewayURL, channel, dryRun)
			if err != nil {
				return err
			}
			if err := s.connect(); err != nil {
				return err
			}
			agents, err := s.pluginAgents(to)
			if err != nil {
				return err
			}
			s.checkRequirements(name)
			results, err := s.installAll(cmd, agents, name, version, channel, opts)
			return s.finish(results, err, opts.jsonOut)
		},
	}
	addPluginFlags(cmd, &gatewayURL, &channel, &to, "stable")
	cmd.Flags().BoolVar(&opts.allowHooks, "allow-hooks", false, "Allow the bundle's hooks and local MCP servers to run commands on this machine")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Verify the package and show what would be installed, without installing")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "Print the outcome per agent as JSON")
	return cmd
}

func addPluginFlags(cmd *cobra.Command, gatewayURL, channel *string, to *[]string, defaultChannel string) {
	cmd.Flags().StringVar(gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().StringVar(channel, "channel", defaultChannel, "Release channel: stable or beta")
	cmd.Flags().StringSliceVar(to, "to", nil, "Agents to act on (claude, codex, cursor, gemini-cli, opencode, cline, windsurf, copilot, vscode); default: those found on this machine")
}

// finish prints the results and turns a failure into the right exit status.
func (s *pluginSession) finish(results []pluginResult, err error, jsonOut bool) error {
	if jsonOut {
		if encErr := json.NewEncoder(s.out).Encode(map[string]any{"results": results}); encErr != nil && err == nil {
			err = encErr
		}
	}
	return err
}

// checkRequirements warns about connectors the bundle needs that are not
// connected. It never blocks an install.
func (s *pluginSession) checkRequirements(name string) {
	var req struct {
		Missing []string `json:"missing"`
	}
	if _, err := gatewayJSON(s.ctx, s.client, http.MethodGet, "/api/plugins/"+url.PathEscape(name)+"/requirements", nil, &req); err != nil {
		return
	}
	for _, slug := range req.Missing {
		fmt.Fprintf(s.err, "warning: %s needs the %s connector, which is not connected; run `%s`\n", textsafe.Clean(name), textsafe.Clean(slug), brand.Cmd("connect "+slug))
	}
}

// installAll installs one plugin for each agent. A package that fails
// verification stops everything; another failure is reported and the next
// agent still runs.
func (s *pluginSession) installAll(cmd *cobra.Command, agents []*pluginAgent, name, version, channel string, opts installOpts) ([]pluginResult, error) {
	var results []pluginResult
	var firstErr error
	for _, a := range agents {
		res, err := s.installFor(cmd, a, name, version, channel, opts)
		results = append(results, res)
		if err == nil {
			continue
		}
		var ve *plugins.VerifyError
		if errors.As(err, &ve) {
			return results, err
		}
		fmt.Fprintf(s.err, "%s: %v\n", a.id, err)
		if firstErr == nil {
			firstErr = &ExitError{Code: 1}
		}
	}
	return results, firstErr
}

func (s *pluginSession) installFor(cmd *cobra.Command, a *pluginAgent, name, version, channel string, opts installOpts) (pluginResult, error) {
	res := pluginResult{Plugin: name, Agent: a.id, Status: "failed"}
	vp, err := s.verified(name, version, channel, a.platform)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	res.Version = vp.entry.Version
	prior, hadPrior := s.ledger.Get(name, a.id)
	if len(vp.commands) > 0 && !(hadPrior && prior.CommandsSHA256 == vp.commandsDigest()) {
		if err := s.approveCommands(cmd, name, vp, opts); err != nil {
			res.Error = err.Error()
			return res, err
		}
	}
	if !opts.jsonOut {
		fmt.Fprintf(s.out, "Verified %s %s (%s package, signed with key %s)\n", textsafe.Clean(name), textsafe.Clean(vp.entry.Version), a.platform, textsafe.Clean(vp.pkg.Signature.KID))
	}
	if s.dryRun {
		res.Status, res.Method = "dry-run", a.method
		if !opts.jsonOut {
			fmt.Fprintf(s.out, "  would install for %s (%s)\n", a.id, a.method)
		}
		return res, nil
	}
	t := &pluginTarget{name: name, version: vp.entry.Version, channel: channel, vp: vp, opts: opts}
	if hadPrior {
		t.prior = &prior
	}
	in, err := a.install(s, a, t)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	in.Version, in.Platform, in.Channel = vp.entry.Version, a.platform, channel
	in.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	in.CommandsSHA256 = vp.commandsDigest()
	if prior.InstallID != "" && in.InstallID == "" {
		in.InstallID = prior.InstallID
	}
	in.InstallID = s.reportInstall(name, in, a.id)
	s.ledger.Set(name, a.id, in)
	if err := s.saveLedger(); err != nil {
		return res, fmt.Errorf("installed, but the ledger could not be written: %w", err)
	}
	res.Status, res.Method = "installed", in.Method
	if !opts.jsonOut {
		fmt.Fprintf(s.out, "Installed %s %s for %s (%s)\n", textsafe.Clean(name), textsafe.Clean(vp.entry.Version), a.id, in.Method)
	}
	return res, nil
}

// approveCommands lists what the bundle runs on this machine and requires
// --allow-hooks or a yes from a person at a terminal.
func (s *pluginSession) approveCommands(cmd *cobra.Command, name string, vp *verifiedPackage, opts installOpts) error {
	w := s.out
	if opts.jsonOut {
		w = s.err
	}
	fmt.Fprintf(w, "%s %s runs the following on this machine:\n", textsafe.Clean(name), textsafe.Clean(vp.entry.Version))
	for _, c := range vp.commands {
		fmt.Fprintf(w, "  %s\n", textsafe.Clean(c))
	}
	if opts.allowHooks {
		return nil
	}
	if canOpenBrowser(cmd, true) && !opts.jsonOut && !s.dryRun {
		if askDefaultNo(cmd.InOrStdin(), w, "Install it anyway? [y/N] ") {
			return nil
		}
		return &ExitError{Code: 1, Msg: "cancelled; nothing was installed"}
	}
	if s.dryRun {
		return nil
	}
	return fmt.Errorf("%s runs commands on this machine (listed above); review them and repeat with --allow-hooks", textsafe.Clean(name))
}

// reportInstall tells the gateway about an install, so it can offer updates
// and revoke versions. It returns the install id, or the previous one when the
// gateway cannot be reached; the install itself never depends on it.
func (s *pluginSession) reportInstall(name string, in plugins.Install, agent string) string {
	var res struct {
		Install struct {
			ID string `json:"id"`
		} `json:"install"`
	}
	body := map[string]any{"plugin": name, "version": in.Version, "agent": agent, "method": in.Method, "cli_version": client.Version()}
	if _, err := gatewayJSON(s.ctx, s.client, http.MethodPost, "/api/plugins/installs", body, &res); err != nil || res.Install.ID == "" {
		return in.InstallID
	}
	return res.Install.ID
}

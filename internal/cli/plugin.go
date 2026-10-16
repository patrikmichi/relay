package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/plugins"
	"github.com/patrikmichi/zuano/internal/signing"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

var pluginNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Package platforms, as the gateway names them.
const (
	platformClaude       = "claude"
	platformAgentPlugins = "agent_plugins"
	platformGemini       = "gemini"
	platformOpencode     = "opencode_npm"
	platformCline        = "cline"
)

// PluginCmd returns `zuano plugin`, which installs the gateway's plugin
// bundles into coding agents.
func PluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Install, update and remove the gateway's plugin bundles in your coding agents",
		Long: `A plugin bundle is skills, commands, subagents, hooks, MCP servers and rules
published once on the gateway. zuano installs it through each agent's own plugin
system where it has one, and expands what it can into the agent's configuration
where it has not.

Every package is checked before anything is installed: its sha256 against the
feed, then the Ed25519 signature inside it against the signing keys pinned for
your gateway, and every file against the signed list. A package that fails
exits with status 4 and nothing is installed. Hooks and local commands in a
bundle run on this machine, so zuano lists them and asks for --allow-hooks.`,
	}
	cmd.AddCommand(pluginInstallCmd(), pluginUpdateCmd(), pluginRemoveCmd(), pluginListCmd(), pluginVerifyCmd(), pluginFeedsCmd())
	return cmd
}

// pluginFeed mirrors GET /api/org/plugin-feeds.
type pluginFeed struct {
	Slug      string   `json:"slug"`
	BaseURL   string   `json:"base_url"`
	NPMScope  string   `json:"npm_scope"`
	GitMirror bool     `json:"git_mirror"`
	Channels  []string `json:"channels"`
}

// pluginSession is the state one plugin command works with.
type pluginSession struct {
	ctx        context.Context
	out, err   io.Writer
	client     *client.Client
	origin     string
	feed       pluginFeed
	fetch      plugins.Fetcher
	keys       []signing.Key
	home       string
	ledger     *plugins.Ledger
	ledgerPath string
	channel    string
	dryRun     bool
	markets    map[string]plugins.Marketplace
	packages   map[string]*verifiedPackage
}

func newPluginSession(cmd *cobra.Command, gatewayURL, channel string, dryRun bool) (*pluginSession, error) {
	if channel != "stable" && channel != "beta" {
		return nil, fmt.Errorf("invalid --channel %q: use stable or beta", textsafe.Clean(channel))
	}
	c, err := approvalsClient(gatewayURL)
	if err != nil {
		return nil, err
	}
	origin, err := config.NormalizeGatewayURL(c.GatewayURL)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	s := &pluginSession{
		ctx: cmd.Context(), out: cmd.OutOrStdout(), err: cmd.ErrOrStderr(), client: c, origin: origin,
		home: home, channel: channel, dryRun: dryRun,
		markets: map[string]plugins.Marketplace{}, packages: map[string]*verifiedPackage{},
	}
	s.ledgerPath = plugins.LedgerPath(home)
	if s.ledger, err = plugins.LoadLedger(s.ledgerPath); err != nil {
		return nil, err
	}
	return s, nil
}

// connect loads the feed description, the signing keys and a feed token; the
// commands that only read the ledger do not need it.
func (s *pluginSession) connect() error {
	var doc struct {
		Feed pluginFeed `json:"feed"`
	}
	if _, err := gatewayJSON(s.ctx, s.client, http.MethodGet, "/api/org/plugin-feeds", nil, &doc); err != nil {
		return err
	}
	s.feed = doc.Feed
	base, err := url.Parse(s.feed.BaseURL)
	o, oerr := url.Parse(s.origin)
	if err != nil || oerr != nil || base.Scheme != o.Scheme || base.Host != o.Host || !strings.HasPrefix(base.Path, "/m/") {
		return errors.New("the gateway's plugin feed is not under its own /m/ path; refusing to use it")
	}
	s.feed.BaseURL = strings.TrimRight(s.feed.BaseURL, "/")
	if s.keys, err = config.PinnedSigningKeys(s.origin); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(s.keys) == 0 {
		pinSigningKeys(s.ctx, s.origin, s.err)
		if s.keys, err = config.PinnedSigningKeys(s.origin); err != nil {
			return fmt.Errorf("load config: %w", err)
		}
	}
	if len(s.keys) == 0 {
		return fmt.Errorf("no signing key is pinned for this gateway, so no package can be verified; run `%s`", brand.Cmd("config trust-key"))
	}
	token, err := feedToken(s.ctx, s.client)
	if err != nil {
		return err
	}
	s.fetch = plugins.Fetcher{Origin: s.origin, Token: token}
	return nil
}

func (s *pluginSession) saveLedger() error {
	if s.dryRun {
		return nil
	}
	return s.ledger.Save(s.ledgerPath)
}

// marketplaceURL is the feed's marketplace document for a platform.
func (s *pluginSession) marketplaceURL(platform, channel string) string {
	kind := "claude"
	if platform == platformAgentPlugins {
		kind = "agent-plugins"
	}
	return s.feed.BaseURL + "/" + kind + "/" + channel + "/marketplace.json"
}

// gitURL is the feed's git mirror: one repository per channel, and one per
// plugin for agents that install a single extension per repository.
func (s *pluginSession) gitURL(plugin, channel string) string {
	if plugin != "" {
		return s.feed.BaseURL + "/git/" + plugin + ".git"
	}
	return s.feed.BaseURL + "/git/" + channel + ".git"
}

func (s *pluginSession) marketplace(platform, channel string) (plugins.Marketplace, error) {
	key := platform + "/" + channel
	if m, ok := s.markets[key]; ok {
		return m, nil
	}
	raw, err := s.fetch.Get(s.ctx, s.marketplaceURL(platform, channel), 8<<20)
	if err != nil {
		return plugins.Marketplace{}, feedError(err)
	}
	m, err := plugins.ParseMarketplace(raw)
	if err != nil {
		return m, err
	}
	s.markets[key] = m
	return m, nil
}

// feedError explains a feed refusal; the gateway answers a plan that lacks
// private feeds with 402.
func feedError(err error) error {
	var se *plugins.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusPaymentRequired:
			return fmt.Errorf("your plan does not include private plugin feeds (%s)", se.Body)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("the feed refused the feed token (HTTP %d); run `%s` again", se.Status, brand.Cmd("login"))
		case http.StatusNotFound:
			return errors.New("the feed has no such channel or marketplace")
		}
	}
	return err
}

// parsePluginRef splits name[@version].
func parsePluginRef(ref string) (name, version string, err error) {
	name, version, _ = strings.Cut(ref, "@")
	if !pluginNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("%q is not a plugin name", textsafe.Clean(ref))
	}
	return name, version, nil
}

// pluginAgents resolves --to. Without it, every supported agent that is
// installed on this machine.
func (s *pluginSession) pluginAgents(to []string) ([]*pluginAgent, error) {
	var out []*pluginAgent
	seen := map[string]bool{}
	add := func(id string) error {
		a := pluginAgentByID(id)
		if a == nil {
			return fmt.Errorf("unknown agent %q (supported: %s)", textsafe.Clean(id), strings.Join(pluginAgentIDs(), ", "))
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, a)
		}
		return nil
	}
	for _, raw := range to {
		for _, id := range strings.Split(raw, ",") {
			id = strings.ToLower(strings.TrimSpace(id))
			switch id {
			case "":
			case "all":
				for _, d := range s.detectedAgents() {
					if err := add(d); err != nil {
						return nil, err
					}
				}
			default:
				if id == "gemini" {
					id = string(agentsync.Gemini)
				}
				if err := add(id); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(to) == 0 {
		for _, d := range s.detectedAgents() {
			if err := add(d); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no supported coding agent detected on this machine; name one with --to (%s)", strings.Join(pluginAgentIDs(), ", "))
	}
	return out, nil
}

func (s *pluginSession) detectedAgents() []string {
	var ids []string
	for _, id := range agentsync.Detected(agentsync.Env{Home: s.home, Project: s.home}) {
		if pluginAgentByID(string(id)) != nil {
			ids = append(ids, string(id))
		}
	}
	return ids
}

// writeFileAtomic replaces path with data through a temporary file, keeping
// the mode of an existing file.
func writeFileAtomic(path string, data []byte, defaultMode os.FileMode) error {
	mode := defaultMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".zuano-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// editSettings changes a JSON or JSONC settings file in place, leaving
// everything the change does not touch as it was.
func editSettings(path string, change func(data []byte) ([]byte, error)) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := change(data)
	if err != nil {
		return fmt.Errorf("%s: %w", textsafe.Clean(path), err)
	}
	if string(out) == string(data) {
		return nil
	}
	return writeFileAtomic(path, out, 0o600)
}

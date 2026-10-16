package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

func pluginFeedsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feeds",
		Short: "Register the gateway's plugin feeds in your coding agents",
	}
	cmd.AddCommand(pluginFeedsSetupCmd())
	return cmd
}

func pluginFeedsSetupCmd() *cobra.Command {
	var gatewayURL, channel string
	var to []string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Add the plugin marketplaces and credential helpers to your agents, without installing anything",
		Long: `Registers the feed in each agent so its own plugin commands can find and update
the bundles:

  claude    a marketplace in ~/.claude/settings.json whose headersHelper is
            'zuano auth header --json'
  codex, copilot
            the git marketplace, added with the agent's own command
  vscode    the git marketplace in VS Code's user settings, and a git
            credential helper for the gateway's /m/ paths in your global git
            config (VS Code runs git itself)
  opencode  the feed's npm scope, with a long-lived read-only feed key made for
            this device, in ~/.config/opencode/.npmrc (npm cannot call a helper;
            revoke the key under Settings, API keys)

Cursor, Gemini CLI, Cline and Windsurf need no registration: installs go through
'zuano plugin install'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newPluginSession(cmd, gatewayURL, channel, false)
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
			var failed bool
			for _, a := range agents {
				note, err := s.feedsSetup(a.id, channel)
				switch {
				case err != nil:
					fmt.Fprintf(s.err, "%s: %v\n", a.id, err)
					failed = true
				default:
					fmt.Fprintf(s.out, "%s: %s\n", a.id, note)
				}
			}
			if failed {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	addPluginFlags(cmd, &gatewayURL, &channel, &to, "stable")
	return cmd
}

func (s *pluginSession) feedsSetup(agent, channel string) (string, error) {
	switch agent {
	case "claude":
		m, err := s.marketplace(platformClaude, channel)
		if err != nil {
			return "", err
		}
		err = editSettings(filepath.Join(s.home, ".claude", "settings.json"), func(d []byte) ([]byte, error) {
			return agentsync.SetJSONMember(d, []string{"extraKnownMarketplaces"}, m.Name, map[string]any{
				"source":        map[string]any{"source": "url", "url": s.marketplaceURL(platformClaude, channel)},
				"headersHelper": brand.Cmd("auth header --json"),
			})
		})
		return "registered marketplace " + textsafe.Clean(m.Name), err
	case "codex", "copilot":
		if err := s.requireGitMirror(); err != nil {
			return "", err
		}
		if err := s.run(s.gitEnv(), agent, "plugin", "marketplace", "add", s.gitURL("", channel)); err != nil {
			return "", err
		}
		return "registered the git marketplace", nil
	case "vscode":
		if err := s.requireGitMirror(); err != nil {
			return "", err
		}
		if _, err := installVSCode(s, nil, &pluginTarget{channel: channel}); err != nil {
			return "", err
		}
		scope := "credential." + s.origin + "/m/."
		exe, _ := os.Executable()
		if exe == "" {
			exe = brand.Name
		}
		helper := "!'" + strings.ReplaceAll(exe, "'", `'\''`) + "' auth git-credential"
		for _, kv := range [][2]string{{scope + "helper", helper}, {scope + "useHttpPath", "true"}} {
			if err := s.run(nil, "git", "config", "--global", kv[0], kv[1]); err != nil {
				return "", err
			}
		}
		return "registered the git marketplace and the git credential helper", nil
	case "opencode":
		if s.feed.NPMScope == "" {
			return "", fmt.Errorf("the gateway's feed has no npm scope")
		}
		if err := s.ensureNPMRC(opencodeConfigDir(s.home)); err != nil {
			return "", err
		}
		return "wrote the npm scope and feed key to " + textsafe.Clean(filepath.Join(opencodeConfigDir(s.home), ".npmrc")), nil
	}
	return "nothing to register; install with `" + brand.Cmd("plugin install <name> --to "+agent) + "`", nil
}

const (
	npmrcBegin = "# zuano-managed:begin"
	npmrcEnd   = "# zuano-managed:end"
)

// ensureNPMRC makes sure dir/.npmrc points the feed's npm scope at the
// gateway with a feed key. An existing block for the same registry is kept, so
// a second run mints no second key.
func (s *pluginSession) ensureNPMRC(dir string) error {
	registry := s.feed.BaseURL + "/npm/"
	scope := "@" + strings.TrimPrefix(s.feed.NPMScope, "@")
	path := filepath.Join(dir, ".npmrc")
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(current)
	if strings.Contains(managedBlock(text), scope+":registry="+registry) {
		return nil
	}
	var res feedTokenResponse
	host, _ := os.Hostname()
	if _, err := gatewayJSON(s.ctx, s.client, http.MethodPost, "/api/cli/feed-token", feedTokenRequest{Kind: "api_key", Name: strings.TrimSpace(host + " (" + brand.Name + ")")}, &res); err != nil {
		return err
	}
	if res.Token == "" {
		return fmt.Errorf("the gateway returned no feed key")
	}
	u, err := url.Parse(registry)
	if err != nil {
		return err
	}
	block := strings.Join([]string{
		npmrcBegin,
		scope + ":registry=" + registry,
		"//" + u.Host + u.Path + ":_authToken=" + res.Token,
		npmrcEnd,
	}, "\n") + "\n"
	return writeFileAtomic(path, []byte(replaceBlock(text, block)), 0o600)
}

func managedBlock(text string) string {
	start := strings.Index(text, npmrcBegin)
	end := strings.Index(text, npmrcEnd)
	if start < 0 || end < start {
		return ""
	}
	return text[start:end]
}

func replaceBlock(text, block string) string {
	start := strings.Index(text, npmrcBegin)
	end := strings.Index(text, npmrcEnd)
	if start >= 0 && end > start {
		rest := strings.TrimPrefix(text[end+len(npmrcEnd):], "\n")
		return text[:start] + block + rest
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + block
}

package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/packs"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const packFile = "pack.yaml"

const packTemplate = `format: ` + packs.Format + `
name: my-pack
version: 0.1.0
displayName: My pack
summary: What this pack sets up, in one sentence.
category: engineering
params:
  - key: approver_group
    label: Group that approves outgoing changes
    type: group
    default: { create: Approvers }
connectors:
  - slug: github
    required: true
    credentialScope: org
    why: Reads repositories and opens pull requests.
groups:
  - key: approvers
    from: params.approver_group
rules:
  - name: conventional-commits
    description: Commit message format.
    alwaysApply: true
    body: |
      Write commit messages as type(scope): description.
risk:
  matrix:
    - { service: github, risk: write, mode: approval }
`

func readPack(dir string) (*packs.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, packFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", textsafe.Clean(filepath.Join(dir, packFile)), err)
	}
	m, err := packs.Parse(data)
	if err != nil {
		return nil, err
	}
	return m, m.Validate()
}

func packInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init <dir>",
		Short: "Create a pack.yaml to start from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := filepath.Join(args[0], packFile)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists; nothing was written", textsafe.Clean(path))
			}
			if err := os.MkdirAll(args[0], 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(packTemplate), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s. Edit it, then run `%s`.\n", textsafe.Clean(path), brand.Cmd("pack validate "+args[0]))
			return nil
		},
	}
}

// remoteValidate asks the gateway to resolve the pack's references. It
// returns nil without a gateway to ask.
func remoteValidate(cmd *cobra.Command, gatewayURL string, m *packs.Manifest) error {
	if Offline() {
		return nil
	}
	if gw, err := resolveGatewayURLOrFailClosed(gatewayURL); err != nil || gw == "" {
		return nil
	}
	c, err := approvalsClient(gatewayURL)
	if err != nil {
		return err
	}
	var res struct {
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := gatewayJSON(cmd.Context(), c, http.MethodPost, "/api/packs/validate", map[string]any{"manifest": m}, &res); err != nil {
		return err
	}
	if len(res.Errors) == 0 {
		return nil
	}
	var errs []error
	for _, e := range res.Errors {
		errs = append(errs, fmt.Errorf("%s: %s", textsafe.Clean(e.Path), textsafe.Clean(e.Message)))
	}
	return errors.Join(errs...)
}

func packValidateCmd() *cobra.Command {
	var gatewayURL string
	cmd := &cobra.Command{
		Use:   "validate <dir>",
		Short: "Check a pack.yaml, and its references against the gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := readPack(args[0])
			if err != nil {
				return err
			}
			if err := remoteValidate(cmd, gatewayURL, m); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s is valid.\n", textsafe.Clean(m.Name), textsafe.Clean(m.Version))
			return nil
		},
	}
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	return cmd
}

func packPublishCmd() *cobra.Command {
	var f approvalFlags
	var channel string
	cmd := &cobra.Command{
		Use:   "publish <dir>",
		Short: "Publish a pack for your organisation",
		Long: `Uploads the pack as a private pack of your organisation. A pack that loosens a
control compared with the defaults waits for an administrator's review before
anyone can install it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentPack(brand.Cmd("pack publish")); err != nil {
				return err
			}
			if channel != "stable" && channel != "beta" {
				return fmt.Errorf("invalid --channel %q: use stable or beta", textsafe.Clean(channel))
			}
			m, err := readPack(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var created struct {
				Pack struct {
					Slug string `json:"slug"`
				} `json:"pack"`
			}
			if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/packs", map[string]any{"manifest": m}, &created); err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, packPath(created.Pack.Slug, "publish"), map[string]any{"version": m.Version, "channel": channel}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Published %s %s on the %s channel.\n", textsafe.Clean(m.Name), textsafe.Clean(m.Version), channel)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&channel, "channel", "stable", "Release channel: stable or beta")
	return cmd
}

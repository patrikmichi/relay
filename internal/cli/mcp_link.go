package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

var linkTargets = []string{"claude-ai", "chatgpt"}

// connectLinks mirrors GET /api/org/{org}/connect-links.
type connectLinks struct {
	ClaudeAI struct {
		URL          string   `json:"url"`
		DirectoryURL string   `json:"directory_url"`
		Endpoint     string   `json:"endpoint"`
		Steps        []string `json:"steps"`
	} `json:"claude_ai"`
	ChatGPT struct {
		Endpoint string   `json:"endpoint"`
		Auth     string   `json:"auth"`
		AdminURL string   `json:"admin_url"`
		Steps    []string `json:"steps"`
	} `json:"chatgpt"`
}

func mcpLinkCmd() *cobra.Command {
	var f approvalFlags
	var org string
	var directory bool
	cmd := &cobra.Command{
		Use:   "link <claude-ai|chatgpt>",
		Short: "Print the link or steps to add the gateway as a connector in Claude or ChatGPT (admins)",
		Long: `Print what an organisation admin needs to add the gateway as a connector in
the Claude or ChatGPT apps, taken from the gateway so the endpoint URLs match
its configuration. For Claude the output is a prefilled link that opens the
add-connector dialog of the organisation settings; ChatGPT has no such link, so
the endpoint, the sign-in mode and the admin steps are printed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			if !slices.Contains(linkTargets, target) {
				return fmt.Errorf("unknown target %q: use %s", textsafe.Clean(target), strings.Join(linkTargets, " or "))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var links connectLinks
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/org/"+url.PathEscape(org)+"/connect-links", nil, &links)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			return printConnectLinks(cmd.OutOrStdout(), links, target, directory)
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&org, "org", currentOrg, "Organisation id (default: the one your credential belongs to)")
	cmd.Flags().BoolVar(&directory, "directory", false, "For claude-ai, use the directory connector's endpoint")
	return cmd
}

func printConnectLinks(out io.Writer, l connectLinks, target string, directory bool) error {
	steps := l.ChatGPT.Steps
	switch target {
	case "claude-ai":
		link := l.ClaudeAI.URL
		if directory {
			link = l.ClaudeAI.DirectoryURL
		}
		if link == "" {
			return errors.New("the gateway sent no Claude link for this endpoint")
		}
		fmt.Fprintf(out, "Open this link as an owner of your Claude organisation:\n  %s\n", textsafe.Clean(link))
		if l.ClaudeAI.Endpoint != "" {
			fmt.Fprintf(out, "Connector URL: %s\n", textsafe.Clean(l.ClaudeAI.Endpoint))
		}
		steps = l.ClaudeAI.Steps
	default:
		if l.ChatGPT.Endpoint == "" {
			return errors.New("the gateway sent no ChatGPT endpoint")
		}
		fmt.Fprintf(out, "Connector URL: %s\n", textsafe.Clean(l.ChatGPT.Endpoint))
		if l.ChatGPT.Auth != "" {
			fmt.Fprintf(out, "Sign-in:       %s\n", textsafe.Clean(l.ChatGPT.Auth))
		}
		if l.ChatGPT.AdminURL != "" {
			fmt.Fprintf(out, "Admin console: %s\n", textsafe.Clean(l.ChatGPT.AdminURL))
		}
	}
	for i, step := range steps {
		fmt.Fprintf(out, "  %d. %s\n", i+1, textsafe.Clean(step))
	}
	return nil
}

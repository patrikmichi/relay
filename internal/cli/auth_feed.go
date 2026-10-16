package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/keychain"
)

// feedTokenSkew is how long before its expiry a cached feed token stops being
// used.
const feedTokenSkew = 5 * time.Minute

// feedTokenRequest and feedTokenResponse are the body and answer of
// POST /api/cli/feed-token.
type feedTokenRequest struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

type feedTokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
}

// feedToken returns a token that can read the plugin feeds. It mints one
// through the session and keeps it in the keychain until just before it
// expires, so the helpers git and the agents call on every request do not each
// hit the gateway.
func feedToken(ctx context.Context, c *client.Client) (string, error) {
	origin, err := config.NormalizeGatewayURL(c.GatewayURL)
	if err != nil {
		return "", err
	}
	email := c.Email()
	if email != "" {
		if cached, err := keychain.ReadProfileToken(keychain.ProfileFeed, origin, email); err == nil && cached.AccessToken != "" && time.Until(time.Unix(cached.ExpiresAt, 0)) > feedTokenSkew {
			return cached.AccessToken, nil
		}
	}
	var res feedTokenResponse
	if _, err := gatewayJSON(ctx, c, http.MethodPost, "/api/cli/feed-token", feedTokenRequest{}, &res); err != nil {
		return "", err
	}
	if res.Token == "" {
		return "", errors.New("the gateway returned no feed token")
	}
	if email != "" && res.ExpiresIn > 0 {
		// A keychain that cannot store it only costs a request next time.
		_ = keychain.WriteProfileToken(keychain.ProfileFeed, origin, email, keychain.TokenData{
			AccessToken: res.Token, ExpiresIn: res.ExpiresIn, ExpiresAt: time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).Unix(),
		})
	}
	return res.Token, nil
}

// AuthCmd returns `zuano auth`, the helpers that let coding agents and git
// read the gateway's private plugin feeds.
func AuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Credential helpers for the gateway's plugin feeds",
		Long: `Helpers that coding agents and git call to authenticate to the gateway's plugin
feeds without a secret in a config file. They mint a short-lived feed token with
your session, cache it in the keychain and answer only for the gateway you
configured.`,
	}
	cmd.AddCommand(authHeaderCmd(), authGitCredentialCmd())
	return cmd
}

func authHeaderCmd() *cobra.Command {
	var f approvalFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "header",
		Short: "Print the Authorization header for the plugin feeds",
		Long: `Prints "Authorization: Bearer <feed token>". With --json it prints
{"Authorization": "Bearer <feed token>"}, the form an agent's headersHelper
setting reads.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			token, err := feedToken(cmd.Context(), c)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"Authorization": "Bearer " + token})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Authorization: Bearer %s\n", token)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print a JSON object of headers")
	return cmd
}

func authGitCredentialCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git-credential <get|store|erase>",
		Short: "Git credential helper for the gateway's git plugin mirror",
		Long: `Speaks the git credential helper protocol. 'get' answers with a feed token for
requests to the configured gateway's /m/ paths and with nothing for any other
host, so git asks its other helpers. 'store' and 'erase' do nothing: the token
is minted on demand.

Configure it for the gateway only:
  git config --global credential.<gateway>/m/.helper '!zuano auth git-credential'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "get":
				return gitCredentialGet(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
			case "store", "erase":
				_, _ = io.Copy(io.Discard, io.LimitReader(cmd.InOrStdin(), 1<<16))
				return nil
			}
			return fmt.Errorf("unknown operation %q: use get, store or erase", args[0])
		},
	}
	return cmd
}

func gitCredentialGet(ctx context.Context, in io.Reader, out io.Writer) error {
	attrs := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(in, 1<<16))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	gateway, err := config.GatewayURL()
	if err != nil || gateway == "" || Offline() {
		return nil
	}
	origin, err := config.NormalizeGatewayURL(gateway)
	if err != nil {
		return nil
	}
	want, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(attrs["protocol"], want.Scheme) || !strings.EqualFold(attrs["host"], want.Host) {
		return nil
	}
	if p := attrs["path"]; p != "" && !strings.HasPrefix(p, "m/") {
		return nil
	}
	c, err := resolveClient(origin)
	if err != nil {
		return err
	}
	token, err := feedToken(ctx, c)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "username=%s\npassword=%s\n", "zuano", token)
	return nil
}

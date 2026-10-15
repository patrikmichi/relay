package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

var errTokenRevoked = errors.New("token expired or revoked — run `relay login` to re-authenticate")

// openBrowserURL and connectPollInterval are variables so tests can stand in
// for the browser and shorten the wait.
var (
	openBrowserURL      = browser.OpenURL
	connectPollInterval = 2 * time.Second
)

const googleWorkspaceID = "google-workspace"

type connectFlags struct {
	fields     []string
	label      string
	oauth      bool
	noBrowser  bool
	reconnect  bool
	wait       time.Duration
	gatewayURL string
}

// ConnectCmd returns `relay connect <slug>`.
func ConnectCmd() *cobra.Command {
	var f connectFlags
	cmd := &cobra.Command{
		Use:   "connect <slug>",
		Short: "Link your own account to a gateway connector",
		Long: `Link your account for a connector listed by 'relay connectors'.

API-key connectors prompt for each credential field without echoing it
(or take --field KEY=VALUE, or RELAY_CONNECT_<KEY> from the environment) and
store it on the gateway. OAuth connectors open the gateway's consent flow in
your browser and wait until the connection shows up.

Some gateways only accept connection changes from a signed-in browser
session. relay detects that and opens the gateway's connection page instead,
then waits for the account to be linked.

Credentials are sent to your gateway only; relay never writes them to disk.

Examples:
  relay connect clockify
  relay connect notion --no-browser
  relay connect intervals-icu --field INTERVALS_API_KEY=... --label coach`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnect(cmd, args[0], f)
		},
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.fields, "field", nil, "Credential field as KEY=VALUE (repeatable)")
	fl.StringVar(&f.label, "label", "", "Account label for multi-account connectors")
	fl.BoolVar(&f.oauth, "oauth", false, "Use the OAuth consent flow even when the gateway does not advertise it")
	fl.BoolVar(&f.noBrowser, "no-browser", false, "Print the URL instead of opening a browser")
	fl.BoolVar(&f.reconnect, "reconnect", false, "Connect again even if the account already looks linked")
	fl.DurationVar(&f.wait, "wait", 5*time.Minute, "How long to wait for a browser flow to finish")
	fl.StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	return cmd
}

type connectionField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret"`
	Optional bool   `json:"optional"`
}

type connectionEntry struct {
	ServiceID   string            `json:"serviceId"`
	ServiceName string            `json:"serviceName"`
	AuthType    string            `json:"authType"`
	Connected   bool              `json:"connected"`
	Fields      []connectionField `json:"fields"`
	OrgAllowed  bool              `json:"orgAllowed"`
	OAuth       *struct {
		State      string `json:"state"`
		ConnectURL string `json:"connectUrl"`
	} `json:"oauth"`
}

// fetchConnection reads the caller's entry from GET /api/connections. ok is
// false when the gateway refuses non-browser callers or lists no such entry.
func fetchConnection(ctx context.Context, c *client.Client, slug string) (entry *connectionEntry, ok bool, err error) {
	resp, err := c.GetContext(ctx, "/api/connections")
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, nil
	}
	var doc struct {
		Connections []connectionEntry `json:"connections"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&doc); err != nil {
		return nil, false, nil
	}
	for i := range doc.Connections {
		if doc.Connections[i].ServiceID == slug {
			return &doc.Connections[i], true, nil
		}
	}
	return nil, true, nil
}

func runConnect(cmd *cobra.Command, slug string, f connectFlags) error {
	gURL, err := resolveGatewayURLOrFailClosed(f.gatewayURL)
	if err != nil {
		return err
	}
	c, err := resolveClient(gURL)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	info, err := fetchIntegrations(c, ctx)
	if err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	if info == nil {
		return errTokenRevoked
	}
	var svc *DiscoveryService
	for i := range info.Services {
		if info.Services[i].ID == slug {
			svc = &info.Services[i]
		}
	}
	switch {
	case svc == nil:
		return fmt.Errorf("no connector %q on this gateway; see `relay connectors search %s`", slug, slug)
	case !svc.Accessible:
		return fmt.Errorf("your account has no access to %s; ask a gateway admin to enable it", slug)
	case !takesCredentials(*svc):
		fmt.Fprintf(out, "%s needs no credentials of yours.\n", slug)
		return nil
	}

	if !f.reconnect {
		if l := fetchServiceLink(ctx, c, *svc); l.AccountLinked {
			fmt.Fprintf(out, "%s is already connected. Use --reconnect to link it again.\n", slug)
			return nil
		}
	}

	entry, sessionAPI, err := fetchConnection(ctx, c, slug)
	if err != nil {
		return err
	}
	gw := strings.TrimRight(gURL, "/")
	started := time.Now()
	deadline := started.Add(f.wait)

	switch {
	case sessionAPI && entry != nil && entry.OAuth != nil:
		if entry.OAuth.State == "needs_app" {
			return fmt.Errorf("%s has no OAuth app registered on this gateway; an admin must set one up first", slug)
		}
		return connectInBrowser(ctx, cmd, c, *svc, gw+entry.OAuth.ConnectURL, true, f, deadline)
	case sessionAPI && entry != nil && len(entry.Fields) > 0:
		return connectWithKey(ctx, cmd, c, *entry, f)
	case slug == googleWorkspaceID:
		return connectInBrowser(ctx, cmd, c, *svc, gw+"/api/connections/google/authorize", false, f, deadline)
	case f.oauth:
		return connectInBrowser(ctx, cmd, c, *svc, gw+"/api/connectors/oauth/start?service="+url.QueryEscape(slug), true, f, deadline)
	}
	fmt.Fprintf(out, "%s is linked from the gateway's connection page.\n", slug)
	return connectInBrowser(ctx, cmd, c, *svc, gw+"/settings/connections", false, f, deadline)
}

// connectInBrowser sends the user to target and waits until the gateway
// reports the account linked.
func connectInBrowser(ctx context.Context, cmd *cobra.Command, c *client.Client, svc DiscoveryService, target string, oauth bool, f connectFlags, deadline time.Time) error {
	out := cmd.OutOrStdout()
	if f.noBrowser {
		fmt.Fprintf(out, "Open this URL in a browser signed in to the gateway:\n  %s\n", target)
	} else if err := openBrowserURL(target); err != nil {
		fmt.Fprintf(out, "Could not open a browser (%v). Open this URL yourself:\n  %s\n", err, target)
	} else {
		fmt.Fprintf(out, "Opened %s\n", target)
	}
	fmt.Fprintf(out, "Waiting for %s to be connected (Ctrl-C to stop)...\n", svc.ID)

	ticker := time.NewTicker(connectPollInterval)
	defer ticker.Stop()
	for {
		if connected, err := pollConnected(ctx, c, svc, oauth); err != nil {
			return err
		} else if connected {
			fmt.Fprintf(out, "%s is connected.\n", svc.ID)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is not connected yet; finish the flow in the browser, then check `relay connectors --status`", svc.ID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollConnected checks the OAuth status endpoint when the gateway lets a
// bearer caller use it, and the credential link otherwise.
func pollConnected(ctx context.Context, c *client.Client, svc DiscoveryService, oauth bool) (bool, error) {
	if oauth {
		resp, err := c.GetContext(ctx, "/api/connectors/oauth/status?service="+url.QueryEscape(svc.ID))
		if err == nil {
			var doc struct {
				State string `json:"state"`
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && json.Unmarshal(body, &doc) == nil {
				switch doc.State {
				case "connected":
					return true, nil
				case "needs_app":
					return false, fmt.Errorf("%s has no OAuth app registered on this gateway; an admin must set one up first", svc.ID)
				}
			}
		}
	}
	return fetchServiceLink(ctx, c, svc).AccountLinked, nil
}

// connectWithKey collects the connector's credential fields and stores them
// on the gateway.
func connectWithKey(ctx context.Context, cmd *cobra.Command, c *client.Client, entry connectionEntry, f connectFlags) error {
	out := cmd.OutOrStdout()
	given := map[string]string{}
	for _, kv := range f.fields {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("invalid --field %q: want KEY=VALUE", kv)
		}
		given[k] = v
	}
	values := map[string]string{}
	for _, field := range entry.Fields {
		v, ok := given[field.Key]
		if !ok {
			v, ok = os.LookupEnv("RELAY_CONNECT_" + field.Key)
		}
		if !ok && field.Optional && !isInteractiveTerminal(os.Stdin) {
			continue
		}
		if !ok {
			var err error
			if v, err = promptField(cmd, field); err != nil {
				return err
			}
		}
		if v == "" && !field.Optional {
			return fmt.Errorf("%s is required", field.Key)
		}
		if v != "" {
			values[field.Key] = v
		}
	}
	body := map[string]any{"service": entry.ServiceID, "fields": values}
	if f.label != "" {
		body["label"] = f.label
	}
	raw, _ := json.Marshal(body)
	resp, err := c.PostContext(ctx, "/api/connections/keys", "application/json", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var doc struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &doc)
		msg := doc.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("the gateway rejected the credentials: %s", msg)
	}
	fmt.Fprintf(out, "%s is connected.\n", entry.ServiceID)
	return nil
}

// promptField asks for one credential on the terminal; secrets are read
// without echo. Without a terminal it fails rather than hang.
func promptField(cmd *cobra.Command, field connectionField) (string, error) {
	if !isInteractiveTerminal(os.Stdin) {
		return "", fmt.Errorf("%s needs a value but there is no terminal; pass --field %s=VALUE or set RELAY_CONNECT_%s", field.Key, field.Key, field.Key)
	}
	label := field.Label
	if label == "" {
		label = field.Key
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s: ", label)
	if field.Secret {
		v, err := readSecret(os.Stdin)
		fmt.Fprintln(out)
		return v, err
	}
	var v string
	_, err := fmt.Fscanln(os.Stdin, &v)
	if err != nil && v == "" && !field.Optional {
		return "", err
	}
	return v, nil
}

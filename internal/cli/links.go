package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// linksPollInterval is how often `links show --wait` asks; tests shorten it.
var linksPollInterval = time.Second

// LinksCmd returns `zuano links`: connect links a person opens to connect an
// account for an end user.
func LinksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "links",
		Short: "Make links that connect an end user's account, and follow them",
		Long: `A connect link takes a person to a page where they connect a service: an OAuth
consent, or the fields an API-key service needs. It expires (15 minutes by
default, at most an hour). The callback address must be on the project's list
of allowed callbacks.`,
	}
	cmd.AddCommand(linksCreateCmd(), linksShowCmd())
	return cmd
}

func linksCreateCmd() *cobra.Command {
	var f approvalFlags
	var user, service, authConfig, alias, callback, expires string
	var scopes []string
	cmd := &cobra.Command{
		Use:   "create --user ID --service S",
		Short: "Make a connect link",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if user == "" || service == "" {
				return errors.New("--user and --service are required")
			}
			body := map[string]any{"userId": user, "service": service}
			for k, v := range map[string]string{"authConfigId": authConfig, "alias": alias, "callbackUrl": callback} {
				if v != "" {
					body[k] = v
				}
			}
			if len(scopes) > 0 {
				body["scopes"] = scopes
			}
			if expires != "" {
				d, err := time.ParseDuration(expires)
				if err != nil || d < time.Minute || d > time.Hour {
					return errors.New("--expires-in must be a duration between 1m and 1h")
				}
				body["expiresIn"] = int(d.Seconds())
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				ID        string `json:"id"`
				URL       string `json:"url"`
				ExpiresAt string `json:"expiresAt"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, builderAPI+"/connect-links", body, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			u, err := url.Parse(doc.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return errors.New("the gateway returned a link that is not an address")
			}
			fmt.Fprintf(out, "Link %s, expires %s\n", textsafe.Clean(doc.ID), textsafe.Clean(doc.ExpiresAt))
			presentLink(out, doc.URL, false, "Send this to the person who connects the account")
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&user, "user", "", "The end user's external id")
	cmd.Flags().StringVar(&service, "service", "", "The service to connect")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "Connect through this auth config")
	cmd.Flags().StringVar(&alias, "alias", "", "Alias for the new account")
	cmd.Flags().StringVar(&callback, "callback-url", "", "Where to send the person afterwards; must be on the project's allowlist")
	cmd.Flags().StringSliceVar(&scopes, "scopes", nil, "Fewer scopes than the config's (narrowing only)")
	cmd.Flags().StringVar(&expires, "expires-in", "", "How long the link works, 1m to 1h (default 15m)")
	return cmd
}

func linksShowCmd() *cobra.Command {
	var f approvalFlags
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "show <lnk_id>",
		Short: "Show whether a link was used; --wait follows it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "lnk_", "a connect link id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			deadline := time.Now().Add(wait)
			for {
				var doc struct {
					Status             string `json:"status"`
					ConnectedAccountID string `json:"connectedAccountId"`
					Error              string `json:"error"`
				}
				raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, builderAPI+"/connect-links/"+url.PathEscape(id), nil, &doc)
				if err != nil {
					return err
				}
				done := doc.Status != "initiated"
				if done || wait == 0 || time.Now().After(deadline) {
					out := cmd.OutOrStdout()
					if f.jsonOut {
						printRaw(out, raw)
					} else {
						line := "Link " + id + ": " + textsafe.Clean(doc.Status)
						if doc.ConnectedAccountID != "" {
							line += ", account " + textsafe.Clean(doc.ConnectedAccountID)
						}
						if doc.Error != "" {
							line += ", " + textsafe.Strict(doc.Error)
						}
						fmt.Fprintln(out, line)
					}
					switch doc.Status {
					case "failed", "expired":
						return &ExitError{Code: 1, Msg: "the link " + doc.Status}
					case "initiated":
						if wait > 0 {
							return &ExitError{Code: exitWaitTimeout, Msg: "the link was not used within " + wait.String()}
						}
					}
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(linksPollInterval):
				}
			}
		},
	}
	f.bind(cmd, "Print the link as JSON")
	cmd.Flags().DurationVar(&wait, "wait", 0, "Keep asking until the link is used, up to this long (exit 17 if not)")
	return cmd
}

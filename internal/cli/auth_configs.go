package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

type authConfigDoc struct {
	ID                string   `json:"id"`
	ProjectID         string   `json:"projectId"`
	Service           string   `json:"service"`
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	Scheme            string   `json:"scheme"`
	CredentialSource  string   `json:"credentialSource"`
	Scopes            []string `json:"scopes"`
	RedirectHost      string   `json:"redirectHost"`
	ShareWithProjects bool     `json:"shareWithProjects"`
	IsDefault         bool     `json:"isDefault"`
	Enabled           bool     `json:"enabled"`
	SecretFields      []struct {
		Key       string `json:"key"`
		Set       bool   `json:"set"`
		UpdatedAt string `json:"updatedAt"`
		Hint      string `json:"hint"`
	} `json:"secretFields"`
}

func (a authConfigDoc) scope() string {
	if a.ProjectID != "" {
		return a.ProjectID
	}
	return "org"
}

// AuthConfigsCmd returns `zuano auth-configs`: how each service authenticates,
// with the app credentials that go with it. A secret is never an argument, is
// never printed and cannot be set from an agent session.
func AuthConfigsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth-configs",
		Short: "Manage how a service authenticates: platform app or your own credentials",
		Long: `An auth config says how one service authenticates: the platform's own app
('managed'), or your organisation's or project's ('custom') with a scheme (oauth2,
api_key, bearer, basic) and the credentials that go with it. Accounts connect
through a config; the project default, then the organisation default, then the
managed app decide which one a connection uses.

Secrets are given as a JSON object on standard input or in a file (--from-file),
never in a flag, and are write-only: no command prints one, and the gateway
answers only whether each field is set. An agent session cannot create a config
with secrets, set or rotate one (exit 10).`,
	}
	cmd.AddCommand(acListCmd(), acShowCmd(), acFieldsCmd(), acCreateCmd(), acUpdateCmd(), acToggleCmd("enable"), acToggleCmd("disable"), acDeleteCmd(), acSecretsCmd())
	return cmd
}

func acPath(id string, rest ...string) string {
	return builderAPI + "/auth-configs/" + url.PathEscape(id) + strings.Join(rest, "")
}

func acRows(list []authConfigDoc) [][]string {
	var rows [][]string
	for _, a := range list {
		state := "on"
		if !a.Enabled {
			state = "off"
		}
		def := ""
		if a.IsDefault {
			def = "default"
		}
		rows = append(rows, []string{a.ID, a.Service, a.Name, a.Type + "/" + a.Scheme, a.scope(), state, def})
	}
	return rows
}

func acListCmd() *cobra.Command {
	var f approvalFlags
	var service, search, project, cursor string
	var showDisabled bool
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List auth configs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := map[string]string{"service": service, "search": search, "projectId": project, "cursor": cursor}
			if showDisabled {
				q["showDisabled"] = "true"
			}
			if limit > 0 {
				q["limit"] = strconv.Itoa(limit)
			}
			var doc struct {
				Items      []authConfigDoc `json:"items"`
				NextCursor string          `json:"nextCursor"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(builderAPI+"/auth-configs", q), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintln(out, "No auth configs.")
				return nil
			}
			printRows(out, []string{"ID", "SERVICE", "NAME", "TYPE", "SCOPE", "STATE", ""}, acRows(doc.Items))
			if doc.NextCursor != "" {
				fmt.Fprintf(out, "\nMore: --cursor %s\n", textsafe.Clean(doc.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	cmd.Flags().StringVar(&service, "service", "", "Only this service")
	cmd.Flags().StringVar(&search, "search", "", "Words in the name")
	cmd.Flags().StringVar(&project, "project", "", "A project (prj_...); default: the organisation's")
	cmd.Flags().BoolVar(&showDisabled, "show-disabled", false, "Include disabled configs")
	cmd.Flags().IntVar(&limit, "limit", 0, "Page size, at most 200")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue from a previous page")
	return cmd
}

func acShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <ac_id>",
		Short: "Show an auth config; secrets appear only as set or not set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ac_", "an auth config id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				AuthConfig authConfigDoc `json:"authConfig"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, acPath(id), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			a := doc.AuthConfig
			fmt.Fprintf(out, "%s  %s (%s)\n", textsafe.Clean(a.ID), textsafe.Strict(a.Name), textsafe.Clean(a.Service))
			fmt.Fprintf(out, "  type %s, scheme %s, credentials from %s, scope %s\n", a.Type, a.Scheme, orDash(a.CredentialSource), a.scope())
			fmt.Fprintf(out, "  enabled %t, default %t, shared with projects %t, redirect host %s\n", a.Enabled, a.IsDefault, a.ShareWithProjects, orDash(a.RedirectHost))
			if len(a.Scopes) > 0 {
				fmt.Fprintf(out, "  scopes: %s\n", textsafe.Strict(strings.Join(a.Scopes, " ")))
			}
			for _, s := range a.SecretFields {
				state := "not set"
				if s.Set {
					state = "set " + textsafe.Clean(s.UpdatedAt)
				}
				hint := ""
				if s.Hint != "" {
					hint = " (" + textsafe.Strict(s.Hint) + "...)"
				}
				fmt.Fprintf(out, "  secret %s: %s%s\n", textsafe.Strict(s.Key), state, hint)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the config as JSON")
	return cmd
}

func acFieldsCmd() *cobra.Command {
	var f approvalFlags
	var scheme string
	var requiredOnly bool
	cmd := &cobra.Command{
		Use:   "fields <service>",
		Short: "Show what a service needs to create an auth config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.ContainsAny(args[0], "/?#% ") || args[0] == "" {
				return fmt.Errorf("%q is not a service", textsafe.Clean(args[0]))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := map[string]string{"scheme": scheme}
			if requiredOnly {
				q["requiredOnly"] = "true"
			}
			var doc struct {
				Scheme string `json:"scheme"`
				Fields []struct {
					Key      string `json:"key"`
					Label    string `json:"label"`
					Required bool   `json:"required"`
					Secret   bool   `json:"secret"`
					Kind     string `json:"kind"`
				} `json:"fields"`
				Scopes struct {
					Available []string `json:"available"`
					Default   []string `json:"default"`
				} `json:"scopes"`
				RedirectURI      string `json:"redirectUri"`
				ManagedAvailable bool   `json:"managedAvailable"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(builderAPI+"/services/"+url.PathEscape(args[0])+"/auth-config-fields", q), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "%s with scheme %s; managed app available: %t\n", textsafe.Clean(args[0]), orDash(doc.Scheme), doc.ManagedAvailable)
			var rows [][]string
			for _, fl := range doc.Fields {
				flags := ""
				if fl.Required {
					flags = "required"
				}
				if fl.Secret {
					flags = strings.TrimSpace(flags + " secret")
				}
				rows = append(rows, []string{fl.Key, fl.Kind, flags, fl.Label})
			}
			if len(rows) > 0 {
				fmt.Fprintln(out)
				printRows(out, []string{"FIELD", "KIND", "", "LABEL"}, rows)
			}
			if doc.RedirectURI != "" {
				fmt.Fprintf(out, "\nRegister this redirect URI with the provider: %s\n", textsafe.Strict(doc.RedirectURI))
			}
			if len(doc.Scopes.Available) > 0 {
				fmt.Fprintf(out, "Scopes: %s\n", textsafe.Strict(strings.Join(doc.Scopes.Available, " ")))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the descriptor as JSON")
	cmd.Flags().StringVar(&scheme, "scheme", "", "oauth2, api_key, bearer or basic")
	cmd.Flags().BoolVar(&requiredOnly, "required-only", false, "Only the fields that must be given")
	return cmd
}

func acCreateCmd() *cobra.Command {
	var f approvalFlags
	var service, name, typ, scheme, source, redirect, project, fromFile string
	var scopes []string
	var share, def, secretsStdin bool
	cmd := &cobra.Command{
		Use:   "create --service S --name N --type managed|custom --scheme oauth2|api_key|bearer|basic",
		Short: "Create an auth config",
		Long: `Creates an auth config. For a custom config give its credentials as a JSON
object on standard input (--secrets-stdin) or in a file (--from-file), with the
keys 'zuano auth-configs fields <service>' lists:

  echo '{"client_id":"...","client_secret":"..."}' | zuano auth-configs create ... --secrets-stdin

They are sealed on the gateway and never shown again. A 'managed' config uses
the platform's app and takes none. Refused in an agent session (exit 10).`,
		Example: `  zuano auth-configs create --service github --name "Our GitHub app" --type custom --scheme oauth2 --from-file creds.json --default`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("auth-configs create")); err != nil {
				return err
			}
			if service == "" || name == "" || typ == "" || scheme == "" {
				return fmt.Errorf("--service, --name, --type and --scheme are required")
			}
			if typ != "managed" && typ != "custom" {
				return fmt.Errorf("--type must be managed or custom")
			}
			body := map[string]any{"service": service, "name": name, "type": typ, "scheme": scheme}
			if len(scopes) > 0 {
				body["scopes"] = scopes
			}
			for k, v := range map[string]string{"credentialSource": source, "redirectHost": redirect, "projectId": project} {
				if v != "" {
					body[k] = v
				}
			}
			if share {
				body["shareWithProjects"] = true
			}
			if def {
				body["isDefault"] = true
			}
			if typ == "custom" && (fromFile != "" || secretsStdin) {
				secrets, err := readJSONInput(cmd, fromFile, "the credentials")
				if err != nil {
					return err
				}
				body["secrets"] = secrets
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				AuthConfig authConfigDoc `json:"authConfig"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, builderAPI+"/auth-configs", body, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "Created %s (%s, %s)\n", textsafe.Clean(doc.AuthConfig.ID), textsafe.Strict(doc.AuthConfig.Name), textsafe.Clean(doc.AuthConfig.Service))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&service, "service", "", "Connector slug or built-in service")
	cmd.Flags().StringVar(&name, "name", "", "Name, unique per service")
	cmd.Flags().StringVar(&typ, "type", "", "managed or custom")
	cmd.Flags().StringVar(&scheme, "scheme", "", "oauth2, oauth2_client_credentials, api_key, bearer, basic or none")
	cmd.Flags().StringSliceVar(&scopes, "scopes", nil, "OAuth scopes, comma-separated (default: the connector's)")
	cmd.Flags().StringVar(&source, "credential-source", "", "config (one shared secret) or connect_time (each owner supplies theirs)")
	cmd.Flags().StringVar(&redirect, "redirect-host", "", "platform or whitelabel")
	cmd.Flags().StringVar(&project, "project", "", "Create it in a project (prj_...) instead of the organisation")
	cmd.Flags().BoolVar(&share, "share-with-projects", false, "Let the organisation's projects use it")
	cmd.Flags().BoolVar(&def, "default", false, "Make it the default for the service")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "JSON file with the credentials (or - for standard input)")
	cmd.Flags().BoolVar(&secretsStdin, "secrets-stdin", false, "Read the credentials as JSON from standard input")
	return cmd
}

func acUpdateCmd() *cobra.Command {
	var f approvalFlags
	var name, redirect string
	var scopes []string
	var share, def bool
	cmd := &cobra.Command{
		Use:   "update <ac_id>",
		Short: "Change an auth config's name, scopes or defaults",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ac_", "an auth config id")
			if err != nil {
				return err
			}
			body := map[string]any{}
			if name != "" {
				body["name"] = name
			}
			if redirect != "" {
				body["redirectHost"] = redirect
			}
			if cmd.Flags().Changed("scopes") {
				body["scopes"] = scopes
			}
			if cmd.Flags().Changed("share-with-projects") {
				body["shareWithProjects"] = share
			}
			if cmd.Flags().Changed("default") {
				body["isDefault"] = def
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: give --name, --scopes, --redirect-host, --share-with-projects or --default")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPatch, acPath(id), body, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %s\n", id)
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&name, "name", "", "New name")
	cmd.Flags().StringSliceVar(&scopes, "scopes", nil, "Replace the scopes (comma-separated; empty list for the connector's)")
	cmd.Flags().StringVar(&redirect, "redirect-host", "", "platform or whitelabel")
	cmd.Flags().BoolVar(&share, "share-with-projects", false, "Whether projects may use it")
	cmd.Flags().BoolVar(&def, "default", false, "Whether it is the default for the service")
	return cmd
}

func acToggleCmd(verb string) *cobra.Command {
	var f approvalFlags
	var suspend bool
	cmd := &cobra.Command{
		Use:   verb + " <ac_id>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " an auth config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ac_", "an auth config id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if verb == "disable" && suspend {
				body["suspendAccounts"] = true
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, acPath(id, ":"+verb), body, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "%sd %s\n", strings.ToUpper(verb[:1])+verb[1:], id)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	if verb == "disable" {
		cmd.Long = "New connections through a disabled config are refused; accounts that exist keep working unless --suspend-accounts."
		cmd.Flags().BoolVar(&suspend, "suspend-accounts", false, "Also suspend the accounts connected through it")
	}
	return cmd
}

func acDeleteCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <ac_id>",
		Short: "Delete an auth config that has no accounts",
		Long:  "A config with accounts is refused (409). Removing one anyway revokes its accounts and is an administrator's act in the browser.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ac_", "an auth config id")
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete auth config %s and its stored secrets?", id)); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodDelete, acPath(id), nil, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", id)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without asking")
	return cmd
}

func acSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Replace or rotate an auth config's credentials"}
	for _, verb := range []string{"set", "rotate"} {
		verb := verb
		var f approvalFlags
		var fromFile, grace string
		c := &cobra.Command{
			Use:   verb + " <ac_id>",
			Short: map[string]string{"set": "Replace the named credential fields", "rotate": "Set new credential values and keep the old ones for a grace window"}[verb],
			Long: `Reads a JSON object of field names and values from standard input or --from-file;
fields not named are kept. The values are never printed or logged. Refused in an
agent session (exit 10).`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := refuseAgentChange(brand.Cmd("auth-configs secrets " + verb)); err != nil {
					return err
				}
				id, err := builderID(args[0], "ac_", "an auth config id")
				if err != nil {
					return err
				}
				secrets, err := readJSONInput(cmd, fromFile, "the credentials")
				if err != nil {
					return err
				}
				body := map[string]any{"fields": secrets}
				method, path := http.MethodPut, acPath(id, "/secrets")
				if verb == "rotate" {
					method, path = http.MethodPost, acPath(id, "/secrets:rotate")
					if grace != "" {
						d, err := time.ParseDuration(grace)
						if err != nil || d < time.Minute || d > 7*24*time.Hour {
							return fmt.Errorf("--grace must be a duration between 1m and 168h")
						}
						body["graceSeconds"] = int(d.Seconds())
					}
				}
				cl, err := approvalsClient(f.gatewayURL)
				if err != nil {
					return err
				}
				if _, err := builderRequest(cmd, cl, f.jsonOut, method, path, body, nil); err != nil {
					return err
				}
				if !f.jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(), "Updated %s on %s (values not shown)\n", maskedFields(secrets), id)
				}
				return nil
			},
		}
		f.bind(c, "Print the gateway's answer as JSON")
		c.Flags().StringVar(&fromFile, "from-file", "", "JSON file with the values (or - for standard input)")
		if verb == "rotate" {
			c.Flags().StringVar(&grace, "grace", "", "How long the old values keep working, such as 24h (default 24h, at most 168h)")
		}
		cmd.AddCommand(c)
	}
	return cmd
}

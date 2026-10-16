package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const catalogAPI = "/api/catalog"

// CatalogCmd returns `zuano catalog`: one view of the services an agent can
// use, whether built in, written by your organisation, federated from another
// vendor's catalog or mirrored from the MCP registry.
func CatalogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Browse services from every source, add a vendor's catalog, install from the MCP registry",
		Long: `The catalog shows each service once, with the sources that offer it: native
(built in), custom (your organisation's own), federated (another vendor's
catalog, such as Composio or Zapier, reached through the gateway) and registry
(servers from the MCP registry). Connecting a federated app or installing a
registry server brings in tools nobody here reviewed, so each is pinned and
classified by risk first, and a person approves that in the browser.

Credentials are given as JSON on standard input, never in a flag. An agent
session cannot add a federated catalog or install from the registry (exit 10).`,
	}
	cmd.AddCommand(catalogSearchCmd(), catalogShowCmd(), catalogFederatedCmd(), catalogRegistryCmd())
	return cmd
}

type catalogSource struct {
	Source string `json:"source"`
	Tools  int    `json:"tools"`
	Name   string `json:"name"`
}

type catalogItem struct {
	Key       string          `json:"key"`
	Name      string          `json:"name"`
	Summary   string          `json:"summary"`
	Sources   []catalogSource `json:"sources"`
	Preferred string          `json:"preferred"`
}

func catalogSearchCmd() *cobra.Command {
	var f approvalFlags
	var source, cursor string
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Find a service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source != "" && !map[string]bool{"native": true, "custom": true, "federated": true, "registry": true}[source] {
				return errors.New("--source must be native, custom, federated or registry")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items      []catalogItem `json:"items"`
				NextCursor string        `json:"nextCursor"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(catalogAPI+"/browse", map[string]string{"q": args[0], "source": source, "cursor": cursor}), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintln(out, "No service matches.")
				return nil
			}
			var rows [][]string
			for _, it := range doc.Items {
				var srcs []string
				for _, s := range it.Sources {
					srcs = append(srcs, s.Source)
				}
				rows = append(rows, []string{it.Key, it.Name, strings.Join(srcs, ", "), orDash(it.Preferred)})
			}
			printRows(out, []string{"SERVICE", "NAME", "SOURCES", "PREFERRED"}, rows)
			if doc.NextCursor != "" {
				fmt.Fprintf(out, "\nMore: --cursor %s\n", textsafe.Clean(doc.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the results as JSON")
	cmd.Flags().StringVar(&source, "source", "", "Only this source: native, custom, federated or registry")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue from a previous page")
	return cmd
}

func catalogShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <service>",
		Short: "Show a service with every source that offers it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Service catalogItem `json:"service"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, catalogAPI+"/services/"+url.PathEscape(key), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			s := doc.Service
			fmt.Fprintf(out, "%s (%s)\n%s\n", textsafe.Strict(s.Name), textsafe.Clean(s.Key), textsafe.Strict(s.Summary))
			for _, src := range s.Sources {
				mark := " "
				if src.Source == s.Preferred {
					mark = "*"
				}
				fmt.Fprintf(out, " %s %-10s %3d tool(s)  %s\n", mark, textsafe.Clean(src.Source), src.Tools, textsafe.Strict(src.Name))
			}
			if s.Preferred != "" {
				fmt.Fprintf(out, "(* preferred source)\n")
			}
			return nil
		},
	}
	f.bind(cmd, "Print the service as JSON")
	return cmd
}

func catalogFederatedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "federated",
		Short: "Another vendor's catalog, reached through the gateway",
		Long: `Adding a vendor's catalog verifies the credential you give; enabling its apps
provisions them and discovers their tools. Every tool is pinned and classified
by risk, and a person reviews and accepts them in the browser with a passkey
('review' opens that page).`,
	}
	cmd.AddCommand(fedListCmd(), fedShowCmd(), fedAddCmd(), fedEnableCmd(), fedReviewCmd())
	return cmd
}

var vendorName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func fedPath(id string, rest ...string) string {
	return catalogAPI + "/federated/" + url.PathEscape(id) + strings.Join(rest, "")
}

func fedListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the federated catalogs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items []struct {
					ID     string `json:"id"`
					Vendor string `json:"vendor"`
					Mode   string `json:"credentialMode"`
					Apps   int    `json:"apps"`
					Health string `json:"health"`
					Review int    `json:"reviewQueue"`
				} `json:"items"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, catalogAPI+"/federated", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintf(out, "No federated catalogs. Add one with `%s`.\n", brand.Cmd("catalog federated add <vendor> --credential-stdin"))
				return nil
			}
			var rows [][]string
			for _, i := range doc.Items {
				rows = append(rows, []string{i.ID, i.Vendor, orDash(i.Mode), fmt.Sprint(i.Apps), orDash(i.Health), fmt.Sprint(i.Review)})
			}
			printRows(out, []string{"ID", "VENDOR", "CREDENTIALS", "APPS", "HEALTH", "TO REVIEW"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	return cmd
}

func fedShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a federated catalog and its apps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Federated struct {
					ID     string `json:"id"`
					Vendor string `json:"vendor"`
					Mode   string `json:"credentialMode"`
					Health string `json:"health"`
					Apps   []struct {
						App    string `json:"vendorApp"`
						Status string `json:"status"`
						Tools  int    `json:"tools"`
					} `json:"apps"`
				} `json:"federated"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, fedPath(id), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			d := doc.Federated
			fmt.Fprintf(out, "%s  %s, credentials %s, health %s\n", textsafe.Clean(d.ID), textsafe.Clean(d.Vendor), orDash(d.Mode), orDash(d.Health))
			var rows [][]string
			for _, a := range d.Apps {
				rows = append(rows, []string{a.App, a.Status, fmt.Sprint(a.Tools)})
			}
			if len(rows) > 0 {
				printRows(out, []string{"APP", "STATUS", "TOOLS"}, rows)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the catalog as JSON")
	return cmd
}

func fedAddCmd() *cobra.Command {
	var f approvalFlags
	var perUser, fromStdin bool
	var fromFile string
	cmd := &cobra.Command{
		Use:   "add <vendor> --credential-stdin",
		Short: "Add a vendor's catalog",
		Long: `Reads the vendor credential as a JSON object from standard input
(--credential-stdin) or a file (--from-file), never from a flag, and has the
gateway verify it. With --per-user each member connects with their own account
at the vendor instead of one shared credential. Refused in an agent session
(exit 10, agent_not_allowed).`,
		Example: `  echo '{"api_key":"..."}' | zuano catalog federated add composio --credential-stdin --per-user`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("catalog federated add")); err != nil {
				return err
			}
			if !vendorName.MatchString(args[0]) {
				return fmt.Errorf("%q is not a vendor name", textsafe.Clean(args[0]))
			}
			if !fromStdin && fromFile == "" {
				return errors.New("give the credential with --credential-stdin or --from-file")
			}
			creds, err := readJSONInput(cmd, fromFile, "the credential")
			if err != nil {
				return err
			}
			mode := "shared"
			if perUser {
				mode = "per_user"
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Federated struct {
					ID string `json:"id"`
				} `json:"federated"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, catalogAPI+"/federated", map[string]any{"vendor": args[0], "credentials": creds, "credentialMode": mode}, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added %s as %s (credential verified, not shown).\nPick its apps with `%s`.\n", textsafe.Clean(args[0]), textsafe.Clean(doc.Federated.ID), brand.Cmd("catalog federated enable "+doc.Federated.ID+" <app>..."))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVar(&fromStdin, "credential-stdin", false, "Read the credential as JSON from standard input")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "JSON file with the credential")
	cmd.Flags().BoolVar(&perUser, "per-user", false, "Each member connects with their own account at the vendor")
	return cmd
}

func fedEnableCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "enable <id> <app>...",
		Short: "Enable apps of a federated catalog",
		Long:  "Provisions the apps and discovers their tools. The tools stay hidden until they are reviewed and accepted in the browser (`zuano catalog federated review`).",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			var apps []map[string]string
			for _, a := range args[1:] {
				if a == "" || len(a) > 100 {
					return errors.New("an app name is empty or too long")
				}
				apps = append(apps, map[string]string{"vendorApp": a})
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, fedPath(id, "/apps"), map[string]any{"apps": apps}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Enabling %d app(s). Review their tools with `%s`.\n", len(apps), brand.Cmd("catalog federated review "+id))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	return cmd
}

func fedReviewCmd() *cobra.Command {
	var f approvalFlags
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "review <id>",
		Short: "Open the page where the tools and their risk are accepted",
		Long:  "Accepting a vendor's tools and what risk each carries is decided in the browser with a passkey; this only opens the page. Refused in an agent session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAutomationInAgent(brand.Cmd("catalog federated review")); err != nil {
				return err
			}
			id, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			return openAutomationPage(cmd, f.gatewayURL, "/catalog/federated/"+url.PathEscape(id), noBrowser, "Review and accept the tools there")
		},
	}
	f.bind(cmd, "Unused: the command only opens a page")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}

func catalogRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Servers from the MCP registry",
	}
	cmd.AddCommand(regSearchCmd(), regShowCmd(), regInstallCmd())
	return cmd
}

type registryServer struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	Status      string `json:"status"`
}

func regSearchCmd() *cobra.Command {
	var f approvalFlags
	var source, cursor string
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items      []registryServer `json:"items"`
				NextCursor string           `json:"nextCursor"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(catalogAPI+"/registry", map[string]string{"q": args[0], "source": source, "cursor": cursor}), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintln(out, "No registry server matches.")
				return nil
			}
			var rows [][]string
			for _, s := range doc.Items {
				rows = append(rows, []string{s.Name, orDash(s.Version), orDash(s.Status), s.Description})
			}
			printRows(out, []string{"NAME", "VERSION", "STATUS", "DESCRIPTION"}, rows)
			if doc.NextCursor != "" {
				fmt.Fprintf(out, "\nMore: --cursor %s\n", textsafe.Clean(doc.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the results as JSON")
	cmd.Flags().StringVar(&source, "source", "", "A registry source: official or a private one")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue from a previous page")
	return cmd
}

type registryVersion struct {
	Version  string `json:"version"`
	Packages []struct {
		RegistryType string   `json:"registryType"`
		Identifier   string   `json:"identifier"`
		RuntimeHint  string   `json:"runtimeHint"`
		Args         []string `json:"args"`
	} `json:"packages"`
	Remotes []struct {
		URL string `json:"url"`
	} `json:"remotes"`
}

func registryName(arg string) (name, version string, err error) {
	name, version, _ = strings.Cut(arg, "@")
	if name == "" || strings.ContainsAny(name, "?#% \t\n") || strings.Contains(name, "..") {
		return "", "", fmt.Errorf("%q is not a registry server name", textsafe.Clean(arg))
	}
	return name, version, nil
}

func regShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show the versions of a registry server and how it runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := registryName(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Versions []registryVersion `json:"versions"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, catalogAPI+"/registry/"+pathEscapeKeepSlash(name)+"/versions", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			for _, v := range doc.Versions {
				fmt.Fprintf(out, "%s %s\n", textsafe.Clean(name), textsafe.Clean(v.Version))
				for _, p := range v.Packages {
					fmt.Fprintf(out, "  package  %s %s\n", textsafe.Clean(p.RegistryType), textsafe.Strict(p.Identifier))
				}
				for _, r := range v.Remotes {
					fmt.Fprintf(out, "  remote   %s\n", textsafe.Strict(r.URL))
				}
			}
			return nil
		},
	}
	f.bind(cmd, "Print the versions as JSON")
	return cmd
}

func pathEscapeKeepSlash(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

var packageToken = regexp.MustCompile(`^[A-Za-z0-9@._/+:=~^-]+$`)

// wrapCommand turns a registry package into the command that starts it, or
// says why it cannot be done safely. Names and versions come from a registry
// anyone can publish to, so they are checked before they are put on a command line.
func wrapCommand(registryType, identifier, version string) ([]string, error) {
	if !packageToken.MatchString(identifier) || (version != "" && !packageToken.MatchString(version)) {
		return nil, fmt.Errorf("the package name %q has characters a command line should not carry", textsafe.Clean(identifier))
	}
	switch registryType {
	case "npm":
		ref := identifier
		if version != "" {
			ref += "@" + version
		}
		return []string{"npx", "-y", ref}, nil
	case "pypi":
		ref := identifier
		if version != "" {
			ref += "==" + version
		}
		return []string{"uvx", ref}, nil
	case "oci":
		ref := identifier
		if version != "" {
			ref += ":" + version
		}
		return []string{"docker", "run", "-i", "--rm", ref}, nil
	}
	return nil, fmt.Errorf("zuano does not know how to start a %q package", textsafe.Clean(registryType))
}

func regInstallCmd() *cobra.Command {
	var f approvalFlags
	var remote, pkg, wrap bool
	cmd := &cobra.Command{
		Use:   "install <name>[@version]",
		Short: "Install a registry server on the gateway, or print how to run it behind the firewall here",
		Long: `Installs the server as an upstream of the gateway, hidden until its tools are
pinned and approved. --remote and --package choose how it runs on the gateway.

--wrap installs nothing on the gateway. It prints the command that runs the
server on this machine behind the firewall:

  zuano mcp wrap --upstream NAME -- npx -y PACKAGE@VERSION

and the 'zuano mcp install ... --firewall' that registers it in your agents.

Refused in an agent session (exit 10, agent_not_allowed).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("catalog registry install")); err != nil {
				return err
			}
			name, version, err := registryName(args[0])
			if err != nil {
				return err
			}
			if remote && pkg {
				return errors.New("--remote and --package exclude each other")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if wrap {
				return printWrap(cmd, c, f, name, version)
			}
			body := map[string]any{"source": "registry", "name": name}
			if version != "" {
				body["version"] = version
			}
			switch {
			case remote:
				body["remote"] = true
			case pkg:
				body["package"] = true
			}
			var doc struct {
				UpstreamID string `json:"upstreamId"`
				Status     string `json:"status"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, catalogAPI+"/registry/install", body, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed %s as %s (%s). Its tools stay hidden until they are approved in the browser.\n", textsafe.Clean(name), textsafe.Clean(doc.UpstreamID), orDash(doc.Status))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVar(&remote, "remote", false, "Run it as a remote server on the gateway")
	cmd.Flags().BoolVar(&pkg, "package", false, "Run its package on the gateway")
	cmd.Flags().BoolVar(&wrap, "wrap", false, "Print how to run it behind the firewall on this machine; install nothing on the gateway")
	return cmd
}

func printWrap(cmd *cobra.Command, c *client.Client, f approvalFlags, name, version string) error {
	var doc struct {
		Versions []registryVersion `json:"versions"`
	}
	raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, catalogAPI+"/registry/"+pathEscapeKeepSlash(name)+"/versions", nil, &doc)
	if err != nil {
		return err
	}
	var chosen *registryVersion
	for i := range doc.Versions {
		if version == "" || doc.Versions[i].Version == version {
			chosen = &doc.Versions[i]
			break
		}
	}
	if chosen == nil {
		return fmt.Errorf("the registry has no version %q of %s", textsafe.Clean(version), textsafe.Clean(name))
	}
	out := cmd.OutOrStdout()
	if f.jsonOut {
		printRaw(out, raw)
		return nil
	}
	upstream := strings.Trim(nonSlugChars.ReplaceAllString(strings.ToLower(name[strings.LastIndex(name, "/")+1:]), "-"), "-")
	if upstream == "" {
		upstream = "registry-server"
	}
	for _, p := range chosen.Packages {
		argv, err := wrapCommand(p.RegistryType, p.Identifier, chosen.Version)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "skipped a package: %v\n", err)
			continue
		}
		fmt.Fprintf(out, "Run it here behind the firewall:\n  %s mcp wrap --upstream %s -- %s\n", brand.Name, upstream, strings.Join(argv, " "))
		install := fmt.Sprintf("%s mcp install %s --command %s", brand.Name, upstream, argv[0])
		for _, a := range argv[1:] {
			install += " --arg " + a
		}
		fmt.Fprintf(out, "Register it in your agents with the firewall on:\n  %s --firewall\n", install)
		return nil
	}
	for _, r := range chosen.Remotes {
		if u, err := url.Parse(r.URL); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && packageToken.MatchString(r.URL) {
			fmt.Fprintf(out, "It is a remote server. Register it in your agents with the firewall on:\n  %s mcp install %s --url %s --firewall\n", brand.Name, upstream, r.URL)
			return nil
		}
	}
	return fmt.Errorf("%s %s has no package this machine can start and no remote address that is safe to print", textsafe.Clean(name), textsafe.Clean(chosen.Version))
}

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

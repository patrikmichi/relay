package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/catalogimport"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const connectorsAPI = "/api/connectors"

var riskClasses = map[string]bool{"read": true, "write": true, "destructive": true, "outward": true}

type importOperation struct {
	ID         string `json:"id"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Summary    string `json:"summary"`
	Risk       string `json:"risk"`
	RiskReason string `json:"riskReason"`
}

type connectorPreview struct {
	Tools []struct {
		Name        string `json:"name"`
		Risk        string `json:"risk"`
		Method      string `json:"method"`
		Path        string `json:"path"`
		Description string `json:"description"`
	} `json:"tools"`
	Quality struct {
		Passed bool     `json:"passed"`
		Issues []string `json:"issues"`
	} `json:"quality"`
	Version int `json:"version"`
}

func connectorsImportCmd() *cobra.Command {
	var f approvalFlags
	var format, slug, authConfig string
	var list, suggest, publish bool
	var ops []string
	var risks []string
	cmd := &cobra.Command{
		Use:   "import <file|url>",
		Short: "Turn an API description into a connector draft",
		Long: `Reads an API description (OpenAPI 3, Swagger 2, a Postman collection, a GraphQL
schema or introspection, or a Google discovery document) and has the gateway
make a draft connector from the operations you choose. The gateway classifies
each operation by risk (read, write, destructive or outward) and a person
reviews them; --risk overrides one with a reason.

  --list      show the operations and stop
  --ops a,b   take these operations
  --suggest   take the ones the gateway suggests

A URL is fetched by the gateway, not by this machine. The draft does nothing
until it is published, and --publish is refused in an agent session (exit 10);
in a strict organisation it asks for a passkey.

Exit status: 60 the description is not valid, 61 its format is not supported,
62 the slug or version is taken, 63 the draft fails the quality checks.`,
		Example: `  zuano connectors import petstore.yaml --list
  zuano connectors import https://api.example.com/openapi.json --suggest --slug example --publish`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !list && !suggest && len(ops) == 0 {
				return errors.New("choose what to import: --list to see the operations, --ops a,b or --suggest")
			}
			if publish {
				if config.AgentMode() {
					return &ExitError{Code: exitDenied, Msg: "agent_not_allowed: publishing a connector is for a person; it is refused while ZUANO_AGENT is set"}
				}
				if list {
					return errors.New("--publish cannot be combined with --list")
				}
			}
			if authConfig != "" {
				if _, err := builderID(authConfig, "ac_", "an auth config id"); err != nil {
					return err
				}
			}
			riskOverrides, err := parseRiskOverrides(risks)
			if err != nil {
				return err
			}
			if format != "auto" && !catalogimport.Valid(format) {
				return fmt.Errorf("--format must be auto or one of %s", strings.Join(catalogimport.Formats, ", "))
			}
			body := map[string]any{}
			if u, err := url.Parse(args[0]); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
				if u.User != nil || u.Host == "" {
					return errors.New("the URL must not carry credentials")
				}
				body["url"] = args[0]
				if format != "auto" {
					body["format"] = format
				}
			} else {
				text, detected, err := readAPIDescription(args[0], format)
				if err != nil {
					return err
				}
				body["text"], body["format"] = text, detected
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var imp struct {
				DraftID    string            `json:"draftId"`
				Revision   int               `json:"revision"`
				Format     string            `json:"format"`
				Operations []importOperation `json:"operations"`
				Warnings   []string          `json:"warnings"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, connectorsAPI+"/import", body, &imp)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if list {
				if f.jsonOut {
					printRaw(out, raw)
					return nil
				}
				fmt.Fprintf(out, "Draft %s from %s: %d operation(s)\n", textsafe.Clean(imp.DraftID), textsafe.Clean(imp.Format), len(imp.Operations))
				var rows [][]string
				for _, o := range imp.Operations {
					rows = append(rows, []string{o.ID, strings.ToUpper(o.Method) + " " + o.Path, orDash(o.Risk), o.Summary})
				}
				printRows(out, []string{"ID", "OPERATION", "RISK", "SUMMARY"}, rows)
				printWarnings(out, imp.Warnings)
				fmt.Fprintf(out, "\nChoose with `%s`.\n", brand.Cmd("connectors import <file> --ops a,b"))
				return nil
			}
			patch := map[string]any{"expectedRevision": imp.Revision}
			if suggest {
				patch["selection"] = map[string]any{"suggest": true}
			} else {
				patch["selection"] = map[string]any{"operations": ops}
			}
			if slug != "" {
				patch["slug"] = slug
			}
			if authConfig != "" {
				patch["authConfigId"] = authConfig
			}
			if len(riskOverrides) > 0 {
				patch["riskOverrides"] = riskOverrides
			}
			draftPath := connectorsAPI + "/drafts/" + url.PathEscape(imp.DraftID)
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodPatch, draftPath, patch, nil); err != nil {
				return err
			}
			var prev connectorPreview
			raw, err = builderRequest(cmd, c, f.jsonOut, http.MethodPost, draftPath+"/preview", map[string]any{}, &prev)
			if err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(out, "Draft %s: %d tool(s)\n", textsafe.Clean(imp.DraftID), len(prev.Tools))
				var rows [][]string
				for _, t := range prev.Tools {
					rows = append(rows, []string{t.Name, t.Risk, strings.ToUpper(t.Method) + " " + t.Path})
				}
				printRows(out, []string{"TOOL", "RISK", "OPERATION"}, rows)
				printWarnings(out, imp.Warnings)
				for _, issue := range prev.Quality.Issues {
					fmt.Fprintf(out, "quality: %s\n", textsafe.Strict(issue))
				}
			}
			if !prev.Quality.Passed && len(prev.Quality.Issues) > 0 && publish {
				return &ExitError{Code: exitQualityGate, Msg: "the draft fails the quality checks, so it was not published"}
			}
			if publish {
				var res struct {
					Slug    string `json:"slug"`
					Version int    `json:"version"`
				}
				pubRaw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, draftPath+"/publish", map[string]any{"expectedVersion": prev.Version}, &res)
				if err != nil {
					return err
				}
				if f.jsonOut {
					printRaw(out, pubRaw)
				} else {
					fmt.Fprintf(out, "Published %s version %d\n", textsafe.Clean(res.Slug), res.Version)
				}
				return nil
			}
			if f.jsonOut {
				printRaw(out, raw)
			} else {
				fmt.Fprintf(out, "\nNothing is published yet: `%s` when the tools and their risk look right.\n", brand.Cmd("connectors publish <slug>"))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&format, "format", "auto", "auto, "+strings.Join(catalogimport.Formats, ", "))
	cmd.Flags().BoolVar(&list, "list", false, "Show the operations and stop")
	cmd.Flags().StringSliceVar(&ops, "ops", nil, "Operations to take, by id (comma-separated)")
	cmd.Flags().BoolVar(&suggest, "suggest", false, "Take the operations the gateway suggests")
	cmd.Flags().StringVar(&slug, "slug", "", "Slug for the connector")
	cmd.Flags().StringArrayVar(&risks, "risk", nil, "tool=class:reason overrides an operation's risk class; repeatable")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "Auth config (ac_...) the connector uses")
	cmd.Flags().BoolVar(&publish, "publish", false, "Publish the draft")
	return cmd
}

func printWarnings(w io.Writer, warnings []string) {
	for _, s := range warnings {
		fmt.Fprintf(w, "warning: %s\n", textsafe.Strict(s))
	}
}

func parseRiskOverrides(list []string) (map[string]any, error) {
	out := map[string]any{}
	for _, r := range list {
		tool, rest, ok := strings.Cut(r, "=")
		class, reason, ok2 := strings.Cut(rest, ":")
		if !ok || !ok2 || tool == "" || strings.TrimSpace(reason) == "" || !riskClasses[class] {
			return nil, fmt.Errorf("--risk %q must be tool=class:reason with class read, write, destructive or outward and a reason", textsafe.Clean(r))
		}
		out[tool] = map[string]any{"class": class, "reason": reason}
	}
	return out, nil
}

// readAPIDescription reads a description from a file, checks it is one the
// gateway can import and returns it with its format.
func readAPIDescription(path, format string) (text, detected string, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, catalogimport.MaxSize+1))
	if err != nil {
		return "", "", err
	}
	if len(raw) > catalogimport.MaxSize {
		return "", "", fmt.Errorf("%s is larger than %d MiB", textsafe.Clean(path), catalogimport.MaxSize>>20)
	}
	detected, err = catalogimport.Detect(raw)
	switch {
	case errors.Is(err, catalogimport.ErrUnsupported):
		return "", "", &ExitError{Code: exitSpecUnsupported, Msg: textsafe.Clean(path) + ": " + err.Error()}
	case err != nil && format == "auto":
		return "", "", &ExitError{Code: exitSpecInvalid, Msg: textsafe.Clean(path) + ": " + err.Error()}
	case err != nil:
		detected = format
	}
	if format != "auto" && detected != format {
		return "", "", &ExitError{Code: exitSpecInvalid, Msg: fmt.Sprintf("%s looks like %s, not %s", textsafe.Clean(path), detected, format)}
	}
	return string(raw), detected, nil
}

func connectorsListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your organisation's custom connectors",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items []struct {
					Slug      string `json:"slug"`
					Name      string `json:"name"`
					Version   int    `json:"version"`
					Status    string `json:"status"`
					Tools     int    `json:"tools"`
					UpdatedAt string `json:"updatedAt"`
				} `json:"items"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, connectorsAPI+"/custom", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintf(out, "No custom connectors. Make one with `%s`.\n", brand.Cmd("connectors import <file|url> --list"))
				return nil
			}
			var rows [][]string
			for _, i := range doc.Items {
				rows = append(rows, []string{i.Slug, i.Name, fmt.Sprintf("v%d", i.Version), i.Status, fmt.Sprint(i.Tools), orDash(i.UpdatedAt)})
			}
			printRows(out, []string{"SLUG", "NAME", "VERSION", "STATUS", "TOOLS", "UPDATED"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	return cmd
}

func connectorSlug(arg string) (string, error) {
	if arg == "" || strings.ContainsAny(arg, "/?#%: \t\n") {
		return "", fmt.Errorf("%q is not a connector slug", textsafe.Clean(arg))
	}
	return arg, nil
}

func connectorsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <slug>",
		Short: "Show a custom connector and its tools",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Connector struct {
					Slug    string `json:"slug"`
					Name    string `json:"name"`
					Version int    `json:"version"`
					Status  string `json:"status"`
					Auth    string `json:"auth"`
					Tools   []struct {
						Name string `json:"name"`
						Risk string `json:"risk"`
					} `json:"tools"`
				} `json:"connector"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, connectorsAPI+"/custom/"+url.PathEscape(slug), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			cn := doc.Connector
			fmt.Fprintf(out, "%s (%s) v%d, %s, auth %s\n", textsafe.Clean(cn.Slug), textsafe.Strict(cn.Name), cn.Version, textsafe.Clean(cn.Status), orDash(cn.Auth))
			var rows [][]string
			for _, t := range cn.Tools {
				rows = append(rows, []string{t.Name, t.Risk})
			}
			if len(rows) > 0 {
				printRows(out, []string{"TOOL", "RISK"}, rows)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the connector as JSON")
	return cmd
}

func connectorsVersionsCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "versions <slug>",
		Short: "List the versions of a custom connector",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items []struct {
					Version int    `json:"version"`
					Status  string `json:"status"`
					At      string `json:"at"`
					By      string `json:"by"`
				} `json:"items"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, connectorsAPI+"/custom/"+url.PathEscape(slug)+"/versions", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows [][]string
			for _, v := range doc.Items {
				rows = append(rows, []string{fmt.Sprintf("v%d", v.Version), v.Status, v.At, orDash(v.By)})
			}
			printRows(out, []string{"VERSION", "STATUS", "WHEN", "BY"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the versions as JSON")
	return cmd
}

func connectorsPublishCmd() *cobra.Command {
	var f approvalFlags
	var version int
	cmd := &cobra.Command{
		Use:   "publish <slug>",
		Short: "Publish the draft of a custom connector",
		Long: `Publishes the connector's open draft. --version is the version you expect to
publish; if the draft moved on, the gateway refuses (exit 62) and nothing is
published. Refused in an agent session (exit 10); a strict organisation asks for
a passkey.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if config.AgentMode() {
				return &ExitError{Code: exitDenied, Msg: "agent_not_allowed: publishing a connector is for a person; it is refused while ZUANO_AGENT is set"}
			}
			slug, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var drafts struct {
				Items []struct {
					ID          string `json:"id"`
					NextVersion int    `json:"nextVersion"`
				} `json:"items"`
			}
			if _, err := builderRequest(cmd, c, false, http.MethodGet, withQuery(connectorsAPI+"/drafts", map[string]string{"slug": slug}), nil, &drafts); err != nil {
				return err
			}
			if len(drafts.Items) != 1 {
				return fmt.Errorf("%d open drafts for %s; there must be exactly one (make one with `%s`)", len(drafts.Items), textsafe.Clean(slug), brand.Cmd("connectors import"))
			}
			expect := version
			if expect == 0 {
				expect = drafts.Items[0].NextVersion
			}
			var res struct {
				Slug    string `json:"slug"`
				Version int    `json:"version"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, connectorsAPI+"/drafts/"+url.PathEscape(drafts.Items[0].ID)+"/publish", map[string]any{"expectedVersion": expect}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Published %s version %d\n", textsafe.Clean(res.Slug), res.Version)
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().IntVar(&version, "version", 0, "The version you expect to publish (default: the next one)")
	return cmd
}

func connectorsDisableCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "disable <slug>",
		Short: "Disable a custom connector: its tools stop being offered",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Disable connector %s? Its tools stop being offered.", textsafe.Clean(slug))); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, connectorsAPI+"/custom/"+url.PathEscape(slug)+"/disable", map[string]any{}, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Disabled %s\n", textsafe.Clean(slug))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Disable without asking")
	return cmd
}

func connectorsExportCmd() *cobra.Command {
	var f approvalFlags
	var outFile string
	cmd := &cobra.Command{
		Use:   "export <slug>",
		Short: "Export a custom connector as JSON; secrets are never in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := connectorSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := builderRequest(cmd, c, true, http.MethodGet, connectorsAPI+"/custom/"+url.PathEscape(slug)+"/export", nil, nil)
			if err != nil {
				return err
			}
			if outFile == "" {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			if err := os.WriteFile(outFile, append(raw, '\n'), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", outFile)
			return nil
		},
	}
	f.bind(cmd, "Unused: the connector is always JSON")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().StringVar(&outFile, "out", "", "Write to a file instead of the terminal")
	return cmd
}

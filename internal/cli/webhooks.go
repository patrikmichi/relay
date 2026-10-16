package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const maxTriggerFile = 1 << 20

var webhookIDPattern = regexp.MustCompile(`^(whe|whs|whd|wtr)_[A-Za-z0-9_-]{6,40}$`)

// secretsMayPrint reports whether a secret can be written to w without it
// landing in a pipe or file by accident. A variable so tests can stand in a
// terminal.
var secretsMayPrint = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isInteractiveTerminal(f)
}

// WebhooksCmd returns `zuano webhooks`, the client for inbound endpoints,
// triggers and outbound subscriptions.
func WebhooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhooks",
		Short: "Manage inbound webhook endpoints, triggers and outbound subscriptions",
		Long: `Inbound endpoints receive events from other services and verify their
signatures; triggers start work when an event arrives; subscriptions send the
gateway's own events (approvals, runs, files) to your HTTPS URL, signed with the
Standard Webhooks headers.

A secret is shown once, when it is created or rotated. zuano prints one only to
a terminal or with --json, and refuses before sending the request when
standard output is a pipe or a file, so a secret is never lost or leaked to a
log by accident.`,
	}
	cmd.AddCommand(webhooksListCmd(), webhooksCreateCmd(), webhooksTestCmd(), webhooksDeliveriesCmd(), webhooksRedeliverCmd(), webhooksRotateCmd(), webhooksEventsCmd())
	return cmd
}

func webhookID(arg string, prefixes ...string) (string, error) {
	for _, p := range prefixes {
		if strings.HasPrefix(arg, p+"_") && webhookIDPattern.MatchString(arg) {
			return arg, nil
		}
	}
	return "", fmt.Errorf("%q is not a valid id (expected %s)", textsafe.Clean(arg), strings.Join(prefixes, "_..., ")+"_...")
}

// webhookPath maps an endpoint or subscription id to its API collection.
func webhookPath(id string) string {
	if strings.HasPrefix(id, "whs_") {
		return "/api/webhooks/subscriptions/" + url.PathEscape(id)
	}
	return "/api/webhooks/endpoints/" + url.PathEscape(id)
}

// webhookWrite sends a change. An agent session's write may be held for
// approval; that is reported like a held call and exits 12.
func webhookWrite(cmd *cobra.Command, c *client.Client, jsonOut bool, method, path string, body, out any) ([]byte, error) {
	status, raw, err := approvalsDo(cmd.Context(), c, controlRequest, method, path, body)
	if err != nil {
		return nil, err
	}
	if held := pendingFromREST(status, raw); held != nil {
		if jsonOut {
			printRaw(cmd.OutOrStdout(), raw)
		}
		return nil, handlePending(cmd.Context(), c, held, jsonOut, callOpts{}, cmd.OutOrStdout())
	}
	if status < 200 || status > 299 {
		return nil, gatewayStatusError(status, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return raw, nil
}

func webhooksListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List endpoints, triggers and subscriptions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Endpoints     []webhookRow `json:"endpoints"`
				Triggers      []webhookRow `json:"triggers"`
				Subscriptions []webhookRow `json:"subscriptions"`
			}
			for _, part := range []struct {
				name string
				dst  *[]webhookRow
			}{{"endpoints", &doc.Endpoints}, {"triggers", &doc.Triggers}, {"subscriptions", &doc.Subscriptions}} {
				var page map[string][]webhookRow
				if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/webhooks/"+part.name, nil, &page); err != nil {
					return err
				}
				*part.dst = page[part.name]
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return json.NewEncoder(out).Encode(doc)
			}
			printWebhookRows(out, "Endpoints", doc.Endpoints)
			printWebhookRows(out, "Triggers", doc.Triggers)
			printWebhookRows(out, "Subscriptions", doc.Subscriptions)
			return nil
		},
	}
	f.bind(cmd, "Print endpoints, triggers and subscriptions as one JSON document")
	return cmd
}

// webhookRow holds the fields the list shows; the JSON output keeps the rest.
type webhookRow struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Service     string          `json:"service"`
	SourceKind  string          `json:"source_kind"`
	URL         string          `json:"url"`
	Status      string          `json:"status"`
	Enabled     *bool           `json:"enabled"`
	EventTypes  []string        `json:"event_types"`
	Description string          `json:"description"`
	Action      json.RawMessage `json:"action"`
}

func (r webhookRow) summary() string {
	switch {
	case len(r.EventTypes) > 0:
		return strings.Join(r.EventTypes, ",")
	case r.Service != "":
		return r.Service
	case r.SourceKind != "":
		return r.SourceKind
	}
	var a struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(r.Action, &a)
	return a.Kind
}

func (r webhookRow) state() string {
	if r.Status != "" {
		return r.Status
	}
	if r.Enabled != nil {
		if *r.Enabled {
			return "enabled"
		}
		return "disabled"
	}
	return ""
}

func printWebhookRows(out io.Writer, title string, rows []webhookRow) {
	fmt.Fprintf(out, "%s (%d)\n", title, len(rows))
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", textsafe.Clean(r.ID), orDash(textsafe.Clean(r.state())), orDash(textsafe.Clean(r.summary())), textsafe.Clean(firstNonEmptyString(r.URL, r.Name, r.Description)))
	}
	_ = tw.Flush()
}

func webhooksCreateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "create", Short: "Create an endpoint, a subscription or a trigger"}
	cmd.AddCommand(createEndpointCmd(), createSubscriptionCmd(), createTriggerCmd())
	return cmd
}

// requireSecretOutput refuses, before anything is sent, a command whose
// result includes a secret when the output would not be seen by a person.
func requireSecretOutput(cmd *cobra.Command, jsonOut bool) error {
	if jsonOut || secretsMayPrint(cmd.OutOrStdout()) {
		return nil
	}
	return errors.New("this command prints a secret that is shown only once, and standard output is not a terminal. Run it in a terminal, or add --json to capture the secret deliberately")
}

func createEndpointCmd() *cobra.Command {
	var f approvalFlags
	var service, header, algorithm, encoding, tsHeader, description string
	var generic bool
	cmd := &cobra.Command{
		Use:   "endpoint",
		Short: "Create an inbound endpoint for a connected service or a generic HMAC source",
		Example: `  zuano webhooks create endpoint --service github --description "repo events"
  zuano webhooks create endpoint --generic --header X-Signature --algorithm sha256 --encoding hex`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (service == "") == !generic {
				return errors.New("give exactly one of --service <slug> or --generic")
			}
			body := map[string]any{"description": description}
			if generic {
				if header == "" {
					return errors.New("--generic needs --header, the header that carries the signature")
				}
				sig := map[string]any{"scheme": "hmac", "algorithm": algorithm, "header": header, "encoding": encoding}
				if tsHeader != "" {
					sig["timestampHeader"], sig["timestampFormat"] = tsHeader, "unix"
				}
				body["source_kind"], body["signature"] = "generic", sig
			} else {
				body["source_kind"], body["service"] = "connector", service
			}
			if err := requireSecretOutput(cmd, f.jsonOut); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Endpoint webhookRow `json:"endpoint"`
				Secret   string     `json:"secret"`
			}
			raw, err := webhookWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/webhooks/endpoints", body, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "Endpoint: %s\nURL:      %s\nSecret:   %s\nThe secret is shown only once; set it, and the URL, in the sending service.\n", textsafe.Clean(res.Endpoint.ID), textsafe.Clean(res.Endpoint.URL), textsafe.Clean(res.Secret))
			return nil
		},
	}
	f.bind(cmd, "Print the response, secret included, as JSON")
	cmd.Flags().StringVar(&service, "service", "", "Connected service whose webhook this receives (a connector slug)")
	cmd.Flags().BoolVar(&generic, "generic", false, "A custom source that signs requests with an HMAC")
	cmd.Flags().StringVar(&header, "header", "", "Header carrying the signature (generic)")
	cmd.Flags().StringVar(&algorithm, "algorithm", "sha256", "HMAC algorithm: sha1, sha256 or sha512 (generic)")
	cmd.Flags().StringVar(&encoding, "encoding", "hex", "Signature encoding: hex or base64 (generic)")
	cmd.Flags().StringVar(&tsHeader, "timestamp-header", "", "Header carrying a unix timestamp, to reject replays (generic)")
	cmd.Flags().StringVar(&description, "description", "", "What this endpoint is for")
	return cmd
}

func createSubscriptionCmd() *cobra.Command {
	var f approvalFlags
	var target, description string
	var events []string
	cmd := &cobra.Command{
		Use:     "subscription",
		Short:   "Send the gateway's events to an HTTPS URL",
		Example: `  zuano webhooks create subscription --url https://example.com/hooks --events approval.*,run.finished`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u, err := url.Parse(target)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return errors.New("--url must be an https address")
			}
			if len(events) == 0 {
				return errors.New("--events needs at least one event type, such as approval.* or run.finished")
			}
			if err := requireSecretOutput(cmd, f.jsonOut); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Subscription webhookRow `json:"subscription"`
				Secret       string     `json:"secret"`
			}
			raw, err := webhookWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/webhooks/subscriptions", map[string]any{"url": target, "event_types": events, "description": description}, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "Subscription: %s\nSecret:       %s\nThe secret is shown only once; verify each delivery with it (Standard Webhooks signature).\n", textsafe.Clean(res.Subscription.ID), textsafe.Clean(res.Secret))
			return nil
		},
	}
	f.bind(cmd, "Print the response, secret included, as JSON")
	cmd.Flags().StringVar(&target, "url", "", "HTTPS address that receives the events")
	cmd.Flags().StringSliceVar(&events, "events", nil, "Event types, comma separated; a trailing .* matches a family (approval.*)")
	cmd.Flags().StringVar(&description, "description", "", "What this subscription is for")
	return cmd
}

func createTriggerCmd() *cobra.Command {
	var f approvalFlags
	var file string
	cmd := &cobra.Command{
		Use:   "trigger --from-file <trigger.json|->",
		Short: "Create a trigger from a JSON definition",
		Long: `A trigger says which events to react to (an endpoint or a polled tool, plus a
filter) and what to do (start an agent run, a skill run, code, an n8n or single
tool call, or a forward). Definitions are files so they can be reviewed; the
gateway validates them. Use - to read the definition from standard input.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("--from-file is required")
			}
			var r io.Reader = cmd.InOrStdin()
			if file != "-" {
				fh, err := os.Open(file)
				if err != nil {
					return err
				}
				defer fh.Close()
				r = fh
			}
			raw, err := io.ReadAll(io.LimitReader(r, maxTriggerFile+1))
			if err != nil {
				return err
			}
			if len(raw) > maxTriggerFile {
				return fmt.Errorf("the trigger definition is larger than %d bytes", maxTriggerFile)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				return fmt.Errorf("the trigger definition is not a JSON object: %w", err)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Trigger webhookRow `json:"trigger"`
			}
			resp, err := webhookWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/webhooks/triggers", body, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), resp)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Trigger: %s  %s\n", textsafe.Clean(res.Trigger.ID), orDash(textsafe.Clean(res.Trigger.state())))
			return nil
		},
	}
	f.bind(cmd, "Print the response as JSON")
	cmd.Flags().StringVar(&file, "from-file", "", "JSON file with the trigger definition, or - for standard input")
	return cmd
}

func webhooksTestCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "test <whe_...|whs_...>",
		Short: "Send a test event to a subscription, or a synthetic one to an endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0], "whe", "whs")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := webhookWrite(cmd, c, f.jsonOut, http.MethodPost, webhookPath(id)+"/test", map[string]any{}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Sent a test event to %s\n", id)
			return nil
		},
	}
	f.bind(cmd, "Print the response as JSON")
	return cmd
}

func webhooksDeliveriesCmd() *cobra.Command {
	var f approvalFlags
	var status string
	cmd := &cobra.Command{
		Use:   "deliveries <whs_...>",
		Short: "Show a subscription's delivery attempts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0], "whs")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := webhookPath(id) + "/deliveries"
			if status != "" {
				path += "?status=" + url.QueryEscape(status)
			}
			var page struct {
				Deliveries []struct {
					ID             string `json:"id"`
					EventID        string `json:"event_id"`
					EventType      string `json:"event_type"`
					Attempt        int    `json:"attempt"`
					Status         string `json:"status"`
					ResponseStatus *int   `json:"response_status"`
					ResponseMS     *int   `json:"response_ms"`
					LastError      string `json:"last_error"`
				} `json:"deliveries"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &page)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(page.Deliveries) == 0 {
				fmt.Fprintln(out, "No deliveries.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tEVENT\tTYPE\tATTEMPT\tSTATUS\tHTTP\tERROR")
			for _, d := range page.Deliveries {
				code := "-"
				if d.ResponseStatus != nil {
					code = fmt.Sprint(*d.ResponseStatus)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", textsafe.Clean(d.ID), textsafe.Clean(d.EventID), textsafe.Clean(d.EventType), d.Attempt, textsafe.Clean(d.Status), code, textsafe.Clean(d.LastError))
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&status, "status", "", "Only deliveries with this status (pending, succeeded, failed, dead)")
	return cmd
}

func webhooksRedeliverCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "redeliver <whd_...>",
		Short: "Send a delivery again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0], "whd")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := webhookWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/webhooks/deliveries/"+url.PathEscape(id)+"/redeliver", map[string]any{}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Queued a new attempt for %s\n", id)
			return nil
		},
	}
	f.bind(cmd, "Print the response as JSON")
	return cmd
}

func webhooksRotateCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "rotate-secret <whe_...|whs_...>",
		Short: "Replace an endpoint's or subscription's secret",
		Long: `Creates a new secret and prints it once. The old secret keeps verifying for a
grace period (24 hours for subscriptions), so the receiver can switch over.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0], "whe", "whs")
			if err != nil {
				return err
			}
			if err := requireSecretOutput(cmd, f.jsonOut); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Secret string `json:"secret"`
			}
			raw, err := webhookWrite(cmd, c, f.jsonOut, http.MethodPost, webhookPath(id)+"/rotate-secret", map[string]any{}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "New secret for %s: %s\nIt is shown only once.\n", id, textsafe.Clean(res.Secret))
			return nil
		},
	}
	f.bind(cmd, "Print the response, secret included, as JSON")
	return cmd
}

func webhooksEventsCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "events <whe_...>",
		Short: "Show the events an endpoint received",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0], "whe")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var page struct {
				Events []struct {
					ID         string `json:"id"`
					EventType  string `json:"event_type"`
					ReceivedAt string `json:"received_at"`
					Verify     string `json:"verify"`
					Status     string `json:"status"`
				} `json:"events"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, webhookPath(id)+"/events", nil, &page)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(page.Events) == 0 {
				fmt.Fprintln(out, "No events.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tRECEIVED\tSIGNATURE\tSTATUS")
			for _, e := range page.Events {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(e.ID), orDash(textsafe.Clean(e.EventType)), textsafe.Clean(e.ReceivedAt), orDash(textsafe.Clean(e.Verify)), textsafe.Clean(e.Status))
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

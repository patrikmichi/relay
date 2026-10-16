package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const triggersAPI = "/api/v1"

// triggerStreamBackoff is the wait before a stream reconnects after a failure;
// tests shorten it.
var triggerStreamBackoff = time.Second

// TriggersCmd returns `zuano triggers`, the client for typed triggers: the
// catalog of events a connector can send, the instances that listen for them
// and the stream of what they received.
func TriggersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "triggers",
		Short: "Find trigger types, create triggers and watch their events",
		Long: `A trigger type is an event a connector can send, such as github.issue_opened.
A trigger is an instance of a type with its config, bound to an automation, a
webhook URL, an agent prompt or (for a project key) the project's subscriptions.
The gateway registers the hook with the provider when it can; otherwise it
prints the steps to do by hand.

'stream' prints the events your triggers received as one JSON object per line.
It resumes after a dropped connection and replays what it missed for 24 hours.

An agent session may read; creating a trigger that sends data out (a webhook or
an agent prompt) is held for an approval like any other write.`,
	}
	cmd.AddCommand(triggersTypesCmd(), triggersInfoCmd(), triggersCreateCmd(), triggersListCmd(), triggersDeleteCmd(), triggersRetryCmd(), triggersStreamCmd())
	return cmd
}

type triggerType struct {
	Slug                      string         `json:"slug"`
	Service                   string         `json:"service"`
	Title                     string         `json:"title"`
	Description               string         `json:"description"`
	Kind                      string         `json:"kind"`
	Latency                   string         `json:"latency"`
	PollIntervalSec           int            `json:"pollIntervalSec"`
	Config                    map[string]any `json:"config"`
	Output                    map[string]any `json:"output"`
	Sample                    any            `json:"sample"`
	RequiresEndpointSetup     bool           `json:"requiresEndpointSetup"`
	PerRecipientAuthorization bool           `json:"perRecipientAuthorization"`
	MinRole                   string         `json:"minRole"`
	Registration              struct {
		Method string `json:"method"`
	} `json:"registration"`
}

func (t triggerType) latency() string {
	if t.Latency == "polling" || t.Kind == "poll" {
		if t.PollIntervalSec > 0 {
			return "polling, every " + (time.Duration(t.PollIntervalSec) * time.Second).String()
		}
		return "polling"
	}
	return "realtime"
}

func triggersTypesCmd() *cobra.Command {
	var f approvalFlags
	var service, kind, search string
	cmd := &cobra.Command{
		Use:   "types",
		Short: "List the events a connector can send",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{}
			for k, v := range map[string]string{"service": service, "kind": kind, "q": search} {
				if v != "" {
					q.Set(k, v)
				}
			}
			path := triggersAPI + "/trigger-types"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Types []triggerType `json:"types"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Types) == 0 {
				fmt.Fprintln(out, "No trigger types match.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "TYPE\tDELIVERY\tTITLE")
			for _, t := range doc.Types {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", textsafe.Clean(t.Slug), t.latency(), shortText(t.Title, 60))
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\nSee one with `%s`.\n", brand.Cmd("triggers info <type>"))
			return nil
		},
	}
	f.bind(cmd, "Print the catalog entries as JSON")
	cmd.Flags().StringVar(&service, "service", "", "Only this service, such as github")
	cmd.Flags().StringVar(&kind, "kind", "", "webhook or poll")
	cmd.Flags().StringVar(&search, "search", "", "Words in the title or description")
	return cmd
}

type schemaField struct {
	name, typ, desc string
	required        bool
}

func schemaFields(s map[string]any) []schemaField {
	props, _ := s["properties"].(map[string]any)
	req := map[string]bool{}
	if list, ok := s["required"].([]any); ok {
		for _, r := range list {
			if n, ok := r.(string); ok {
				req[n] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []schemaField
	for _, n := range names {
		p, _ := props[n].(map[string]any)
		typ := "any"
		switch t := p["type"].(type) {
		case string:
			typ = t
		case []any:
			var parts []string
			for _, x := range t {
				parts = append(parts, fmt.Sprint(x))
			}
			typ = strings.Join(parts, "|")
		}
		desc, _ := p["description"].(string)
		out = append(out, schemaField{n, typ, desc, req[n]})
	}
	return out
}

func printFields(w io.Writer, title string, fields []schemaField) {
	if len(fields) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, f := range fields {
		req := ""
		if f.required {
			req = "required"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", textsafe.Strict(f.name), textsafe.Strict(f.typ), req, shortText(f.desc, 70))
	}
	_ = tw.Flush()
}

func triggersInfoCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "info <type>",
		Short: "Show a trigger type's config, payload and a sample event",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == "" || strings.ContainsAny(args[0], "/?#% ") {
				return fmt.Errorf("%q is not a trigger type", textsafe.Clean(args[0]))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Type triggerType `json:"type"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, triggersAPI+"/trigger-types/"+url.PathEscape(args[0]), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			t := doc.Type
			fmt.Fprintf(out, "%s: %s\n%s\n\n", textsafe.Clean(t.Slug), textsafe.Strict(t.Title), textsafe.Strict(t.Description))
			fmt.Fprintf(out, "Delivery:      %s (registered by %s)\n", t.latency(), orDash(t.Registration.Method))
			fmt.Fprintf(out, "Needs role:    %s\n", orDash(t.MinRole))
			if t.RequiresEndpointSetup {
				fmt.Fprintln(out, "Setup:         a custom OAuth app must register its own ingress URL")
			}
			if t.PerRecipientAuthorization {
				fmt.Fprintln(out, "Authorization: each event is checked against the recipient's own account")
			}
			printFields(out, "Config (--config key=value):", schemaFields(t.Config))
			printFields(out, "Event data:", schemaFields(t.Output))
			if t.Sample != nil {
				sample, _ := json.MarshalIndent(cleanJSON(t.Sample), "", "  ")
				fmt.Fprintf(out, "\nSample data:\n%s\n", sample)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the catalog entry as JSON")
	return cmd
}

func triggersCreateCmd() *cobra.Command {
	var f approvalFlags
	var configPairs []string
	var configFile, account, user, automation, webhook, prompt, agentResource, description string
	var deliver bool
	cmd := &cobra.Command{
		Use:   "create <type>",
		Short: "Listen for an event and send it to an automation, a URL, an agent or your project",
		Long: `Creates a trigger of a type and, where the provider has an API, registers the
hook with it using the connected account. Choose exactly one destination:

  --automation atm_...   starts the draft automation (a person still turns it on)
  --webhook URL          posts each event to the URL as a signed delivery
  --agent-prompt TEXT    starts an agent run with the prompt on each event
  --deliver              sends it to the project's webhook subscriptions (project keys)

If registration cannot be done for you, the steps to do by hand are printed.`,
		Example: `  zuano triggers create github.issue_opened --config owner=acme --config repo=web --automation atm_k3j2h1
  zuano triggers create stripe.payment_failed --webhook https://example.com/hooks/stripe`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			binding, err := triggerBinding(automation, webhook, prompt, agentResource, deliver, description)
			if err != nil {
				return err
			}
			config, err := parseParams(configPairs, "")
			if err != nil {
				return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "--param", "--config"))
			}
			if configFile != "" {
				raw, err := os.ReadFile(configFile)
				if err != nil {
					return err
				}
				var fromFile map[string]any
				if json.Unmarshal(raw, &fromFile) != nil || fromFile == nil {
					return fmt.Errorf("%s is not a JSON object", textsafe.Clean(configFile))
				}
				for k, v := range config {
					fromFile[k] = v
				}
				config = fromFile
			}
			body := map[string]any{"type": args[0], "config": config, "binding": binding}
			if account != "" {
				body["connectedAccountId"] = account
			}
			if user != "" {
				body["userId"] = user
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				TriggerID    string `json:"triggerId"`
				Registration struct {
					Status      string   `json:"status"`
					Method      string   `json:"method"`
					ManualSteps []string `json:"manualSteps"`
				} `json:"registration"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, triggersAPI+"/triggers", body, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
			} else {
				fmt.Fprintf(out, "Created trigger %s (registration: %s via %s)\n", textsafe.Clean(res.TriggerID), orDash(res.Registration.Status), orDash(res.Registration.Method))
				for i, step := range res.Registration.ManualSteps {
					fmt.Fprintf(out, "  %d. %s\n", i+1, textsafe.Strict(step))
				}
			}
			if res.Registration.Status == "failed" {
				return fmt.Errorf("the provider refused the registration, so the trigger is off; fix it and run `%s`", brand.Cmd("triggers retry "+res.TriggerID))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringArrayVar(&configPairs, "config", nil, "Config of the type as key=value; repeatable")
	cmd.Flags().StringVar(&configFile, "config-file", "", "JSON file with the config")
	cmd.Flags().StringVar(&account, "account", "", "Connected account (ca_...) the trigger uses")
	cmd.Flags().StringVar(&user, "user", "", "End user's external id, for project keys")
	cmd.Flags().StringVar(&automation, "automation", "", "Start this automation (atm_...)")
	cmd.Flags().StringVar(&webhook, "webhook", "", "Post each event to this URL")
	cmd.Flags().StringVar(&prompt, "agent-prompt", "", "Start an agent run with this prompt")
	cmd.Flags().StringVar(&agentResource, "agent", "", "Agent (resource id) for --agent-prompt")
	cmd.Flags().BoolVar(&deliver, "deliver", false, "Send events to the project's webhook subscriptions")
	cmd.Flags().StringVar(&description, "description", "", "Note shown next to a --webhook subscription")
	return cmd
}

func triggerBinding(automation, webhook, prompt, agent string, deliver bool, description string) (map[string]any, error) {
	n := 0
	for _, set := range []bool{automation != "", webhook != "", prompt != "", deliver} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, errors.New("choose exactly one destination: --automation, --webhook, --agent-prompt or --deliver")
	}
	switch {
	case automation != "":
		id, err := automationID(automation)
		if err != nil {
			return nil, err
		}
		return map[string]any{"kind": "automation", "automationId": id}, nil
	case webhook != "":
		u, err := url.Parse(webhook)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("--webhook %q must be an http(s) URL without credentials", textsafe.Clean(webhook))
		}
		b := map[string]any{"kind": "subscription", "url": webhook}
		if description != "" {
			b["description"] = description
		}
		return b, nil
	case prompt != "":
		b := map[string]any{"kind": "agent_run", "prompt": prompt}
		if agent != "" {
			b["agentResourceId"] = agent
		}
		return b, nil
	}
	return map[string]any{"kind": "deliver"}, nil
}

type triggerDoc struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Enabled            bool   `json:"enabled"`
	ConnectedAccountID string `json:"connectedAccountId"`
	Binding            struct {
		Kind string `json:"kind"`
	} `json:"binding"`
	Registration struct {
		Status string `json:"status"`
	} `json:"registration"`
}

func triggersListCmd() *cobra.Command {
	var f approvalFlags
	var users, accounts, typesF, authConfigs, ids, cursor string
	var showDisabled bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your triggers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{}
			for k, v := range map[string]string{"userIds": users, "connectedAccountIds": accounts, "types": typesF, "authConfigIds": authConfigs, "triggerIds": ids, "cursor": cursor} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if showDisabled {
				q.Set("showDisabled", "true")
			}
			path := triggersAPI + "/triggers"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Triggers   []triggerDoc `json:"triggers"`
				NextCursor string       `json:"nextCursor"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Triggers) == 0 {
				fmt.Fprintf(out, "No triggers. Create one with `%s`.\n", brand.Cmd("triggers create <type> ..."))
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tSTATE\tDESTINATION\tREGISTRATION\tACCOUNT")
			for _, t := range doc.Triggers {
				state := "on"
				if !t.Enabled {
					state = "off"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(t.ID), textsafe.Clean(t.Type), state, orDash(t.Binding.Kind), orDash(t.Registration.Status), orDash(t.ConnectedAccountID))
			}
			_ = tw.Flush()
			if doc.NextCursor != "" {
				fmt.Fprintf(out, "\nMore: --cursor %s\n", textsafe.Clean(doc.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	cmd.Flags().StringVar(&users, "user", "", "Comma-separated end users' external ids")
	cmd.Flags().StringVar(&accounts, "account", "", "Comma-separated connected account ids")
	cmd.Flags().StringVar(&typesF, "type", "", "Comma-separated trigger types")
	cmd.Flags().StringVar(&authConfigs, "auth-config", "", "Comma-separated auth config ids")
	cmd.Flags().StringVar(&ids, "trigger-id", "", "Comma-separated trigger ids")
	cmd.Flags().BoolVar(&showDisabled, "show-disabled", false, "Include triggers that are off")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue from a previous page")
	return cmd
}

func triggerID(arg string) (string, error) {
	if len(arg) < 5 || !strings.HasPrefix(arg, "wtr_") || strings.ContainsAny(arg, "/?#% ") {
		return "", fmt.Errorf("%q is not a trigger id (expected wtr_...)", textsafe.Clean(arg))
	}
	return arg, nil
}

func triggersDeleteCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a trigger and remove its hook at the provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := triggerID(args[0])
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete trigger %s? Its hook at the provider is removed.", id)); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodDelete, triggersAPI+"/triggers/"+url.PathEscape(id), nil, nil); err != nil {
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

func triggersRetryCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "retry <id>",
		Short: "Try to register a trigger's hook at the provider again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := triggerID(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Registration struct {
					Status      string   `json:"status"`
					ManualSteps []string `json:"manualSteps"`
				} `json:"registration"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, triggersAPI+"/triggers/"+url.PathEscape(id)+"/retry-registration", map[string]any{}, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
			} else {
				fmt.Fprintf(out, "Registration of %s: %s\n", id, orDash(res.Registration.Status))
				for i, step := range res.Registration.ManualSteps {
					fmt.Fprintf(out, "  %d. %s\n", i+1, textsafe.Strict(step))
				}
			}
			if res.Registration.Status == "failed" {
				return errors.New("the provider refused the registration again")
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	return cmd
}

// triggerFilters are the stream's query parameters.
type triggerFilters struct{ trigger, typ, service, account, user string }

func (tf triggerFilters) query() string {
	q := url.Values{}
	for k, v := range map[string]string{"trigger_id": tf.trigger, "type": tf.typ, "service": tf.service, "connected_account_id": tf.account, "user_id": tf.user} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q.Encode()
}

// bind adds the filter flags; the stream command and the dev listener name the
// trigger and user flags differently.
func (tf *triggerFilters) bind(cmd *cobra.Command, triggerFlag, userFlag string) {
	cmd.Flags().StringVar(&tf.trigger, triggerFlag, "", "Only this trigger")
	cmd.Flags().StringVar(&tf.typ, "type", "", "Only this event type; github.* matches a prefix")
	cmd.Flags().StringVar(&tf.service, "service", "", "Only this service")
	cmd.Flags().StringVar(&tf.account, "account", "", "Only this connected account")
	cmd.Flags().StringVar(&tf.user, userFlag, "", "Only this end user's external id")
}

// sseEvent is one message of the trigger stream.
type sseEvent struct {
	ID, Name string
	Data     []byte
}

// streamTriggerEvents follows the trigger stream until ctx ends, handing every
// event to handle. It resumes from the last id after any interruption: the
// gateway closes each response before its time limit, and the network drops.
func streamTriggerEvents(ctx context.Context, c *client.Client, tf triggerFilters, lastID string, handle func(sseEvent) error, note func(string)) error {
	path := triggersAPI + "/triggers/stream"
	if q := tf.query(); q != "" {
		path += "?" + q
	}
	wait := triggerStreamBackoff
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		got, err := streamOnce(ctx, c, path, &lastID, handle)
		switch {
		case err == nil && got:
			wait = triggerStreamBackoff
			continue
		case err == nil:
		default:
			var rl *client.RateLimitedError
			var fatal *streamFatal
			switch {
			case errors.As(err, &fatal):
				return fatal.err
			case errors.As(err, &rl):
				d := 5 * time.Second
				if n, e := strconv.Atoi(rl.RetryAfter); e == nil && n > 0 && n <= 120 {
					d = time.Duration(n) * time.Second
				}
				note(fmt.Sprintf("too many streams on this key; trying again in %s", d))
				wait = d
			case ctx.Err() != nil:
				return nil
			default:
				note("stream interrupted: " + textsafe.Clean(err.Error()) + "; reconnecting")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

// streamFatal marks an error that reconnecting cannot fix.
type streamFatal struct{ err error }

func (e *streamFatal) Error() string { return e.err.Error() }

func streamOnce(ctx context.Context, c *client.Client, path string, lastID *string, handle func(sseEvent) error) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.GatewayURL+path, nil)
	if err != nil {
		return false, &streamFatal{err}
	}
	req.Header.Set("Accept", "text/event-stream")
	if *lastID != "" {
		req.Header.Set("Last-Event-ID", *lastID)
	}
	resp, err := c.Do(req)
	if err != nil {
		// A refusal the gateway meant (plan, suspension, step-up, revoked token) will not clear by reconnecting.
		if errors.As(err, new(*client.EntitlementError)) || errors.Is(err, errTokenRevoked) || errors.As(err, new(*client.StepUpRequiredError)) || errors.As(err, new(*client.ReauthRequiredError)) {
			return false, &streamFatal{err}
		}
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return false, &streamFatal{errors.New("the stream needs an API key; a browser session is not enough")}
	case resp.StatusCode == http.StatusTooManyRequests:
		return false, &client.RateLimitedError{RetryAfter: resp.Header.Get("Retry-After")}
	case resp.StatusCode >= 500:
		return false, fmt.Errorf("the gateway answered %d", resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxApprovalBody))
		return false, &streamFatal{gatewayStatusError(resp.StatusCode, b)}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var ev sseEvent
	var data bytes.Buffer
	got := false
	flush := func() error {
		defer func() { ev = sseEvent{}; data.Reset() }()
		if data.Len() == 0 {
			if ev.ID != "" {
				*lastID = ev.ID
			}
			return nil
		}
		ev.Data = append([]byte(nil), data.Bytes()...)
		if ev.ID != "" {
			*lastID = ev.ID
		}
		got = true
		if err := handle(ev); err != nil {
			return &streamFatal{err}
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return got, err
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id:"):
			ev.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := flush(); err != nil {
		return got, err
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return got, err
	}
	return got, nil
}

func triggersStreamCmd() *cobra.Command {
	var f approvalFlags
	var tf triggerFilters
	var outFile string
	cmd := &cobra.Command{
		Use:   "stream",
		Short: "Print the events your triggers receive, one JSON object per line",
		Long: `Follows the gateway's event stream with your API key and prints each event
envelope as one line of JSON. Filters combine. If the connection drops it
reconnects and resumes from the last event; events from the last 24 hours that
were missed are replayed. Stop it with Ctrl+C.

The stream is for development: it is not signed and keeps no delivery
guarantees. Use a webhook subscription in production. Streaming costs nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			tee := io.Discard
			if outFile != "" {
				fh, err := os.OpenFile(outFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				defer fh.Close()
				tee = fh
			}
			handle := func(ev sseEvent) error {
				var buf bytes.Buffer
				if err := json.Compact(&buf, ev.Data); err != nil {
					return fmt.Errorf("the gateway sent an event that is not JSON: %w", err)
				}
				buf.WriteByte('\n')
				_, _ = out.Write(buf.Bytes())
				_, _ = tee.Write(buf.Bytes())
				return nil
			}
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			return streamTriggerEvents(ctx, c, tf, "", handle, func(s string) { fmt.Fprintln(cmd.ErrOrStderr(), s) })
		},
	}
	f.bind(cmd, "Unused: the stream is always JSON lines")
	_ = cmd.Flags().MarkHidden("json")
	tf.bind(cmd, "trigger", "user")
	cmd.Flags().StringVar(&outFile, "out", "", "Also append the lines to this file")
	return cmd
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

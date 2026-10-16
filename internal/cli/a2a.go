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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/a2a"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// maxA2AMessage bounds a message read from a file.
const maxA2AMessage = 1 << 20

// A2ACmd returns `zuano a2a`, the client for agent-to-agent governance.
func A2ACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "a2a",
		Short: "Call agents through the gateway and see who delegated what to whom",
		Long: `Agents call other agents through the gateway, which checks who may call whom,
keeps a chain of every delegation, applies approvals to every effect in it and
can stop the whole chain. These commands register and read agents, send them a
message, show chains and the agent graph, and explain a decision.

Exit status
  10  the call was denied (policy, edge, DLP or approval)
  12  the call is held for an approval; the approval page link is printed
  13  rate limited
  17  --wait ended before the task did
  31  a plan quota or spend cap is used up
  50  loop, depth or chain limit
  51  the chain was killed
  52  the chain's budget is used up
  53  the agent's card changed and is quarantined`,
	}
	cmd.AddCommand(a2aAgentsCmd(), a2aCardCmd(), a2aCallCmd(), a2aChainsCmd(), a2aGraphCmd(), a2aExplainCmd())
	return cmd
}

// a2aSend sends one request, turning a held call into exit 12 and a refusal
// whose reason has an exit status into that status.
func a2aSend(cmd *cobra.Command, c *client.Client, kind requestKind, jsonOut bool, method, path string, body, out any) ([]byte, error) {
	status, raw, err := approvalsDo(cmd.Context(), c, kind, method, path, body)
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
		if refusal := policyRefusal(raw, nil); refusal != nil {
			return nil, refusal
		}
		return nil, gatewayStatusError(status, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return raw, nil
}

func a2aSlug(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, "/?#% \t\n") {
		return "", fmt.Errorf("%q is not an agent slug", textsafe.Strict(s))
	}
	return url.PathEscape(s), nil
}

func shortText(s string, max int) string {
	s = strings.Join(strings.Fields(textsafe.Strict(s)), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func a2aAgentsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agents", Short: "The registry of callable agents"}
	cmd.AddCommand(a2aAgentsListCmd(), a2aAgentsShowCmd(), a2aAgentsRegisterCmd(), a2aAgentsChangesCmd(), a2aAgentsReviewCmd())
	return cmd
}

func a2aAgentsListCmd() *cobra.Command {
	var f approvalFlags
	var kind string
	var external bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the agents you may call",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{}
			if kind != "" {
				q.Set("kind", kind)
			}
			if external {
				q.Set("external", "1")
			}
			path := "/api/a2a/agents"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Agents []a2a.Agent `json:"agents"`
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
			if len(doc.Agents) == 0 {
				fmt.Fprintln(out, "No agents. An administrator registers them with `"+brand.Cmd("a2a agents register --card-url <url>")+"`.")
				return nil
			}
			rows := make([]map[string]any, len(doc.Agents))
			for i, a := range doc.Agents {
				where := a.Kind
				if a.External() {
					where = "external"
				}
				rows[i] = map[string]any{"slug": a.Slug, "name": a.Name, "kind": where, "status": a.Status, "card": cardLabel(a), "skills": len(a.Skills), "changes": a.PendingChanges}
			}
			printTable(out, rows, []column{{"SLUG", "slug"}, {"NAME", "name"}, {"KIND", "kind"}, {"STATUS", "status"}, {"CARD", "card"}, {"SKILLS", "skills"}, {"CHANGES", "changes"}})
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&kind, "kind", "", "Only hosted, org_service or external agents")
	cmd.Flags().BoolVar(&external, "external", false, "Only agents outside your organisation")
	return cmd
}

func cardLabel(a a2a.Agent) string {
	label := a.CardState
	if label == "" {
		label = "-"
	}
	if a.Signature != "" {
		label += " (" + a.Signature + ")"
	}
	return label
}

func a2aAgentsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <slug>",
		Short: "Show an agent, its endpoint and the skills its pinned card describes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := a2aSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Agent a2a.Agent `json:"agent"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/a2a/agents/"+slug, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			a := doc.Agent
			fmt.Fprintf(out, "%s (%s)\n  kind:      %s\n  status:    %s\n  card:      %s\n  on change: %s\n  exposure:  %s\n  endpoint:  %s\n", textsafe.Strict(a.Name), textsafe.Strict(a.Slug), textsafe.Strict(a.Kind), textsafe.Strict(a.Status), textsafe.Strict(cardLabel(a)), textsafe.Strict(orDash(a.OnCardChange)), textsafe.Strict(orDash(strings.Join(a.Exposure, ", "))), textsafe.Strict(orDash(a.Endpoint)))
			if len(a.Residency) > 0 || len(a.DLPCategories) > 0 {
				fmt.Fprintf(out, "  residency: %s  data: %s\n", textsafe.Strict(orDash(strings.Join(a.Residency, ", "))), textsafe.Strict(orDash(strings.Join(a.DLPCategories, ", "))))
			}
			if a.PendingChanges > 0 {
				fmt.Fprintf(out, "  %d card change(s) wait for review: `%s`\n", a.PendingChanges, brand.Cmd("a2a agents changes "+a.Slug))
			}
			if len(a.Skills) == 0 {
				fmt.Fprintln(out, "  no skills")
				return nil
			}
			rows := make([]map[string]any, len(a.Skills))
			for i, s := range a.Skills {
				rows[i] = map[string]any{"id": s.ID, "risk": s.Risk, "source": s.Source, "description": shortText(s.Description, 70)}
			}
			fmt.Fprintln(out, "\nSkills (the pinned card's text):")
			printTable(out, rows, []column{{"SKILL", "id"}, {"RISK", "risk"}, {"CLASSIFIED BY", "source"}, {"DESCRIPTION", "description"}})
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func a2aAgentsRegisterCmd() *cobra.Command {
	var f approvalFlags
	var cardURL, auth string
	var internal bool
	cmd := &cobra.Command{
		Use:   "register --card-url <url>",
		Short: "Register an agent from its card (administrators)",
		Long: `The gateway fetches the card, checks its signature and screens its text, then
waits for an administrator to review it. Approving the pinned card needs a
passkey in a browser, so this prints the review link and decides nothing.
Refused while ZUANO_AGENT is set (exit 10).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("a2a agents register")); err != nil {
				return err
			}
			u, err := url.Parse(cardURL)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				return errors.New("--card-url must be the agent card's https address")
			}
			mode := map[string]string{"none": "none", "stored": "stored", "per-user": "per_user"}[auth]
			if mode == "" {
				return fmt.Errorf("invalid --auth %q: use none, stored or per-user", auth)
			}
			kind := "external"
			if internal {
				kind = "org_service"
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Agent     a2a.Agent `json:"agent"`
				ReviewURL string    `json:"review_url"`
			}
			raw, err := a2aSend(cmd, c, controlRequest, f.jsonOut, http.MethodPost, "/api/a2a/agents", map[string]any{"kind": kind, "cardUrl": cardURL, "auth": mode}, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			link, lerr := a2aLink(c.GatewayURL, res.ReviewURL, "/agents/a2a/"+url.PathEscape(res.Agent.Slug))
			if lerr != nil {
				return lerr
			}
			fmt.Fprintf(out, "Registered %s. Review its card and approve it with your passkey:\n  %s\n", textsafe.Strict(res.Agent.Slug), textsafe.Strict(link))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&cardURL, "card-url", "", "Address of the agent card")
	cmd.Flags().StringVar(&auth, "auth", "none", "How the gateway authenticates to it: none, stored or per-user")
	cmd.Flags().BoolVar(&internal, "internal", false, "The agent is a service of your organisation, not an external party")
	_ = cmd.MarkFlagRequired("card-url")
	return cmd
}

// a2aLink makes the review link absolute on the gateway: the gateway's own
// answer if it is a path or an address on the gateway's host, else fallback.
func a2aLink(gateway, given, fallback string) (string, error) {
	if given == "" {
		given = fallback
	}
	if strings.HasPrefix(given, "/") {
		return gatewayLink(gateway, given)
	}
	g, gerr := url.Parse(gateway)
	u, uerr := url.Parse(given)
	if gerr != nil || uerr != nil || u.Host != g.Host || u.Scheme != g.Scheme {
		return gatewayLink(gateway, fallback)
	}
	return u.String(), nil
}

type cardChange struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Status   string `json:"status"`
	At       string `json:"at"`
}

func a2aAgentsChangesCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "changes <slug>",
		Short: "List changes to an agent's card that wait for review",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := a2aSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Changes []cardChange `json:"changes"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/a2a/agents/"+slug+"/changes", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Changes) == 0 {
				fmt.Fprintln(out, "No card changes wait for review.")
				return nil
			}
			rows := make([]map[string]any, len(doc.Changes))
			for i, ch := range doc.Changes {
				rows[i] = map[string]any{"id": ch.ID, "severity": ch.Severity, "status": ch.Status, "at": ch.At, "summary": shortText(ch.Summary, 70)}
			}
			printTable(out, rows, []column{{"ID", "id"}, {"SEVERITY", "severity"}, {"STATUS", "status"}, {"AT", "at"}, {"SUMMARY", "summary"}})
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func a2aAgentsReviewCmd() *cobra.Command {
	var f approvalFlags
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "review <slug>",
		Short: "Open the page where a card or a card change is approved with a passkey",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAutomationInAgent(brand.Cmd("a2a agents review")); err != nil {
				return err
			}
			slug, err := a2aSlug(args[0])
			if err != nil {
				return err
			}
			return openAutomationPage(cmd, f.gatewayURL, "/agents/a2a/"+slug, noBrowser, "Review the card there")
		},
	}
	f.bind(cmd, "Unused: the command only opens a page")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}

// cleanJSON removes control and invisible characters from every string of a
// decoded document, so a card from an outside party cannot rewrite the screen.
func cleanJSON(v any) any {
	switch t := v.(type) {
	case string:
		return textsafe.Strict(t)
	case []any:
		for i := range t {
			t[i] = cleanJSON(t[i])
		}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[textsafe.Strict(k)] = cleanJSON(x)
		}
		return out
	}
	return v
}

func a2aCardCmd() *cobra.Command {
	var f approvalFlags
	var live bool
	cmd := &cobra.Command{
		Use:   "card <slug>",
		Short: "Print an agent's pinned card, or the live one",
		Long:  "The pinned card is the one an administrator approved, and the only text callers and models are shown. --live fetches what the agent publishes now, which may differ.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := a2aSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			version := "pinned"
			if live {
				version = "live"
			}
			var doc struct {
				Card any `json:"card"`
			}
			if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/a2a/agents/"+slug+"/card?version="+version, nil, &doc); err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			return enc.Encode(cleanJSON(doc.Card))
		},
	}
	f.bind(cmd, "Accepted for symmetry: the card is always printed as JSON")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&live, "live", false, "Fetch the card the agent publishes now")
	return cmd
}

func readMessage(arg string) (string, error) {
	if path, ok := strings.CutPrefix(arg, "@"); ok {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxA2AMessage+1))
		if err != nil {
			return "", err
		}
		if len(b) > maxA2AMessage {
			return "", fmt.Errorf("%s is larger than 1 MiB", textsafe.Strict(path))
		}
		return string(b), nil
	}
	return arg, nil
}

func a2aCallCmd() *cobra.Command {
	var f approvalFlags
	var skill, message, contextID string
	var files []string
	var wait int
	var stream bool
	cmd := &cobra.Command{
		Use:   "call <slug> --skill <id> --message <text|@file>",
		Short: "Send a message to an agent through the gateway",
		Long: `The gateway checks the edge from you to the agent, the chain's limits and DLP,
and holds the call for an approval when the hop or an effect further down needs
one (exit 12, with the approval page link). --wait waits that many seconds for
the task to finish and exits 17 if it does not. --stream follows the task as it
runs. --file param=@path uploads a file and passes it by reference.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := a2aSlug(args[0])
			if err != nil {
				return err
			}
			if skill == "" || message == "" {
				return errors.New("--skill and --message are required")
			}
			if wait < 0 || wait > 280 {
				return errors.New("--wait must be between 0 and 280 seconds")
			}
			text, err := readMessage(message)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			body := map[string]any{"skill": skill, "message": text}
			if contextID != "" {
				body["context_id"] = contextID
			}
			if wait > 0 {
				body["wait_seconds"] = wait
			}
			if len(files) > 0 {
				refs, err := uploadCallFiles(cmd.Context(), c, files, nil, nil)
				if err != nil {
					return err
				}
				var list []map[string]string
				for _, r := range refs {
					param, ref, _ := strings.Cut(r, "=file:")
					list = append(list, map[string]string{"param": param, "file_id": ref})
				}
				body["files"] = list
			}
			path := "/api/a2a/agents/" + slug + "/messages"
			var task a2a.Task
			if stream {
				task, err = a2aStream(cmd, c, path, body, f.jsonOut, wait)
				if err != nil {
					return err
				}
			} else {
				var res struct {
					Task a2a.Task `json:"task"`
				}
				raw, err := a2aSend(cmd, c, toolCallRequest, f.jsonOut, http.MethodPost, path, body, &res)
				if err != nil {
					return err
				}
				task = res.Task
				if f.jsonOut {
					printRaw(cmd.OutOrStdout(), raw)
				}
			}
			return finishTask(cmd, task, f.jsonOut, wait > 0 || stream, !stream)
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	fl := cmd.Flags()
	fl.StringVar(&skill, "skill", "", "The skill id on the agent's pinned card")
	fl.StringVar(&message, "message", "", "The message, or @file to read it from a file")
	fl.StringArrayVar(&files, "file", nil, "param=@path: upload a file and pass it by reference (repeatable)")
	fl.StringVar(&contextID, "context", "", "Continue an earlier conversation")
	fl.IntVar(&wait, "wait", 0, "Wait up to this many seconds for the task to finish")
	fl.BoolVar(&stream, "stream", false, "Follow the task's progress as it runs")
	return cmd
}

// finishTask prints a task and ends with the status its state calls for.
func finishTask(cmd *cobra.Command, t a2a.Task, jsonOut, waited, printBody bool) error {
	out := cmd.OutOrStdout()
	if !jsonOut && printBody {
		printTask(out, t)
	}
	switch t.State {
	case "COMPLETED":
		return nil
	case "INPUT_REQUIRED":
		if t.Approval != nil {
			if !jsonOut {
				fmt.Fprintf(out, "Approval required: %s.%s (%s) is held.\n  approval: %s\n  page:     %s\n", textsafe.Strict(t.Approval.Effect.Service), textsafe.Strict(t.Approval.Effect.Tool), textsafe.Strict(t.Approval.Effect.Risk), textsafe.Strict(t.Approval.ID), textsafe.Strict(t.Approval.URL))
				fmt.Fprintf(out, "Decide it in the browser, then run the call again, or collect results with `%s`.\n", brand.Cmd("approvals wait "+t.Approval.ID))
			}
			return &ExitError{Code: exitPending}
		}
		return &ExitError{Code: exitPending, Msg: "the agent needs more input"}
	case "AUTH_REQUIRED":
		msg := "the agent needs an account connection first"
		if t.ConnectURL != "" {
			msg += ": " + textsafe.Strict(t.ConnectURL)
		}
		return &ExitError{Code: 1, Msg: msg}
	case "FAILED", "REJECTED", "CANCELED":
		if code, ok := refusalCodes[t.Reason]; ok {
			return &ExitError{Code: code, Msg: refusalMessage(t)}
		}
		return &ExitError{Code: 1, Msg: refusalMessage(t)}
	}
	if waited {
		return &ExitError{Code: exitWaitTimeout, Msg: fmt.Sprintf("task %s is still %s", textsafe.Strict(t.ID), strings.ToLower(textsafe.Strict(t.State)))}
	}
	return nil
}

func refusalMessage(t a2a.Task) string {
	msg := fmt.Sprintf("the task %s", strings.ToLower(textsafe.Strict(t.State)))
	if t.Reason != "" {
		msg += " (" + textsafe.Strict(t.Reason) + ")"
	}
	if t.Message != "" {
		msg += ": " + shortText(t.Message, 200)
	}
	return msg
}

func printTask(out io.Writer, t a2a.Task) {
	fmt.Fprintf(out, "task %s  %s", textsafe.Strict(t.ID), textsafe.Strict(t.State))
	if t.ContextID != "" {
		fmt.Fprintf(out, "  context %s", textsafe.Strict(t.ContextID))
	}
	fmt.Fprintln(out)
	for _, text := range t.Texts() {
		fmt.Fprintln(out, textsafe.Strict(text))
	}
	for _, a := range t.Artifacts {
		for _, p := range a.Parts {
			if p.FileID != "" {
				fmt.Fprintf(out, "file %s (%s); fetch it with `%s`\n", textsafe.Strict(p.FileID), textsafe.Strict(orDash(p.Name)), brand.Cmd("files get "+p.FileID))
			}
		}
	}
}

// a2aStream follows a task's server-sent events: each event is a JSON
// {"task": TASK, "final": bool}. It returns the last task.
func a2aStream(cmd *cobra.Command, c *client.Client, path string, body map[string]any, jsonOut bool, wait int) (a2a.Task, error) {
	limit := 280 * time.Second
	if wait > 0 {
		limit = time.Duration(wait) * time.Second
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), limit)
	defer cancel()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.GatewayURL+path, bytes.NewReader(raw))
	if err != nil {
		return a2a.Task{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		return a2a.Task{}, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxApprovalBody))
		if held := pendingFromREST(resp.StatusCode, b); held != nil {
			return a2a.Task{}, handlePending(cmd.Context(), c, held, jsonOut, callOpts{}, cmd.OutOrStdout())
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxApprovalBody))
		if refusal := policyRefusal(b, nil); refusal != nil {
			return a2a.Task{}, refusal
		}
		return a2a.Task{}, gatewayStatusError(resp.StatusCode, b)
	}
	out := cmd.OutOrStdout()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var last a2a.Task
	var data bytes.Buffer
	shown := map[string]int{}
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		defer data.Reset()
		var ev struct {
			Task  a2a.Task `json:"task"`
			Final bool     `json:"final"`
		}
		if err := json.Unmarshal(data.Bytes(), &ev); err != nil {
			return fmt.Errorf("the gateway sent an event that is not JSON: %w", err)
		}
		last = ev.Task
		if jsonOut {
			fmt.Fprintln(out, data.String())
			return nil
		}
		fmt.Fprintf(out, "[%s] %s\n", textsafe.Strict(ev.Task.State), shortText(ev.Task.Message, 160))
		texts := ev.Task.Texts()
		for i := shown[ev.Task.ID]; i < len(texts); i++ {
			fmt.Fprintln(out, textsafe.Strict(texts[i]))
		}
		shown[ev.Task.ID] = len(texts)
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return last, err
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := flush(); err != nil {
		return last, err
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return last, err
	}
	if last.ID == "" {
		return last, errors.New("the stream ended without a task")
	}
	if !last.Terminal() && ctx.Err() != nil {
		return last, nil
	}
	return last, nil
}

func a2aChainsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "chains", Short: "Delegation chains: who asked whom, and what it cost"}
	cmd.AddCommand(a2aChainsListCmd(), a2aChainsShowCmd(), a2aChainsKillCmd())
	return cmd
}

func a2aChainsListCmd() *cobra.Command {
	var f approvalFlags
	var mine bool
	var status, agent string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List chains, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{}
			if mine {
				q.Set("origin", "me")
			}
			if status != "" {
				q.Set("status", status)
			}
			if agent != "" {
				q.Set("agent", agent)
			}
			path := "/api/a2a/chains"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Chains []a2a.Chain `json:"chains"`
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
			if len(doc.Chains) == 0 {
				fmt.Fprintln(out, "No chains match.")
				return nil
			}
			rows := make([]map[string]any, len(doc.Chains))
			for i, ch := range doc.Chains {
				rows[i] = map[string]any{"id": ch.ID, "status": ch.Status, "origin": ch.Origin, "depth": ch.Depth, "hops": len(ch.Hops), "started": ch.StartedAt}
			}
			printTable(out, rows, []column{{"ID", "id"}, {"STATUS", "status"}, {"ORIGIN", "origin"}, {"DEPTH", "depth"}, {"HOPS", "hops"}, {"STARTED", "started"}})
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&mine, "mine", false, "Only chains you started")
	cmd.Flags().StringVar(&status, "status", "", "Only chains in this state (running, completed, killed, failed)")
	cmd.Flags().StringVar(&agent, "agent", "", "Only chains that include this agent")
	return cmd
}

func a2aChainsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a chain as a tree with its effects, budget and audit check",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := a2aSlug(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Chain a2a.Chain `json:"chain"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/a2a/chains/"+id, nil, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			doc.Chain.Render(cmd.OutOrStdout())
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func a2aChainsKillCmd() *cobra.Command {
	var f approvalFlags
	var reason string
	cmd := &cobra.Command{
		Use:   "kill <id> --reason <text>",
		Short: "Stop a chain: running hops are cancelled and its pending approvals withdrawn",
		Long:  "Stopping is always allowed for the person who started the chain, an administrator and an agent on its own chain. It takes effect on every hop within seconds.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := a2aSlug(args[0])
			if err != nil {
				return err
			}
			if strings.TrimSpace(reason) == "" {
				return errors.New("--reason is required: it goes into the audit trail")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := a2aSend(cmd, c, controlRequest, f.jsonOut, http.MethodPost, "/api/a2a/chains/"+id+"/kill", map[string]any{"reason": reason}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Chain %s killed.\n", textsafe.Strict(args[0]))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&reason, "reason", "", "Why the chain is stopped")
	return cmd
}

func a2aGraphCmd() *cobra.Command {
	var f approvalFlags
	var format, from, to, window string
	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Export the agent graph: allowed edges and observed traffic",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "dot" && format != "json" {
				return fmt.Errorf("invalid --format %q: use dot or json", format)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{"window": {window}}
			if from != "" {
				q.Set("from", from)
			}
			if to != "" {
				q.Set("to", to)
			}
			var g a2a.Graph
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/a2a/graph?"+q.Encode(), nil, &g)
			if err != nil {
				return err
			}
			if format == "json" || f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), g.DOT())
			return nil
		},
	}
	f.bind(cmd, "Same as --format json")
	cmd.Flags().StringVar(&format, "format", "dot", "dot or json")
	cmd.Flags().StringVar(&from, "from", "", "Only edges from this node")
	cmd.Flags().StringVar(&to, "to", "", "Only edges to this node")
	cmd.Flags().StringVar(&window, "window", "30d", "How far back observed traffic counts")
	return cmd
}

func a2aExplainCmd() *cobra.Command {
	var f approvalFlags
	var from, to, skill string
	cmd := &cobra.Command{
		Use:   "explain --from <identity> --to <agent> --skill <id>",
		Short: "Explain which rule decides whether a caller may use an agent's skill",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" || to == "" || skill == "" {
				return errors.New("--from, --to and --skill are required")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{"from": {from}, "to": {to}, "skill": {skill}}
			var doc struct {
				Decision struct {
					Result    string   `json:"result"`
					Reasons   []string `json:"reasons"`
					Approvals []string `json:"approvals"`
					Ceilings  []string `json:"ceilings"`
					Edge      struct {
						ID   string `json:"id"`
						File string `json:"file"`
						Line int    `json:"line"`
					} `json:"edge"`
				} `json:"decision"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/a2a/explain?"+q.Encode(), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			d := doc.Decision
			fmt.Fprintf(out, "%s → %s.%s: %s\n", textsafe.Strict(from), textsafe.Strict(to), textsafe.Strict(skill), strings.ToUpper(textsafe.Strict(d.Result)))
			if d.Edge.ID != "" {
				where := textsafe.Strict(d.Edge.File)
				if d.Edge.Line > 0 {
					where = fmt.Sprintf("%s:%d", where, d.Edge.Line)
				}
				fmt.Fprintf(out, "  decided by edge %s (%s)\n", textsafe.Strict(d.Edge.ID), textsafe.Strict(orDash(where)))
			}
			for _, r := range d.Reasons {
				fmt.Fprintf(out, "  - %s\n", shortText(r, 200))
			}
			for _, a := range d.Approvals {
				fmt.Fprintf(out, "  approval needed: %s\n", shortText(a, 200))
			}
			for _, cl := range d.Ceilings {
				fmt.Fprintf(out, "  ceiling: %s\n", shortText(cl, 200))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&from, "from", "", "The caller: an identity id or name")
	cmd.Flags().StringVar(&to, "to", "", "The agent's slug")
	cmd.Flags().StringVar(&skill, "skill", "", "The skill id")
	return cmd
}

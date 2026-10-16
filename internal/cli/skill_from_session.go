package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/redact"
	"github.com/patrikmichi/zuano/internal/textsafe"
	"github.com/patrikmichi/zuano/internal/transcripts"
)

type fromSessionOpts struct {
	approvalFlags
	agent, conversation, into, set, goal, then string
	pick, last, showRedacted, consent, wait    bool
	accept, open                               bool
	fromTurn, toTurn                           int
	redactTerms                                []string
	redactFile                                 string
}

func skillFromSessionCmd() *cobra.Command {
	var o fromSessionOpts
	cmd := &cobra.Command{
		Use:   "from-session [<file>|<run-id>|-]",
		Short: "Draft a skill and eval cases from a coding-agent session",
		Long: `Reads a session on this machine, removes secrets and personal data from it
here, and uploads only the redacted copy; the file itself never leaves the
machine. The gateway redacts it again and drafts a skill and eval cases from it
for you to review. Nothing is published.

The source is a file, '-' for standard input, a run id (crn_...) the gateway
reads itself, or mcp-session:<ref> with --goal. Without an argument on a
terminal you choose from your recent sessions. --agent names the format:
claude, codex, gemini, opencode, cline, claude-ai, chatgpt, markdown, text; the
default recognises it. For OpenCode, pass the session id and zuano runs
'opencode export' for it. An export with several conversations needs
--conversation <id>, or asks.

--show-redacted prints exactly what would be uploaded and the redaction report,
and uploads nothing. Uploading asks you to accept the gateway's consent text;
without a terminal pass --consent.

Uploading a local transcript is for a person: it is refused while ZUANO_AGENT is
set (exit 10); an agent uses the gateway's skill_from_session tool.

exit status
  0   done
  2   the file could not be read or parsed, or consent is missing
  10  refused for an agent session
  22  the session is too large for your plan: trim with --from-turn/--to-turn
  30-33  the plan does not allow it`,
		Example: `  zuano skill from-session --last --show-redacted
  zuano skill from-session ./session.jsonl --wait --accept
  zuano skill from-session crn_k3j2h1 --into pr-triage`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			return runFromSession(cmd, arg, o)
		},
	}
	f := cmd.Flags()
	o.bind(cmd, "Print the result as JSON")
	f.StringVar(&o.agent, "agent", "auto", "Format: auto, claude, codex, gemini, opencode, cline, claude-ai, chatgpt, markdown, text")
	f.BoolVar(&o.pick, "pick", false, "Choose from your recent local sessions (the default on a terminal without an argument)")
	f.BoolVar(&o.last, "last", false, "Use the newest session, of --agent if given, preferring the current project")
	f.StringVar(&o.conversation, "conversation", "", "Conversation id inside a claude.ai or ChatGPT export")
	f.IntVar(&o.fromTurn, "from-turn", 0, "First turn to keep (1-based)")
	f.IntVar(&o.toTurn, "to-turn", 0, "Last turn to keep")
	f.StringArrayVar(&o.redactTerms, "redact-term", nil, "Also remove this text (repeatable)")
	f.StringVar(&o.redactFile, "redact-file", "", "Also remove every line of this file")
	f.BoolVar(&o.showRedacted, "show-redacted", false, "Print the redacted session and report; upload nothing")
	f.StringVar(&o.into, "into", "", "Add the cases to this existing skill instead of drafting a new one")
	f.StringVar(&o.set, "set", "", "With --into: the eval set to add them to")
	f.StringVar(&o.goal, "goal", "", "What you were trying to do (needed for mcp-session:<ref>)")
	f.BoolVar(&o.consent, "consent", false, "Accept the consent text without asking (needed without a terminal)")
	f.BoolVar(&o.wait, "wait", false, "Follow the extraction and print the proposal")
	f.BoolVar(&o.accept, "accept", false, "With --wait: accept the proposal unchanged")
	f.StringVar(&o.then, "then", "", "With --accept: evals or evals-optimize")
	f.BoolVar(&o.open, "open", false, "Open the review page")
	cmd.AddCommand(sessionListCmd(), sessionShowCmd(), sessionDiscardCmd())
	return cmd
}

func (o *fromSessionOpts) terms() ([]string, error) {
	terms := append([]string(nil), o.redactTerms...)
	if o.redactFile != "" {
		data, err := os.ReadFile(o.redactFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", textsafe.Clean(o.redactFile), err)
		}
		terms = append(terms, strings.Split(string(data), "\n")...)
	}
	return terms, nil
}

// sessionSource is what the argument names.
type sessionSource struct {
	runID      string
	sessionRef string
	session    *transcripts.Session
}

func runFromSession(cmd *cobra.Command, arg string, o fromSessionOpts) error {
	if config.AgentMode() {
		return &ExitError{Code: exitDenied, Msg: "uploading a local transcript is for a person: it is refused while ZUANO_AGENT is set; an agent uses the gateway's skill_from_session tool, which asks you to review it first"}
	}
	if o.accept && !o.wait {
		return errors.New("--accept needs --wait, so the proposal is seen before it is accepted")
	}
	if o.then != "" && !o.accept {
		return errors.New("--then needs --accept")
	}
	then, err := thenValue(o.then)
	if err != nil {
		return err
	}
	if o.set != "" && o.into == "" {
		return errors.New("--set needs --into")
	}
	terms, err := o.terms()
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	src, err := loadSessionSource(cmd, arg, o)
	if err != nil {
		return err
	}
	body := map[string]any{}
	var session *transcripts.Session
	switch {
	case src.runID != "":
		body["source"], body["runId"] = "agent_run", src.runID
	case src.sessionRef != "":
		if o.goal == "" {
			return &ExitError{Code: exitSessionUsage, Msg: "mcp-session:<ref> needs --goal, because the trace has no conversation"}
		}
		body["source"], body["sessionRef"], body["goal"] = "mcp_session", src.sessionRef, o.goal
	default:
		session = src.session
		if err := session.Trim(o.fromTurn, o.toTurn); err != nil {
			return &ExitError{Code: exitSessionUsage, Msg: err.Error()}
		}
		redact.Session(session, terms)
		for _, w := range session.Warnings {
			fmt.Fprintf(errOut, "warning: %s\n", textsafe.Clean(w))
		}
		if o.showRedacted {
			return printRedacted(out, session, o.jsonOut)
		}
		printRedactionSummary(errOut, session)
	}
	if o.showRedacted {
		return &ExitError{Code: exitSessionUsage, Msg: "--show-redacted needs a local session; this source is read and redacted by the gateway"}
	}

	c, err := approvalsClient(o.gatewayURL)
	if err != nil {
		return err
	}
	stmt, err := fetchConsent(cmd.Context(), c)
	if err != nil {
		return err
	}
	if err := acceptConsent(cmd, o.consent, stmt); err != nil {
		return err
	}
	if session != nil {
		body = sessionUpload(session, stmt.Version, [2]int{o.fromTurn, o.toTurn})
	}
	body["consent"] = map[string]any{"statementVersion": stmt.Version}
	if len(terms) > 0 && session == nil {
		body["redactTerms"] = terms
	}
	cap, err := uploadCapture(cmd.Context(), c, body)
	if err != nil {
		return err
	}
	review := capturePage(c, cap.CaptureID)
	if !o.jsonOut {
		fmt.Fprintf(out, "Uploaded %s: %d turns, %d tool calls, kept until %s.\n", textsafe.Clean(cap.CaptureID), cap.Turns, cap.ToolCalls, textsafe.Clean(cap.ExpiresAt))
	}

	mode := map[bool]string{false: "new_skill", true: "eval_cases"}[o.into != ""]
	ext := map[string]any{"mode": mode}
	if o.into != "" {
		ext["targetResourceId"] = o.into
		if o.set != "" {
			ext["setId"] = o.set
		}
	}
	extID, err := startExtraction(cmd, c, cap.CaptureID, ext)
	if err != nil {
		return err
	}
	result := map[string]any{"captureId": cap.CaptureID, "extractionId": extID, "reviewUrl": review}
	if !o.wait {
		return finishSession(cmd, o, result, fmt.Sprintf("Drafting started. Review it at %s", review))
	}
	e, err := waitExtraction(cmd.Context(), c, extID, extractionWait)
	if err != nil {
		return err
	}
	result["proposal"] = e.Proposal
	if !o.jsonOut {
		printProposal(out, e)
	}
	if o.accept {
		acceptBody := map[string]any{"proposal": e.Proposal, "mode": mode}
		if o.into != "" {
			acceptBody["targetResourceId"] = o.into
			if o.set != "" {
				acceptBody["setId"] = o.set
			}
		}
		if then != "" {
			acceptBody["then"] = then
		}
		raw, err := gatewayWrite(cmd, c, o.jsonOut, http.MethodPost, sessionAPI+"/extractions/"+url.PathEscape(extID)+"/accept", acceptBody, nil)
		if err != nil {
			return err
		}
		result["accepted"] = json.RawMessage(raw)
		if !o.jsonOut {
			fmt.Fprintln(out, "Accepted. The draft is private to you and not published.")
		}
	}
	return finishSession(cmd, o, result, fmt.Sprintf("Review it at %s", review))
}

func thenValue(s string) (string, error) {
	switch s {
	case "":
		return "", nil
	case "evals":
		return "evals", nil
	case "evals-optimize":
		return "evals_optimize", nil
	}
	return "", fmt.Errorf("invalid --then %q: use evals or evals-optimize", textsafe.Clean(s))
}

// capturePage is the review page of a capture on the gateway.
func capturePage(c *client.Client, id string) string {
	return strings.TrimRight(c.GatewayURL, "/") + "/skills/from-session/" + url.PathEscape(id)
}

func finishSession(cmd *cobra.Command, o fromSessionOpts, result map[string]any, line string) error {
	out := cmd.OutOrStdout()
	if o.jsonOut {
		return json.NewEncoder(out).Encode(result)
	}
	fmt.Fprintln(out, line)
	if o.open {
		if link, _ := result["reviewUrl"].(string); link != "" {
			showLink(cmd, link, false, "Review it there")
		}
	}
	return nil
}

// loadSessionSource resolves the argument into a session read locally, or a
// run or MCP session the gateway reads itself.
func loadSessionSource(cmd *cobra.Command, arg string, o fromSessionOpts) (sessionSource, error) {
	in, errOut := cmd.InOrStdin(), cmd.ErrOrStderr()
	format := ""
	if o.agent != "" && o.agent != "auto" {
		f, ok := transcripts.Sources[o.agent]
		if !ok {
			return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: fmt.Sprintf("unknown --agent %q", textsafe.Clean(o.agent))}
		}
		format = f
	}
	switch {
	case strings.HasPrefix(arg, "crn_"):
		return sessionSource{runID: arg}, nil
	case strings.HasPrefix(arg, "mcp-session:"):
		return sessionSource{sessionRef: strings.TrimPrefix(arg, "mcp-session:")}, nil
	}
	var data []byte
	name := arg
	var err error
	switch {
	case arg == "-":
		data, err = readStdinCapped(in)
	case arg != "":
		if _, statErr := os.Stat(arg); statErr != nil && format == transcripts.SourceOpenCode {
			data, err = transcripts.ExportOpenCode(cmd.Context(), arg)
		} else {
			data, err = transcripts.ReadFile(arg, maxTranscriptBytes)
		}
	case o.last:
		f, ok := newestSession(format)
		if !ok {
			return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: "no sessions were found on this machine"}
		}
		fmt.Fprintf(errOut, "Using the newest session: %s %s, %s\n", f.Source, textsafe.Clean(f.Project), ago(f.Mod))
		data, err = f.Open(maxTranscriptBytes)
		name, format = f.Path(), f.Source
	case o.pick || canPrompt(in):
		var f transcripts.Found
		if f, err = pickSession(in, cmd.OutOrStdout(), format); err != nil {
			return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: err.Error()}
		}
		data, err = f.Open(maxTranscriptBytes)
		name, format = f.Path(), f.Source
	default:
		return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: "name a session file, '-', a run id or mcp-session:<ref>; there is no terminal to choose from"}
	}
	if err != nil {
		return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: fmt.Sprintf("read the session: %v", err)}
	}
	if format == "" {
		if format, err = transcripts.Detect(name, data); err != nil {
			return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: err.Error()}
		}
	}
	conv := o.conversation
	if conv == "" && (format == transcripts.SourceClaudeAI || format == transcripts.SourceChatGPT) && canPrompt(in) {
		if conv, err = chooseConversation(in, cmd.OutOrStdout(), format, data); err != nil {
			return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: err.Error()}
		}
	}
	s, err := transcripts.Read(format, data, conv)
	if err != nil {
		return sessionSource{}, &ExitError{Code: exitSessionUsage, Msg: err.Error()}
	}
	return sessionSource{session: s}, nil
}

func readStdinCapped(in io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(in, maxTranscriptBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTranscriptBytes {
		return nil, fmt.Errorf("the input is larger than %d bytes", maxTranscriptBytes)
	}
	return data, nil
}

// acceptConsent shows the gateway's consent text and asks, unless --consent
// was given; without a terminal it refuses.
func acceptConsent(cmd *cobra.Command, flag bool, stmt consentStatement) error {
	if flag {
		return nil
	}
	if !canPrompt(cmd.InOrStdin()) {
		return &ExitError{Code: exitSessionUsage, Msg: "uploading needs your consent and there is no terminal to ask on; read the statement with a terminal once, then pass --consent"}
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\n%s\n\n", textsafe.Clean(stmt.Text))
	if !askDefaultNo(cmd.InOrStdin(), out, "Do you agree and upload the redacted copy? [y/N] ") {
		return &ExitError{Code: 1, Msg: "cancelled; nothing was uploaded"}
	}
	return nil
}

func printRedactionSummary(w io.Writer, s *transcripts.Session) {
	var parts []string
	counts := s.Redaction.Counts
	classes := make([]string, 0, len(counts))
	for k := range counts {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	for _, k := range classes {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	removed := "nothing needed removing"
	if len(parts) > 0 {
		removed = "removed " + strings.Join(parts, ", ")
	}
	fmt.Fprintf(w, "%s session, %d turns, %d tool calls: %s.\n", textsafe.Clean(s.Source), s.Stats.Turns, s.Stats.ToolCalls, removed)
	shown := 0
	for _, t := range s.Turns {
		if t.Text == "" || shown == 3 {
			continue
		}
		line, _, _ := strings.Cut(t.Text, "\n")
		if r := []rune(line); len(r) > 100 {
			line = string(r[:100]) + "..."
		}
		fmt.Fprintf(w, "  %s: %s\n", t.Role, textsafe.Clean(line))
		shown++
	}
}

func printRedacted(out io.Writer, s *transcripts.Session, jsonOut bool) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if jsonOut {
		return enc.Encode(s)
	}
	printRedactionSummary(out, s)
	fmt.Fprintln(out)
	if err := enc.Encode(s); err != nil {
		return err
	}
	fmt.Fprintln(out, "Nothing was uploaded.")
	return nil
}

func printProposal(out io.Writer, e extractionDoc) {
	p := e.Proposal
	fmt.Fprintf(out, "\nProposal %s\n  %s\n", textsafe.Clean(p.Name), textsafe.Clean(p.Description))
	if len(p.Connectors) > 0 {
		fmt.Fprintf(out, "  connectors: %s\n", textsafe.Clean(strings.Join(p.Connectors, ", ")))
	}
	fmt.Fprintf(out, "  %d eval cases\n", p.caseCount())
	for _, w := range p.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", textsafe.Clean(w))
	}
	if e.InjectionFlag {
		fmt.Fprintln(out, "  warning: the session contains text that looks like instructions to a model; read it before accepting")
	}
}

type captureRow struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt"`
	Skill     *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"skill"`
}

func sessionListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your uploaded sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Captures []captureRow `json:"captures"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, sessionAPI+"/captures", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Captures) == 0 {
				fmt.Fprintln(out, "No uploaded sessions.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "CAPTURE\tSOURCE\tSTATUS\tEXPIRES\tSKILL")
			for _, r := range doc.Captures {
				skill := "-"
				if r.Skill != nil {
					skill = textsafe.Clean(r.Skill.Name)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(r.ID), textsafe.Clean(r.Source), textsafe.Clean(r.Status), textsafe.Clean(r.ExpiresAt), skill)
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func sessionShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <capture>",
		Short: "Show an uploaded session's redaction report and its review page",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if config.AgentMode() {
				return &ExitError{Code: exitDenied, Msg: "an uploaded session holds your conversation and is shown to a person only; it is refused while ZUANO_AGENT is set"}
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Capture struct {
					captureRow
					Turns     int `json:"turns"`
					ToolCalls int `json:"toolCalls"`
					Redaction struct {
						Counts map[string]int `json:"counts"`
					} `json:"redaction"`
				} `json:"capture"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, sessionAPI+"/captures/"+url.PathEscape(args[0]), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			k := doc.Capture
			fmt.Fprintf(out, "%s (%s, %s): %d turns, %d tool calls, expires %s\n", textsafe.Clean(k.ID), textsafe.Clean(k.Source), textsafe.Clean(k.Status), k.Turns, k.ToolCalls, textsafe.Clean(k.ExpiresAt))
			classes := make([]string, 0, len(k.Redaction.Counts))
			for c, n := range k.Redaction.Counts {
				if n > 0 {
					classes = append(classes, fmt.Sprintf("%s %d", c, n))
				}
			}
			sort.Strings(classes)
			if len(classes) > 0 {
				fmt.Fprintf(out, "  removed: %s\n", strings.Join(classes, ", "))
			}
			fmt.Fprintf(out, "  review: %s\n", capturePage(c, k.ID))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func sessionDiscardCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "discard <capture>",
		Short: "Delete an uploaded session now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete %s and anything drafted from it that you have not accepted?", textsafe.Clean(args[0]))); err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodDelete, sessionAPI+"/captures/"+url.PathEscape(args[0]), nil, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s.\n", textsafe.Clean(args[0]))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without asking")
	return cmd
}

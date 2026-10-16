package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/policyfile"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const maxDLPInput = 4 << 20

// DLPCmd returns `zuano dlp`, the client for data loss prevention.
func DLPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dlp",
		Short: "See and change what the gateway does with sensitive data in tool calls",
		Long: `Data loss prevention finds secrets and personal data in what goes to and comes
back from tools, and blocks, masks, tokenizes or holds it for approval by rule.
These commands read and replace the policy, test a sample, list findings and
mark false positives. A finding never holds the value, only its class and a
count; 'test' prints masked previews.

Changing the policy is for a person: 'policy apply' is refused while ZUANO_AGENT
is set (exit 10), and a loosening change needs a passkey confirmation. A call
that DLP blocks ends with exit 10, one it holds for approval with exit 12.`,
	}
	cmd.AddCommand(dlpPolicyCmd(), dlpTestCmd(), dlpFindingsCmd(), dlpStatsCmd(), dlpFeedbackCmd(), dlpHashCmd())
	return cmd
}

// readInput reads what was piped in. A terminal is refused: the value would
// be typed into shell history or left on screen.
func readInput(cmd *cobra.Command) ([]byte, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && isInteractiveTerminal(f) {
		return nil, errors.New("this reads standard input; pipe or redirect a file into it")
	}
	data, err := io.ReadAll(io.LimitReader(in, maxDLPInput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDLPInput {
		return nil, fmt.Errorf("the input is larger than %d bytes", maxDLPInput)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("the input is empty")
	}
	return data, nil
}

// parseSince turns 24h or 7d into a start time.
func parseSince(s string) (time.Time, error) {
	d, err := parseDays(s)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("invalid --since %q: use a duration such as 24h or 7d", textsafe.Clean(s))
	}
	return time.Now().UTC().Add(-d), nil
}

func parseDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func dlpPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "policy", Short: "Read, check and replace the DLP policy"}
	cmd.AddCommand(dlpPolicyGetCmd(), dlpPolicyApplyCmd(), dlpPolicyVersionsCmd())
	return cmd
}

func dlpPolicyGetCmd() *cobra.Command {
	var f approvalFlags
	var asYAML bool
	cmd := &cobra.Command{
		Use:   "get [--yaml]",
		Short: "Show the active DLP policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if asYAML {
				r, err := gatewayExchange(cmd.Context(), c, http.MethodGet, "/api/admin/dlp/policy?format=yaml", nil, nil)
				if err != nil {
					return err
				}
				if !r.ok() {
					return r.failure()
				}
				_, err = cmd.OutOrStdout().Write(r.Body)
				return err
			}
			var doc struct {
				Policy struct {
					Version int `json:"version"`
					Spec    struct {
						Mode  string            `json:"mode"`
						Rules []json.RawMessage `json:"rules"`
					} `json:"spec"`
				} `json:"policy"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/admin/dlp/policy", nil, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			p := doc.Policy
			fmt.Fprintf(cmd.OutOrStdout(), "DLP policy version %d: mode %s, %d rule(s). Print it as a file with `%s`.\n", p.Version, orDash(textsafe.Clean(p.Spec.Mode)), len(p.Spec.Rules), brand.Cmd("dlp policy get --yaml"))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&asYAML, "yaml", false, "Print the policy as the YAML document you can apply")
	return cmd
}

func dlpPolicyApplyCmd() *cobra.Command {
	var f approvalFlags
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "apply <file> [--dry-run]",
		Short: "Check a DLP policy file, show its impact, and replace the active policy",
		Long: `--dry-run sends the file for checking only and prints the difference from the
active policy and what the new rules would have done to the last days of real
traffic. Without it the file becomes the active policy as a new version; in
strict mode, or when it loosens anything, that needs a passkey confirmation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				if err := refuseAgentChange(brand.Cmd("dlp policy apply")); err != nil {
					return err
				}
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read %s: %w", textsafe.Clean(args[0]), err)
			}
			if len(data) > policyfile.MaxDocumentBytes {
				return fmt.Errorf("%s is larger than %d bytes", textsafe.Clean(args[0]), policyfile.MaxDocumentBytes)
			}
			contentType := "application/yaml"
			if strings.EqualFold(filepath.Ext(args[0]), ".json") {
				contentType = "application/json"
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := "/api/admin/dlp/policy"
			if dryRun {
				path += "?dryRun=true"
			}
			r, err := gatewayExchangeRaw(cmd.Context(), c, http.MethodPut, path, contentType, data, nil)
			if err != nil {
				return err
			}
			if !r.ok() {
				return policyFailure(cmd.ErrOrStderr(), r)
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, r.Body)
				return nil
			}
			var res struct {
				Version int `json:"version"`
				Diff    struct {
					Changes []policyfile.Change `json:"changes"`
				} `json:"diff"`
				Impact struct {
					Days          int `json:"days"`
					WouldBlock    int `json:"wouldBlock"`
					WouldHold     int `json:"wouldHold"`
					WouldMask     int `json:"wouldMask"`
					WouldTokenize int `json:"wouldTokenize"`
					ByRule        []struct {
						Rule  string `json:"rule"`
						Count int    `json:"count"`
					} `json:"byRule"`
				} `json:"impact"`
			}
			_ = json.Unmarshal(r.Body, &res)
			if dryRun {
				printChanges(out, res.Diff.Changes)
				im := res.Impact
				fmt.Fprintf(out, "Over the last %d day(s) the new rules would have blocked %d, held %d, masked %d and tokenized %d call(s).\n", im.Days, im.WouldBlock, im.WouldHold, im.WouldMask, im.WouldTokenize)
				for _, br := range im.ByRule {
					fmt.Fprintf(out, "  %s: %d\n", textsafe.Clean(br.Rule), br.Count)
				}
				fmt.Fprintln(out, "[dry-run] nothing was changed")
				return nil
			}
			fmt.Fprintf(out, "The DLP policy is now version %d.\n", res.Version)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only check and show the impact; change nothing")
	return cmd
}

func dlpPolicyVersionsCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "versions",
		Short: "List the DLP policy versions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Versions []struct {
					Version   int    `json:"version"`
					CreatedAt string `json:"createdAt"`
					Actor     string `json:"actor"`
					Mode      string `json:"mode"`
					Source    string `json:"source"`
				} `json:"versions"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/admin/dlp/policy/versions", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tAT\tBY\tMODE\tSOURCE")
			for _, v := range doc.Versions {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", v.Version, textsafe.Clean(v.CreatedAt), orDash(textsafe.Clean(v.Actor)), orDash(textsafe.Clean(v.Mode)), orDash(textsafe.Clean(v.Source)))
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func dlpTestCmd() *cobra.Command {
	var f approvalFlags
	var service, tool, direction string
	cmd := &cobra.Command{
		Use:   "test [--service S --tool T] [--direction in|out] < file",
		Short: "Show what DLP would find and do for a sample read from standard input",
		Long: `Sends the sample to the gateway for one check and prints the classes found
with masked previews and the decision. The gateway neither logs nor stores it.
--direction out is data going to a tool, in is a tool's result coming back.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := map[string]string{"in": "inbound", "out": "outbound"}[direction]
			if dir == "" {
				return fmt.Errorf("invalid --direction %q: use in or out", textsafe.Clean(direction))
			}
			data, err := readInput(cmd)
			if err != nil {
				return err
			}
			body := map[string]any{"direction": dir}
			var parsed any
			if json.Unmarshal(data, &parsed) == nil {
				switch parsed.(type) {
				case map[string]any, []any:
					body["json"] = parsed
				}
			}
			if body["json"] == nil {
				body["text"] = string(data)
			}
			if service != "" {
				body["service"] = service
			}
			if tool != "" {
				body["tool"] = tool
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Findings []struct {
					Class      string `json:"class"`
					Detector   string `json:"detector"`
					Confidence string `json:"confidence"`
					Count      int    `json:"count"`
					Masked     string `json:"masked"`
				} `json:"findings"`
				Decision struct {
					Action string `json:"action"`
					Rule   string `json:"rule"`
				} `json:"decision"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodPost, "/api/admin/dlp/test", body, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(res.Findings) == 0 {
				fmt.Fprintln(out, "Nothing sensitive found.")
			}
			for _, fd := range res.Findings {
				fmt.Fprintf(out, "  %-24s %-10s x%d  %s\n", textsafe.Clean(fd.Class), orDash(textsafe.Clean(fd.Confidence)), fd.Count, textsafe.Clean(fd.Masked))
			}
			rule := ""
			if res.Decision.Rule != "" {
				rule = " by rule " + textsafe.Clean(res.Decision.Rule)
			}
			fmt.Fprintf(out, "Decision: %s%s\n", orDash(textsafe.Clean(res.Decision.Action)), rule)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	fl := cmd.Flags()
	fl.StringVar(&service, "service", "", "The tool's service")
	fl.StringVar(&tool, "tool", "", "The tool")
	fl.StringVar(&direction, "direction", "out", "out (to the tool) or in (from the tool)")
	return cmd
}

func dlpFindingsCmd() *cobra.Command {
	var f approvalFlags
	var since, class, service string
	cmd := &cobra.Command{
		Use:   "findings [--since 24h] [--class 'pii.*'] [--service S]",
		Short: "List what DLP found, newest first, without the values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, err := parseSince(since)
			if err != nil {
				return err
			}
			q := url.Values{"from": {from.Format(time.RFC3339)}}
			if class != "" {
				q.Set("class", class)
			}
			if service != "" {
				q.Set("service", service)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Findings []struct {
					ID       string `json:"id"`
					At       string `json:"at"`
					Class    string `json:"class"`
					Detector string `json:"detector"`
					Service  string `json:"service"`
					Tool     string `json:"tool"`
					Outcome  string `json:"outcome"`
					Enforced bool   `json:"enforced"`
					Count    int    `json:"count"`
				} `json:"findings"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/admin/dlp/findings?"+q.Encode(), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Findings) == 0 {
				fmt.Fprintln(out, "No findings.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAT\tCLASS\tTOOL\tOUTCOME\tCOUNT")
			for _, x := range doc.Findings {
				outcome := textsafe.Clean(x.Outcome)
				if !x.Enforced {
					outcome += " (monitor)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s.%s\t%s\t%d\n", textsafe.Clean(x.ID), textsafe.Clean(x.At), textsafe.Clean(x.Class), textsafe.Clean(x.Service), textsafe.Clean(x.Tool), outcome, x.Count)
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	fl := cmd.Flags()
	fl.StringVar(&since, "since", "24h", "How far back: 24h, 7d")
	fl.StringVar(&class, "class", "", "Only this class, such as pii.* or secret.credential")
	fl.StringVar(&service, "service", "", "Only this service")
	return cmd
}

func dlpStatsCmd() *cobra.Command {
	var f approvalFlags
	var since, groupBy string
	cmd := &cobra.Command{
		Use:   "stats [--since 7d] [--group-by class|service|tool|agent|rule]",
		Short: "Count findings by class, service, tool, agent or rule",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch groupBy {
			case "class", "service", "tool", "agent", "rule":
			default:
				return fmt.Errorf("invalid --group-by %q: use class, service, tool, agent or rule", textsafe.Clean(groupBy))
			}
			from, err := parseSince(since)
			if err != nil {
				return err
			}
			q := url.Values{"from": {from.Format(time.RFC3339)}, "groupBy": {groupBy}}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Stats []struct {
					Key   string `json:"key"`
					Count int    `json:"count"`
				} `json:"stats"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/admin/dlp/stats?"+q.Encode(), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			for _, s := range doc.Stats {
				fmt.Fprintf(out, "  %-32s %d\n", textsafe.Clean(s.Key), s.Count)
			}
			if len(doc.Stats) == 0 {
				fmt.Fprintln(out, "No findings.")
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&since, "since", "7d", "How far back: 24h, 7d")
	cmd.Flags().StringVar(&groupBy, "group-by", "class", "class, service, tool, agent or rule")
	return cmd
}

func dlpFeedbackCmd() *cobra.Command {
	var f approvalFlags
	var falsePositive, truePositive bool
	var scope string
	cmd := &cobra.Command{
		Use:   "feedback <finding-id> --false-positive|--true-positive [--scope value|tool|path]",
		Short: "Say whether a finding was right",
		Long: `A false positive can stop the same value (--scope value), tool or argument path
from being flagged again, once an administrator accepts it. The finding holds no
value; the gateway matches it by a keyed hash.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if falsePositive == truePositive {
				return errors.New("give exactly one of --false-positive or --true-positive")
			}
			switch scope {
			case "value", "tool", "path":
			default:
				return fmt.Errorf("invalid --scope %q: use value, tool or path", textsafe.Clean(scope))
			}
			verdict := "true_positive"
			if falsePositive {
				verdict = "false_positive"
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/dlp/findings/"+url.PathEscape(args[0])+"/feedback", map[string]any{"verdict": verdict, "scope": scope}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s for %s.\n", strings.ReplaceAll(verdict, "_", " "), textsafe.Clean(args[0]))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&falsePositive, "false-positive", false, "The finding was not sensitive")
	cmd.Flags().BoolVar(&truePositive, "true-positive", false, "The finding was right")
	cmd.Flags().StringVar(&scope, "scope", "value", "What a false positive covers: value, tool or path")
	return cmd
}

func dlpHashCmd() *cobra.Command {
	var f approvalFlags
	var detector string
	cmd := &cobra.Command{
		Use:   "hash --detector iban",
		Short: "Print the key to put in a DLP policy for a known value",
		Long: `Reads the value from standard input (or a hidden prompt on a terminal), so it
never appears in shell history, and asks the gateway for its keyed hash. The
value is not stored. Put the key in 'allowValues' to let that value through.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if detector == "" {
				return errors.New("--detector is required")
			}
			value, err := readHashValue(cmd)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Key string `json:"key"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodPost, "/api/admin/dlp/hash", map[string]any{"detector": detector, "value": value}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), textsafe.Clean(res.Key))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&detector, "detector", "", "The detector the value belongs to, such as iban")
	return cmd
}

func readHashValue(cmd *cobra.Command) (string, error) {
	if f, ok := cmd.InOrStdin().(*os.File); ok && isInteractiveTerminal(f) {
		fmt.Fprint(cmd.ErrOrStderr(), "Value (hidden): ")
		v, err := readSecret(f)
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(v), nil
	}
	data, err := readInput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

type dataProfile struct {
	Service   string `json:"service"`
	Category  string `json:"category"`
	Residency string `json:"residency"`
	Basis     string `json:"basis"`
}

func connectorsProfileCmd() *cobra.Command {
	var f approvalFlags
	var residency, basis string
	cmd := &cobra.Command{
		Use:   "profile <slug> [--set-residency eu --basis org_verified]",
		Short: "Show or set where a connector's data is kept",
		Long: `The data profile says what kind of data a connector carries and where it is
kept, which DLP rules use ("customer data stays in the EU"). Setting the
residency is an administrator's decision and says on what basis it is known.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			set := cmd.Flags().Changed("set-residency")
			if set {
				if err := refuseAgentChange(brand.Cmd("connectors profile --set-residency")); err != nil {
					return err
				}
				if basis == "" {
					return errors.New("--basis is required with --set-residency")
				}
				_, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPut, "/api/admin/connectors/"+url.PathEscape(args[0])+"/data-profile", map[string]any{"residency": residency, "basis": basis}, nil)
				if err != nil {
					return err
				}
			}
			var doc struct {
				Profiles []dataProfile `json:"profiles"`
			}
			if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/admin/connectors/data-profiles", nil, &doc); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, p := range doc.Profiles {
				if p.Service != args[0] {
					continue
				}
				if f.jsonOut {
					return json.NewEncoder(out).Encode(p)
				}
				fmt.Fprintf(out, "%s: %s data, residency %s (%s)\n", textsafe.Clean(p.Service), orDash(textsafe.Clean(p.Category)), orDash(textsafe.Clean(p.Residency)), orDash(textsafe.Clean(p.Basis)))
				return nil
			}
			return fmt.Errorf("no data profile for %q; see `%s`", textsafe.Clean(args[0]), brand.Cmd("connectors"))
		},
	}
	f.bind(cmd, "Print the profile as JSON")
	cmd.Flags().StringVar(&residency, "set-residency", "", "Where the data is kept, such as eu, eea or us")
	cmd.Flags().StringVar(&basis, "basis", "", "How that is known: org_verified, vendor_documented, or contract")
	return cmd
}

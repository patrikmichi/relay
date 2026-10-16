package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

var (
	dayPattern   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	packPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,40}$`)
	exportWait   = 30 * time.Minute
	exportPoll   = 3 * time.Second
	downloadHTTP = func() *http.Client {
		return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("refusing to follow a redirect") }}
	}
)

// ComplianceCmd returns `zuano compliance`, the client for compliance packs.
func ComplianceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compliance",
		Short: "Export evidence for SOC 2, ISO 27001, GDPR and other frameworks, and verify it",
		Long: `A compliance pack maps a framework's controls to evidence the gateway already
holds: audit records, access, policies, connections, registers you keep for
incidents, data subject requests and access reviews. An export is a signed
bundle for a date range (the end day is not included) that an auditor can check
without trusting the gateway: 'verify' recomputes the audit hash chain and
checks the signatures offline.

A pack shows what the gateway can evidence and where a person must supply it. It
is not legal advice and does not certify compliance.

Changes (exports, schedules, shares, registers, deleting) are for a person:
refused while ZUANO_AGENT is set (exit 10). Auditors can use the read commands.

export --wait exit status
  0   the bundle is ready (and downloaded with --out)
  17  still running when --wait-timeout ended
  20  the export failed
  30-33  the plan does not allow it

verify exit status
  0   the bundle is valid
  45  verification failed; each file or event that does not check out is named
  46  the public keys could not be obtained`,
	}
	cmd.AddCommand(compliancePacksCmd(), complianceControlsCmd(), complianceExportCmd(), complianceExportsCmd(), complianceSchedulesCmd(),
		complianceShareCmd(), complianceVerifyCmd(), complianceIncidentsCmd(), complianceDSRCmd(), complianceAccessReviewCmd())
	return cmd
}

func checkDay(name, v string) error {
	if v == "" || dayPattern.MatchString(v) {
		return nil
	}
	return fmt.Errorf("invalid --%s %q: use YYYY-MM-DD", name, textsafe.Clean(v))
}

func rangeQuery(from, to string) func(*cobra.Command) func() string {
	return func(cmd *cobra.Command) func() string {
		cmd.Flags().StringVar(&from, "from", "", "First day, YYYY-MM-DD")
		cmd.Flags().StringVar(&to, "to", "", "Day to stop before, YYYY-MM-DD")
		return func() string {
			q := url.Values{}
			if from != "" {
				q.Set("from", from)
			}
			if to != "" {
				q.Set("to", to)
			}
			if len(q) == 0 {
				return ""
			}
			return "?" + q.Encode()
		}
	}
}

func compliancePacksCmd() *cobra.Command {
	var from, to string
	return listCommand("packs", "List the compliance packs and how much of each the gateway can evidence",
		"/api/compliance/packs", "packs", "No compliance packs.",
		[]column{{"PACK", "id"}, {"NAME", "name"}, {"VERSION", "version"}, {"COVERED", "coverage.covered"}, {"PARTIAL", "coverage.partial"}, {"GAPS", "coverage.gaps"}, {"TOTAL", "coverage.total"}},
		rangeQuery(from, to))
}

func complianceControlsCmd() *cobra.Command {
	var f approvalFlags
	var pack, from, to string
	var gapsOnly bool
	cmd := &cobra.Command{
		Use:   "controls --pack soc2 [--from D --to D] [--gaps]",
		Short: "List a pack's controls with their status and the evidence behind each",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !packPattern.MatchString(pack) {
				return errors.New("--pack is required, such as soc2")
			}
			if err := errors.Join(checkDay("from", from), checkDay("to", to)); err != nil {
				return err
			}
			q := url.Values{}
			if from != "" {
				q.Set("from", from)
			}
			if to != "" {
				q.Set("to", to)
			}
			path := "/api/compliance/packs/" + url.PathEscape(pack) + "/controls"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Controls []map[string]any `json:"controls"`
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
			rows := doc.Controls[:0]
			for _, r := range doc.Controls {
				if !gapsOnly || fieldText(r, "status") == "gap" || fieldText(r, "status") == "partial" || fieldText(r, "status") == "manual" {
					rows = append(rows, r)
				}
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "No controls to show.")
				return nil
			}
			printTable(out, rows, []column{{"CONTROL", "id"}, {"TITLE", "title"}, {"STATUS", "status"}, {"SOURCES", "sources"}, {"RECORDS", "records"}, {"NOTE", "gapNote"}})
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	fl := cmd.Flags()
	fl.StringVar(&pack, "pack", "", "The pack, such as soc2")
	fl.StringVar(&from, "from", "", "First day, YYYY-MM-DD")
	fl.StringVar(&to, "to", "", "Day to stop before, YYYY-MM-DD")
	fl.BoolVar(&gapsOnly, "gaps", false, "Only controls that are not fully covered")
	return cmd
}

func complianceExportCmd() *cobra.Command {
	var f approvalFlags
	var pack, from, to, outFile string
	var wait bool
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "export --pack soc2 --from D --to D [--wait] [--out bundle.zip]",
		Short: "Create a signed evidence bundle for a date range",
		Long: `Starts the export as a background job. With --wait, follows it and, with --out,
downloads the bundle when it is ready. The range includes --from and stops before
--to. Check the bundle with 'zuano compliance verify'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance export")); err != nil {
				return err
			}
			if !packPattern.MatchString(pack) || !dayPattern.MatchString(from) || !dayPattern.MatchString(to) {
				return errors.New("--pack, --from and --to (YYYY-MM-DD) are required")
			}
			if from >= to {
				return errors.New("--from must be before --to")
			}
			if outFile != "" && !wait {
				return errors.New("--out needs --wait, since the bundle is not ready until the job finishes")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var started struct {
				ExportID string `json:"export_id"`
				JobID    string `json:"job_id"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/compliance/exports", map[string]any{"pack": pack, "from": from, "to": to}, &started)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if started.ExportID == "" || started.JobID == "" {
				return errors.New("the gateway did not say which export started")
			}
			if !wait {
				if f.jsonOut {
					printRaw(out, raw)
					return nil
				}
				fmt.Fprintf(out, "Export %s started (%s). Follow it with `%s`.\n", textsafe.Clean(started.ExportID), textsafe.Clean(started.JobID), brand.Cmd("compliance exports show "+started.ExportID))
				return nil
			}
			if err := waitExportJob(cmd, c, started.JobID, waitTimeout); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(out, "Export %s is ready.\n", textsafe.Clean(started.ExportID))
			}
			if outFile == "" {
				return nil
			}
			path, err := downloadExport(cmd, c, started.ExportID, outFile)
			if err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(out, "Wrote %s. Check it with `%s`.\n", textsafe.Clean(path), brand.Cmd("compliance verify "+path))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	fl := cmd.Flags()
	fl.StringVar(&pack, "pack", "", "The pack, such as soc2")
	fl.StringVar(&from, "from", "", "First day, YYYY-MM-DD")
	fl.StringVar(&to, "to", "", "Day to stop before, YYYY-MM-DD")
	fl.BoolVar(&wait, "wait", false, "Follow the job until the bundle is ready")
	fl.DurationVar(&waitTimeout, "wait-timeout", exportWait, "How long --wait follows the job")
	fl.StringVar(&outFile, "out", "", "With --wait: download the bundle to this file")
	return cmd
}

// waitExportJob follows a job to its end: exit 17 when time runs out, 20 when
// the export fails.
func waitExportJob(cmd *cobra.Command, c *client.Client, jobID string, total time.Duration) error {
	deadline := time.Now().Add(total)
	for {
		raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, jobPath(jobID), nil, nil)
		if err != nil {
			return err
		}
		doc, err := jobStatusOf(raw)
		if err != nil {
			return err
		}
		switch doc.Status {
		case "succeeded":
			return nil
		case "failed", "dead", "cancelled":
			msg := "the export " + doc.Status
			if e := doc.errorText(); e != "" {
				msg += ": " + textsafe.Clean(e)
			}
			return &ExitError{Code: exitRunFailed, Msg: msg}
		}
		if time.Now().After(deadline) {
			return &ExitError{Code: exitWaitTimeout, Msg: fmt.Sprintf("the export is still running after %s; follow it with `%s`", total, brand.Cmd("compliance exports list"))}
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(exportPoll):
		}
	}
}

// downloadExport fetches a ready bundle from its short-lived signed address.
// The address gets no credentials and must be https, or on the gateway itself.
func downloadExport(cmd *cobra.Command, c *client.Client, id, outFile string) (string, error) {
	var dl struct {
		URL      string `json:"url"`
		FileName string `json:"fileName"`
		SHA256   string `json:"sha256"`
	}
	if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/compliance/exports/"+url.PathEscape(id)+"/download", nil, &dl); err != nil {
		return "", err
	}
	u, err := url.Parse(dl.URL)
	base, berr := url.Parse(c.GatewayURL)
	if err != nil || berr != nil || (u.Scheme != "https" && (u.Scheme != base.Scheme || u.Host != base.Host)) {
		return "", errors.New("the gateway gave a download address that is neither https nor on the gateway; refusing to fetch it")
	}
	if outFile == "" || strings.HasSuffix(outFile, string(filepath.Separator)) {
		name := filepath.Base(dl.FileName)
		if name == "." || name == string(filepath.Separator) {
			name = id + ".zip"
		}
		outFile = filepath.Join(outFile, name)
	}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, dl.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := downloadHTTP().Do(req)
	if err != nil {
		return "", fmt.Errorf("download the bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download the bundle: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(outFile), ".bundle-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("download the bundle: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if dl.SHA256 != "" && !strings.EqualFold(dl.SHA256, hex.EncodeToString(h.Sum(nil))) {
		return "", errors.New("the downloaded bundle does not match the hash the gateway gave; it was discarded")
	}
	if err := os.Rename(tmp.Name(), outFile); err != nil {
		return "", err
	}
	return outFile, nil
}

func complianceExportsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "exports", Short: "List, show and delete exports"}
	list := listCommand("list", "List exports, newest first", "/api/compliance/exports", "exports", "No exports.",
		[]column{{"EXPORT", "id"}, {"PACK", "pack"}, {"FROM", "from"}, {"TO", "to"}, {"STATUS", "status"}, {"EXPIRES", "expiresAt"}}, nil)
	show := showCommand("show <id>", "Show one export", "/api/compliance/exports", "export")
	cmd.AddCommand(list, show, complianceDeleteCmd("delete <id>", "Delete an export and its bundle now", "/api/compliance/exports"))
	return cmd
}

// complianceDeleteCmd builds a confirmed DELETE of path/<arg>.
func complianceDeleteCmd(use, short, path string) *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance " + strings.Fields(use)[0])); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete %s?", textsafe.Clean(args[0]))); err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodDelete, path+"/"+url.PathEscape(args[0]), nil, nil)
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

func complianceSchedulesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "schedules", Short: "Export on a schedule"}
	list := listCommand("list", "List scheduled exports", "/api/compliance/schedules", "schedules", "No schedules.",
		[]column{{"SCHEDULE", "id"}, {"PACK", "pack"}, {"FREQUENCY", "frequency"}, {"RANGE DAYS", "rangeDays"}, {"SHARED WITH", "recipients"}}, nil)
	var f approvalFlags
	var pack, frequency string
	var rangeDays int
	var recipients []string
	create := &cobra.Command{
		Use:   "create --pack soc2 --frequency monthly [--range-days 90] [--to auditor@firm.example]",
		Short: "Create a scheduled export, optionally shared with an auditor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance schedules create")); err != nil {
				return err
			}
			if !packPattern.MatchString(pack) {
				return errors.New("--pack is required, such as soc2")
			}
			switch frequency {
			case "weekly", "monthly", "quarterly":
			default:
				return fmt.Errorf("invalid --frequency %q: use weekly, monthly or quarterly", textsafe.Clean(frequency))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Schedule struct {
					ID string `json:"id"`
				} `json:"schedule"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/compliance/schedules", map[string]any{"pack": pack, "frequency": frequency, "rangeDays": rangeDays, "recipients": recipients}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created schedule %s.\n", textsafe.Clean(res.Schedule.ID))
			return nil
		},
	}
	f.bind(create, "Print the response body unchanged")
	create.Flags().StringVar(&pack, "pack", "", "The pack, such as soc2")
	create.Flags().StringVar(&frequency, "frequency", "monthly", "weekly, monthly or quarterly")
	create.Flags().IntVar(&rangeDays, "range-days", 90, "Days each export covers")
	create.Flags().StringArrayVar(&recipients, "to", nil, "An auditor to share each export with, by email; repeatable")
	cmd.AddCommand(list, create, complianceDeleteCmd("delete <id>", "Delete a schedule", "/api/compliance/schedules"))
	return cmd
}

func complianceShareCmd() *cobra.Command {
	var f approvalFlags
	var to, expires string
	cmd := &cobra.Command{
		Use:   "share <export-id> --to auditor@firm.example [--expires 14d]",
		Short: "Give an auditor a link to download an export",
		Long: `The link needs the auditor to receive a code at the email address you name, and
stops working after --expires. Anyone holding the link and the code can download
the bundle, so send it only to that person.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance share")); err != nil {
				return err
			}
			if !strings.Contains(to, "@") {
				return errors.New("--to must be an email address")
			}
			d, err := parseDays(expires)
			if err != nil || d < 24*time.Hour || d > 90*24*time.Hour {
				return fmt.Errorf("invalid --expires %q: use 1d to 90d", textsafe.Clean(expires))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Share struct {
					URL       string `json:"url"`
					ExpiresAt string `json:"expiresAt"`
				} `json:"share"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/compliance/exports/"+url.PathEscape(args[0])+"/shares", map[string]any{"to": to, "expiresInDays": int(d.Hours() / 24)}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Share link for %s, valid until %s:\n  %s\n", textsafe.Clean(to), textsafe.Clean(res.Share.ExpiresAt), textsafe.Clean(res.Share.URL))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&to, "to", "", "The auditor's email address")
	cmd.Flags().StringVar(&expires, "expires", "14d", "How long the link works, 1d to 90d")
	return cmd
}

func complianceIncidentsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "incidents", Short: "The incident register"}
	list := listCommand("list", "List incidents", "/api/compliance/incidents", "incidents", "No incidents.",
		[]column{{"INCIDENT", "id"}, {"TITLE", "title"}, {"SEVERITY", "severity"}, {"STATUS", "status"}, {"DETECTED", "detectedAt"}}, nil)
	show := showCommand("show <id>", "Show one incident", "/api/compliance/incidents", "incident")
	var f approvalFlags
	var title, severity, detectedAt, description string
	create := &cobra.Command{
		Use:   "create --title T --severity high [--detected-at TIME] [--description TEXT|-]",
		Short: "Record an incident",
		Long:  `--description - reads the description from standard input. It is stored sealed.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance incidents create")); err != nil {
				return err
			}
			switch severity {
			case "low", "medium", "high", "critical":
			default:
				return fmt.Errorf("invalid --severity %q: use low, medium, high or critical", textsafe.Clean(severity))
			}
			if strings.TrimSpace(title) == "" {
				return errors.New("--title is required")
			}
			if detectedAt == "" {
				detectedAt = time.Now().UTC().Format(time.RFC3339)
			} else if _, err := time.Parse(time.RFC3339, detectedAt); err != nil {
				return fmt.Errorf("invalid --detected-at %q: use RFC 3339, such as 2026-10-15T09:30:00Z", textsafe.Clean(detectedAt))
			}
			if description == "-" {
				data, err := readInput(cmd)
				if err != nil {
					return err
				}
				description = string(data)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Incident struct {
					ID string `json:"id"`
				} `json:"incident"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/compliance/incidents", map[string]any{"title": title, "severity": severity, "detectedAt": detectedAt, "description": description}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded incident %s.\n", textsafe.Clean(res.Incident.ID))
			return nil
		},
	}
	f.bind(create, "Print the response body unchanged")
	create.Flags().StringVar(&title, "title", "", "A short title")
	create.Flags().StringVar(&severity, "severity", "medium", "low, medium, high or critical")
	create.Flags().StringVar(&detectedAt, "detected-at", "", "When it was detected, RFC 3339 (default now)")
	create.Flags().StringVar(&description, "description", "", "What happened, or - for standard input")
	cmd.AddCommand(list, show, create)
	return cmd
}

func complianceDSRCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dsr", Short: "The register of data subject requests"}
	list := listCommand("list", "List requests", "/api/compliance/dsr", "requests", "No requests.",
		[]column{{"REQUEST", "id"}, {"KIND", "kind"}, {"SUBJECT", "subject"}, {"STATUS", "status"}, {"RECEIVED", "receivedAt"}, {"DUE", "dueAt"}}, nil)
	var f approvalFlags
	var kind, subject string
	create := &cobra.Command{
		Use:   "create --kind access|erasure|rectification|portability --subject EMAIL",
		Short: "Record a data subject request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance dsr create")); err != nil {
				return err
			}
			switch kind {
			case "access", "erasure", "rectification", "portability":
			default:
				return fmt.Errorf("invalid --kind %q: use access, erasure, rectification or portability", textsafe.Clean(kind))
			}
			if !strings.Contains(subject, "@") {
				return errors.New("--subject must be the person's email address")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Request struct {
					ID string `json:"id"`
				} `json:"request"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/compliance/dsr", map[string]any{"kind": kind, "subject": subject}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded request %s.\n", textsafe.Clean(res.Request.ID))
			return nil
		},
	}
	f.bind(create, "Print the response body unchanged")
	create.Flags().StringVar(&kind, "kind", "", "access, erasure, rectification or portability")
	create.Flags().StringVar(&subject, "subject", "", "The person's email address")
	cmd.AddCommand(list, create, complianceActionCmd("execute <id>", "Carry out a request: export the person's data or erase it (needs a passkey confirmation)", "/api/compliance/dsr", "execute", "This carries out the request now; an erasure cannot be undone."))
	return cmd
}

// complianceActionCmd builds a confirmed POST of path/<arg>/<action>.
func complianceActionCmd(use, short, path, action, warning string) *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance " + strings.Fields(use)[0])); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("%s %s? %s", strings.Fields(use)[0], textsafe.Clean(args[0]), warning)); err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, path+"/"+url.PathEscape(args[0])+"/"+action, map[string]any{}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s done.\n", textsafe.Clean(args[0]), action)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Do it without asking")
	return cmd
}

func complianceAccessReviewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "access-review", Short: "Run an access review campaign for evidence"}
	var f approvalFlags
	var scope string
	var dueDays int
	start := &cobra.Command{
		Use:   "start [--scope all|admins|agents|group:<id>] [--due-days 14]",
		Short: "Start a campaign: each reviewer keeps, revokes or changes the access they own",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("compliance access-review start")); err != nil {
				return err
			}
			if scope != "all" && scope != "admins" && scope != "agents" && !strings.HasPrefix(scope, "group:") {
				return fmt.Errorf("invalid --scope %q: use all, admins, agents or group:<id>", textsafe.Clean(scope))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Review struct {
					ID string `json:"id"`
				} `json:"review"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/compliance/access-reviews", map[string]any{"scope": scope, "dueDays": dueDays}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Started review %s. Follow it with `%s`.\n", textsafe.Clean(res.Review.ID), brand.Cmd("compliance access-review status "+res.Review.ID))
			return nil
		},
	}
	f.bind(start, "Print the response body unchanged")
	start.Flags().StringVar(&scope, "scope", "all", "Who is reviewed: all, admins, agents or group:<id>")
	start.Flags().IntVar(&dueDays, "due-days", 14, "Days reviewers have")
	status := showCommand("status <id>", "Show a campaign's progress", "/api/compliance/access-reviews", "review")
	apply := complianceActionCmd("apply <id>", "Carry out the revocations the reviewers decided (needs a passkey confirmation)", "/api/compliance/access-reviews", "apply", "Roles, grants and keys the reviewers revoked are removed now.")
	cmd.AddCommand(start, status, apply)
	return cmd
}

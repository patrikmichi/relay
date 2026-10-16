package cli

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
)

var auditFormats = []string{"jsonl", "csv"}

// auditPageSize is the largest page the gateway serves.
const auditPageSize = 200

// auditCSVColumns are the event fields a CSV export carries, in order.
var auditCSVColumns = []string{"ts", "event", "user_email", "service", "tool", "outcome", "risk", "duration_ms", "approval_id", "request_id", "trace_id"}

// auditPage mirrors GET /api/org/{org}/observability/audit.
type auditPage struct {
	Events     []json.RawMessage `json:"events"`
	NextCursor *string           `json:"next_cursor"`
}

// exportProgress is what a file export leaves beside its output so an
// interrupted run can continue where it stopped.
type exportProgress struct {
	Cursor string `json:"cursor"`
	Rows   int    `json:"rows"`
	Query  string `json:"query"`
}

func progressPath(output string) string { return output + ".cursor" }

func readProgress(output string) (exportProgress, error) {
	var p exportProgress
	raw, err := os.ReadFile(progressPath(output))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(raw, &p)
}

// writeProgress replaces the progress file in one rename, so a crash leaves
// either the old or the new cursor, never a torn one.
func writeProgress(output string, p exportProgress) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := progressPath(output) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, progressPath(output))
}

func auditExportCmd() *cobra.Command {
	var f approvalFlags
	var filter auditFilter
	var format, output, org string
	var resume bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Download audit events as JSON lines or CSV",
		Long: `Download the organisation's audit events for a date range, optionally for one
user or service. The gateway serves them a page at a time; with --output the
file is written as it arrives and the position is kept in <file>.cursor, so an
export that stopped (a dropped connection, Ctrl-C) continues with --resume
instead of starting over. The output file is created with mode 0600 and must
not exist yet, except when resuming.`,
		Example: `  zuano audit export --from 2026-10-01 --to 2026-10-16 --format csv -o october.csv
  zuano audit export --from 2026-10-01 --to 2026-10-16 --format csv -o october.csv --resume`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(auditFormats, format) {
				return fmt.Errorf("invalid --format %q: use one of %s", format, strings.Join(auditFormats, ", "))
			}
			if resume && output == "" {
				return errors.New("--resume needs --output: there is no file to continue")
			}
			q, err := filter.query(url.Values{"limit": {strconv.Itoa(auditPageSize)}})
			if err != nil {
				return err
			}
			query := q.Encode()
			dest, progress, err := openAuditExport(cmd.OutOrStdout(), output, query, resume)
			if err != nil {
				return err
			}
			defer dest.Close()
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			base := "/api/org/" + url.PathEscape(org) + "/observability/audit?" + query
			rows, err := runAuditExport(cmd, c, base, format, output, dest, progress)
			if err != nil {
				if output != "" {
					return fmt.Errorf("%w\nexport stopped after %d rows; run the same command with --resume to continue", err, rows)
				}
				return err
			}
			if output != "" {
				_ = os.Remove(progressPath(output))
				fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d rows to %s\n", rows, output)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	filter.bind(cmd, true)
	cmd.Flags().StringVar(&org, "org", currentOrg, "Organisation id (default: the one your credential belongs to)")
	cmd.Flags().StringVar(&format, "format", "jsonl", "Export format: "+strings.Join(auditFormats, " or "))
	cmd.Flags().StringVarP(&output, "output", "o", "", "Write to this new file (mode 0600) instead of stdout")
	cmd.Flags().BoolVar(&resume, "resume", false, "Continue an interrupted export to --output")
	return cmd
}

// openAuditExport opens the destination. A fresh file export starts with an
// empty progress; a resumed one appends from the saved cursor, provided the
// filters are the ones the file was started with.
func openAuditExport(stdout io.Writer, output, query string, resume bool) (io.WriteCloser, exportProgress, error) {
	progress := exportProgress{Query: query}
	if output == "" {
		return nopCloser{stdout}, progress, nil
	}
	if !resume {
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, progress, fmt.Errorf("create %s (it must not exist yet; use --resume to continue an interrupted export): %w", output, err)
		}
		if err := writeProgress(output, progress); err != nil {
			_ = file.Close()
			return nil, progress, fmt.Errorf("save the export position: %w", err)
		}
		return file, progress, nil
	}
	saved, err := readProgress(output)
	if err != nil {
		return nil, progress, fmt.Errorf("nothing to resume: cannot read %s: %w", progressPath(output), err)
	}
	if saved.Query != query {
		return nil, progress, errors.New("the dates and filters differ from the interrupted export; give the same ones to --resume")
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, progress, fmt.Errorf("open %s to resume: %w", output, err)
	}
	return file, saved, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// runAuditExport fetches pages until the gateway reports no further cursor,
// returning the number of rows written in total.
func runAuditExport(cmd *cobra.Command, c *client.Client, base, format, output string, dest io.Writer, progress exportProgress) (int, error) {
	out := bufio.NewWriter(dest)
	var rows rowWriter = jsonlRows{out}
	header := false
	if format == "csv" {
		rows = csvRows{csv.NewWriter(out)}
		// The header goes out with the first page, so a run that failed
		// before any page arrived resumes into an empty file.
		header = progress.Rows == 0 && progress.Cursor == ""
	}
	total := progress.Rows
	cursor := progress.Cursor
	for {
		path := base
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page auditPage
		if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &page); err != nil {
			_ = rows.flush()
			_ = out.Flush()
			return total, err
		}
		if header {
			if err := rows.head(); err != nil {
				return total, err
			}
			header = false
		}
		for _, raw := range page.Events {
			if err := rows.write(raw); err != nil {
				return total, err
			}
			total++
		}
		if err := rows.flush(); err != nil {
			return total, err
		}
		if err := out.Flush(); err != nil {
			return total, err
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return total, nil
		}
		cursor = *page.NextCursor
		if output != "" {
			if err := writeProgress(output, exportProgress{Cursor: cursor, Rows: total, Query: progress.Query}); err != nil {
				return total, fmt.Errorf("save the export position: %w", err)
			}
		}
	}
}

type rowWriter interface {
	head() error
	write(raw json.RawMessage) error
	flush() error
}

type jsonlRows struct{ w io.Writer }

func (j jsonlRows) write(raw json.RawMessage) error {
	var line bytes.Buffer
	if err := json.Compact(&line, raw); err != nil {
		return fmt.Errorf("an audit event is not valid JSON: %w", err)
	}
	line.WriteByte('\n')
	_, err := j.w.Write(line.Bytes())
	return err
}

func (jsonlRows) head() error  { return nil }
func (jsonlRows) flush() error { return nil }

type csvRows struct{ w *csv.Writer }

func (c csvRows) write(raw json.RawMessage) error {
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return fmt.Errorf("an audit event is not a JSON object: %w", err)
	}
	record := make([]string, len(auditCSVColumns))
	for i, col := range auditCSVColumns {
		record[i] = csvCell(event[col])
	}
	return c.w.Write(record)
}

func (c csvRows) head() error { return c.w.Write(auditCSVColumns) }

func (c csvRows) flush() error {
	c.w.Flush()
	return c.w.Error()
}

// csvCell renders a value and defuses one a spreadsheet would run as a
// formula, because audit fields such as a user-chosen tool name are untrusted.
func csvCell(v any) string {
	var s string
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		s = t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		raw, _ := json.Marshal(t)
		s = string(raw)
	}
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

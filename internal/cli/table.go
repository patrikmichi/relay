package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// column is one table column: a header and the field it shows, as a dotted
// path into the row.
type column struct{ header, field string }

// fieldText reads a dotted path out of a decoded JSON object as text.
func fieldText(row map[string]any, path string) string {
	var cur any = row
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "-"
		}
		if cur, ok = m[part]; !ok {
			return "-"
		}
	}
	switch v := cur.(type) {
	case nil:
		return "-"
	case string:
		if v == "" {
			return "-"
		}
		return textsafe.Clean(v)
	case float64:
		return fmt.Sprintf("%g", v)
	case bool:
		return fmt.Sprintf("%t", v)
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = fmt.Sprint(x)
		}
		return textsafe.Clean(strings.Join(parts, ", "))
	}
	raw, _ := json.Marshal(cur)
	return textsafe.Clean(string(raw))
}

// printTable writes rows with the given columns.
func printTable(out io.Writer, rows []map[string]any, cols []column) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = fieldText(r, c.field)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

// printObject writes an object as sorted "key: value" lines.
func printObject(out io.Writer, obj map[string]any) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(out, "%s: %s\n", textsafe.Clean(k), fieldText(obj, k))
	}
}

// listCommand builds a read-only command that GETs path, takes the array under
// key and prints it as a table.
func listCommand(use, short, path, key, empty string, cols []column, extra func(*cobra.Command) func() string) *cobra.Command {
	var f approvalFlags
	var query func() string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			p := path
			if query != nil {
				p += query()
			}
			var doc map[string]json.RawMessage
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, p, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows []map[string]any
			_ = json.Unmarshal(doc[key], &rows)
			if len(rows) == 0 {
				fmt.Fprintln(out, empty)
				return nil
			}
			printTable(out, rows, cols)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	if extra != nil {
		query = extra(cmd)
	}
	return cmd
}

// showCommand builds a command that GETs path/<arg> and prints the object
// under key.
func showCommand(use, short, path, key string) *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc map[string]json.RawMessage
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path+"/"+url.PathEscape(args[0]), nil, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			var obj map[string]any
			_ = json.Unmarshal(doc[key], &obj)
			printObject(cmd.OutOrStdout(), obj)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

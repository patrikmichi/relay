package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const builderAPI = "/api/v1"

// maxBuilderBody bounds a document read from a file or standard input.
const maxBuilderBody = 1 << 20

// builderRequest sends one request of the builder API. A change an agent
// session makes may be held for approval (exit 12); a refusal the gateway gave
// a reason for keeps its exit status; any other 403 is a denial (exit 10) and
// every other failure is exit 1, whatever the builder refusal code, because
// the codes name the same few kinds of mistake.
func builderRequest(cmd *cobra.Command, c *client.Client, jsonOut bool, method, path string, body, out any) ([]byte, error) {
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
		if refusal := policyRefusal(raw, nil); refusal != nil {
			return nil, refusal
		}
		err := gatewayStatusError(status, raw)
		if status == http.StatusForbidden {
			return nil, &ExitError{Code: exitDenied, Msg: err.Error()}
		}
		return nil, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return raw, nil
}

func withQuery(path string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func printRows(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		for i := range r {
			r[i] = textsafe.Strict(r[i])
		}
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

// builderID checks that an id has the prefix its kind uses, so a mistyped
// argument cannot reach another path.
func builderID(arg, prefix, what string) (string, error) {
	if !strings.HasPrefix(arg, prefix) || len(arg) <= len(prefix) || strings.ContainsAny(arg, "/?#%: \t\n") {
		return "", fmt.Errorf("%q is not %s (expected %s...)", textsafe.Clean(arg), what, prefix)
	}
	return arg, nil
}

func readJSONBytes(raw []byte, what string) (map[string]any, error) {
	if len(raw) > maxBuilderBody {
		return nil, fmt.Errorf("%s is larger than %d bytes", what, maxBuilderBody)
	}
	var m map[string]any
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("%s must be one JSON object", what)
	}
	return m, nil
}

// readJSONInput reads a JSON object from a file, or from standard input when
// the file is "-" or absent and standard input is not a terminal. A value is
// never taken from a flag: it would end up in shell history and process lists.
func readJSONInput(cmd *cobra.Command, file, what string) (map[string]any, error) {
	var r io.Reader
	switch {
	case file != "" && file != "-":
		fh, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer fh.Close()
		r = fh
	default:
		if f, ok := cmd.InOrStdin().(*os.File); ok && isInteractiveTerminal(f) {
			return nil, fmt.Errorf("give %s as a JSON object on standard input or with --from-file; they are never taken from flags", what)
		}
		r = cmd.InOrStdin()
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBuilderBody+1))
	if err != nil {
		return nil, err
	}
	return readJSONBytes(raw, what)
}

// maskedFields lists the keys of a secret document without their values.
func maskedFields(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, textsafe.Strict(k))
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// BuilderCmd returns `zuano builder`, the commands for apps that give their
// own users agents: sessions that bound what an agent may do for one user.
func BuilderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "builder",
		Short: "Sessions and session configs for apps built on the gateway",
		Long: `A session ties one end user (or member) to a tool-access policy, the accounts it
may use and a little persistent state, and has an MCP address and short-lived
tokens an agent connects with. It can only narrow what the organisation and the
project already allow. Auth configs, connected accounts and connect links have
commands of their own: ` + brand.Cmd("auth-configs") + `, ` + brand.Cmd("accounts") + ` and ` + brand.Cmd("links") + `.

Exit status: 1 for a request the gateway refuses as wrong (a bad field, a
conflict, a policy widening, an unknown id), 10 for a denial, 12 when an agent
session's write is held for approval, 30 to 33 for the plan.`,
	}
	cmd.AddCommand(builderSessionsCmd(), builderSessionConfigsCmd())
	return cmd
}

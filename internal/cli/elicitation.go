package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// codeURLElicitationRequired is the JSON-RPC error a gateway answers a call
// with when a person must visit a page before the call can run.
const codeURLElicitationRequired = -32042

// urlElicitation is one page the gateway wants a person to visit, taken from
// the data of a -32042 error.
type urlElicitation struct {
	ID         string
	URL        string
	Message    string
	ApprovalID string
}

func elicitationFromError(e *mcpRPCError) *urlElicitation {
	var data struct {
		Elicitations []struct {
			Mode          string `json:"mode"`
			ElicitationID string `json:"elicitationId"`
			URL           string `json:"url"`
			Message       string `json:"message"`
		} `json:"elicitations"`
		Relay struct {
			ApprovalID string `json:"approval_id"`
		} `json:"relay"`
	}
	if e == nil || json.Unmarshal(e.Data, &data) != nil {
		return nil
	}
	for _, el := range data.Elicitations {
		if el.URL == "" || (el.Mode != "" && el.Mode != "url") {
			continue
		}
		out := &urlElicitation{ID: el.ElicitationID, URL: el.URL, Message: el.Message, ApprovalID: data.Relay.ApprovalID}
		if out.ApprovalID == "" {
			out.ApprovalID = approvalIDFromPage(el.URL)
		}
		return out
	}
	return nil
}

// approvalIDFromPage reads the id out of .../approvals/<id>.
func approvalIDFromPage(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Path, "/approvals/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

// elicitationPage checks that link is a web address. sameOrigin reports
// whether it is a page on the gateway the call went to; zuano opens only
// those itself, since a page elsewhere could imitate a passkey prompt.
func elicitationPage(gateway, link string) (page *url.URL, sameOrigin bool, err error) {
	page, err = url.Parse(link)
	if err != nil || (page.Scheme != "https" && page.Scheme != "http") || page.Host == "" || page.User != nil {
		return nil, false, errors.New("the gateway sent an approval link that is not a web address; refusing to use it")
	}
	base, baseErr := url.Parse(gateway)
	return page, baseErr == nil && page.Scheme == base.Scheme && page.Host == base.Host, nil
}

// highlightHost writes the address with its host in bold underline, so the one
// part that says whose page this is stands out.
func highlightHost(page *url.URL, color bool) string {
	link := textsafe.Clean(page.String())
	host := textsafe.Clean(page.Host)
	if !color {
		return link
	}
	before, after, ok := strings.Cut(link, "//"+host)
	if !ok {
		return link
	}
	return before + "//\x1b[1;4m" + host + "\x1b[0m" + after
}

func colorEnabled(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && os.Getenv("NO_COLOR") == "" && isInteractiveTerminal(f)
}

// confirm asks a yes/no question that defaults to yes.
func confirm(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprint(out, question)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// handleURLElicitation sends a person to the approval page the gateway named,
// waits for the decision and runs the call again once it is approved.
// Without a person at the keyboard (an agent session, a script, --json) it
// does not open anything and waits only when --wait asks it to.
func handleURLElicitation(parent context.Context, c *client.Client, e *urlElicitation, retry func() (*heldCall, error), jsonOut bool, o callOpts, out io.Writer) error {
	page, sameOrigin, err := elicitationPage(c.GatewayURL, e.URL)
	if err != nil {
		return err
	}
	if e.ApprovalID == "" {
		return errors.New("the gateway asked for an approval without saying which one; open the link it sent and repeat the call")
	}
	interactive := o.interactive && !jsonOut
	if !jsonOut {
		fmt.Fprintf(out, "Approval required: %s\n", textsafe.Clean(firstNonEmptyString(e.Message, "this call is held until a person approves it")))
		fmt.Fprintf(out, "  %s\n  host: %s\n", highlightHost(page, colorEnabled(out)), textsafe.Clean(page.Host))
		if !sameOrigin {
			fmt.Fprintf(out, "This page is not on your gateway (%s); zuano will not open it. Check the host before you visit it.\n", textsafe.Clean(c.GatewayURL))
		}
	}
	if interactive && sameOrigin && !o.noBrowser && confirm(o.in, out, "Open it in your browser? [Y/n] ") {
		if err := openBrowserURL(page.String()); err != nil {
			fmt.Fprintf(out, "Could not open a browser (%v); open the link above.\n", err)
		}
	}

	total := o.waitFor
	switch {
	case o.wait:
	case interactive:
		total = time.Duration(defaultWaitSeconds) * time.Second
	default:
		if !jsonOut {
			fmt.Fprintf(out, "Approve it at the link, then repeat the call or run `%s`.\n", brand.Cmd("approvals wait "+textsafe.Clean(e.ApprovalID)))
		}
		return &ExitError{Code: exitPending}
	}
	if !jsonOut {
		fmt.Fprintf(out, "Waiting up to %s for the decision...\n", total)
	}
	_, exit, err := pollDecision(parent, c, e.ApprovalID, total, out, jsonOut)
	if err != nil {
		return err
	}
	if exit != 0 {
		return &ExitError{Code: exit}
	}
	if !jsonOut {
		fmt.Fprintln(out, "Approved. Running the call again.")
	}
	again, err := retry()
	if err != nil {
		return err
	}
	if again != nil {
		return &ExitError{Code: exitPending, Msg: "the call is held for approval again; nothing more was run"}
	}
	return nil
}

// bindPresence records who is at the keyboard and whether a browser may be
// opened, for a call that may need a person's approval.
func (o *callOpts) bindPresence(cmd *cobra.Command, noBrowser bool) {
	o.noBrowser = noBrowser
	o.in = cmd.InOrStdin()
	o.interactive = canOpenBrowser(cmd, false)
}

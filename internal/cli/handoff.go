package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/qr"
	"github.com/patrikmichi/relay/internal/textsafe"
)

// canOpenBrowser is false when a person is not at the keyboard: an agent
// session, a script, or an explicit --no-browser.
func canOpenBrowser(cmd *cobra.Command, noBrowser bool) bool {
	return !noBrowser && !config.AgentMode() && canPrompt(cmd.InOrStdin())
}

// openIfInteractive opens link in the browser when a person is present and
// reports whether it did.
func openIfInteractive(cmd *cobra.Command, link string, noBrowser bool) bool {
	return canOpenBrowser(cmd, noBrowser) && openBrowserURL(link) == nil
}

// showLink hands the person a link to finish in a browser: it opens one when
// it can, and otherwise prints the link with a QR code, so a phone with the
// person's passkey can take over. It reports whether a browser was opened.
func showLink(cmd *cobra.Command, link string, noBrowser bool, hint string) bool {
	return presentLink(cmd.OutOrStdout(), link, canOpenBrowser(cmd, noBrowser), hint)
}

func presentLink(out io.Writer, link string, tryBrowser bool, hint string) bool {
	link = textsafe.Clean(link)
	if tryBrowser {
		err := openBrowserURL(link)
		if err == nil {
			fmt.Fprintf(out, "Opened %s\n%s.\n", link, hint)
			return true
		}
		fmt.Fprintf(out, "Could not open a browser (%v).\n", err)
	}
	fmt.Fprintf(out, "%s: open this link in a browser, or scan the code with your phone.\n  %s\n", hint, link)
	if modules, err := qr.Encode(link); err == nil {
		fmt.Fprintln(out)
		fmt.Fprint(out, qr.Render(modules))
	}
	return false
}

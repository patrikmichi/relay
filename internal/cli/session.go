// Package cli implements the cobra command definitions.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/patrikmichi/relay/internal/client"
)

// resolveClient builds an authenticated *client.Client for gateway calls
// (see client.Resolve for the GATEWAY_API_KEY / keychain resolution order
// and the transparent OAuth refresh behavior it wires up), exiting the
// process with a user-facing message on failure.
//
// This is the single auth-resolution entry point every command should use
// — it replaces the RELAY_EMAIL-env-var + keychain.ReadToken boilerplate
// that used to be duplicated per-command (and never refreshed a token, or
// supported a static GATEWAY_API_KEY bearer session).
func resolveClient(gatewayURL string) *client.Client {
	c, err := client.Resolve(gatewayURL)
	if err != nil {
		if errors.Is(err, client.ErrNotLoggedIn) {
			fmt.Fprintln(os.Stderr, "Not logged in. Run `relay login` first, or set GATEWAY_API_KEY for non-interactive auth.")
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	return c
}

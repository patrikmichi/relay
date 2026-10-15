// Package cli implements the cobra command definitions.
package cli

import (
	"errors"

	"github.com/patrikmichi/relay/internal/client"
)

// resolveClient builds an authenticated *client.Client for gateway calls
// (see client.Resolve for the GATEWAY_API_KEY / keychain resolution order
// and the transparent OAuth refresh behavior it wires up).
//
// This is the single auth-resolution entry point every command should use
// — it replaces the RELAY_EMAIL-env-var + keychain.ReadToken boilerplate
// that used to be duplicated per-command (and never refreshed a token, or
// supported a static GATEWAY_API_KEY bearer session).
//
// Returns an error instead of exiting so every caller's RunE propagates it
// through cobra as the single source of truth for error reporting — only
// cmd/relay/main.go prints an error and calls os.Exit.
func resolveClient(gatewayURL string) (*client.Client, error) {
	c, err := client.Resolve(gatewayURL)
	if err != nil {
		if errors.Is(err, client.ErrNotLoggedIn) {
			return nil, errors.New("not logged in. Run `relay login` first, or set GATEWAY_API_KEY for non-interactive auth")
		}
		return nil, err
	}
	return c, nil
}

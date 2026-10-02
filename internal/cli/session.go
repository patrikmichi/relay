// Package cli implements the cobra command definitions.
package cli

import (
	"errors"

	"github.com/patrikmichi/relay/internal/client"
)

// resolveClient builds an authenticated *client.Client for gateway calls (see client.Resolve for the GATEWAY_API_KEY / keychain resolution order and the transparent OAuth refresh behavior it wires up).
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

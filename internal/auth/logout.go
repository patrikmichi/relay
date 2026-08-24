package auth

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/keychain"
)

// Logout revokes the token family on the server and deletes the keychain
// entry for c.Email(). Dispatched through c.Post so a near-expiry token
// still gets one refresh attempt before the revoke call — otherwise a
// stale-but-refreshable session would spuriously log the "server returned
// 401" warning below instead of just working.
func Logout(c *client.Client) error {
	resp, err := c.Post("/api/cli/logout", "application/json", nil)
	if err != nil {
		// Server unavailable — still delete local keychain entry
		fmt.Printf("Warning: could not reach gateway to revoke session: %v\n", err)
	} else {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var errBody map[string]string
			_ = json.NewDecoder(resp.Body).Decode(&errBody)
			fmt.Printf("Warning: server returned %d during logout: %s\n",
				resp.StatusCode, errBody["error"])
		}
	}

	// Always delete the keychain entry
	if err := keychain.DeleteToken(c.Email()); err != nil {
		return fmt.Errorf("delete keychain entry: %w", err)
	}
	return nil
}

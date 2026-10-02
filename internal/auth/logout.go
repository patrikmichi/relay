package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/keychain"
)

// LogoutResult reports the two independently-outcomed halves of a logout:
// removing the local keychain entry (always attempted, regardless of
// network reachability) and revoking the token family server-side
// (best-effort). The two are distinguished explicitly rather than folded
// into one boolean/error, so a caller can tell "revoked" apart from
// "removed locally only" instead of guessing from a printed warning.
type LogoutResult struct {
	Email string
	// RemoteRevoked is true only when the server confirmed the revoke with
	// HTTP 200. False covers both an unreachable gateway and any non-200
	// response — RemoteWarning explains which.
	RemoteRevoked bool
	// RemoteWarning is a human-readable reason RemoteRevoked is false; ""
	// when RemoteRevoked is true.
	RemoteWarning string
}

// Logout revokes the token family on the server (best-effort, bounded by
// ctx) and unconditionally deletes the keychain entry for
// c.GatewayOrigin()/c.Email() — a failed or unreachable remote revoke must
// never block the local cleanup the user asked for. Dispatched through
// c.PostContext so a near-expiry token still gets one refresh attempt
// before the revoke call.
func Logout(ctx context.Context, c *client.Client) (LogoutResult, error) {
	result := LogoutResult{Email: c.Email()}

	resp, err := c.PostContext(ctx, "/api/cli/logout", "application/json", nil)
	switch {
	case err != nil:
		result.RemoteWarning = fmt.Sprintf("could not reach gateway to revoke session: %v", err)
	case resp.StatusCode == http.StatusOK:
		_ = resp.Body.Close()
		result.RemoteRevoked = true
	default:
		_ = resp.Body.Close()
		result.RemoteWarning = fmt.Sprintf("server returned HTTP %d during logout", resp.StatusCode)
	}

	// Always delete the keychain entry — offline logout removes exactly the
	// selected (gatewayOrigin, email) entry, independent of the remote
	// outcome above.
	if err := keychain.DeleteToken(c.GatewayOrigin(), c.Email()); err != nil {
		return result, fmt.Errorf("delete keychain entry: %w", err)
	}
	return result, nil
}

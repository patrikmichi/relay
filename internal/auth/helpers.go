package auth

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// writeToken is a shared helper to write a token to the OS keychain — bound
// to both gatewayOrigin and email — and persist the logged-in email
// (config.SetEmail) so subsequent commands don't require
// RELAY_EMAIL to be set. Callers should propagate a non-nil return as a
// user-visible abort — the keychain write is the one that matters; a
// config-persist failure is logged but non-fatal (mirrors Login's same
// best-effort treatment in login.go).
func writeToken(gatewayOrigin, email, accessToken, refreshToken string, expiresIn int) error {
	if err := keychain.WriteToken(gatewayOrigin, email, keychain.TokenData{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
	}); err != nil {
		return err
	}
	removeLegacySession(email, os.Stderr)

	if err := config.SetEmail(email); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist login email to config: %v\n", err)
	}
	return nil
}

// removeLegacySession deletes the email-only session an older relay version
// may have left, now that a gateway-bound session replaces it. Failure only
// warns: the new session is already stored.
func removeLegacySession(email string, diagnostic io.Writer) {
	if err := keychain.DeleteLegacyToken(email); err != nil {
		fmt.Fprintf(diagnostic, "Warning: could not remove the old session entry: %v\n", err)
	}
}

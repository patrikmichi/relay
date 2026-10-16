package auth

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/keychain"
)

// writeToken is a shared helper to write a token to the OS keychain — bound
// to profile, gatewayOrigin and email — and persist the logged-in email
// (persistLogin) so subsequent commands don't require ZUANO_EMAIL to be set.
// Callers should propagate a non-nil return as a user-visible abort — the
// keychain write is the one that matters; a config-persist failure is logged
// but non-fatal (mirrors Login's same best-effort treatment in login.go).
func writeToken(profile keychain.Profile, gatewayOrigin, email, accessToken, refreshToken string, expiresIn int) error {
	return writeBoundToken(profile, gatewayOrigin, email, accessToken, refreshToken, expiresIn, "")
}

// writeBoundToken is writeToken for a session bound to the device key keyRef.
func writeBoundToken(profile keychain.Profile, gatewayOrigin, email, accessToken, refreshToken string, expiresIn int, keyRef string) error {
	if err := keychain.WriteProfileToken(profile, gatewayOrigin, email, keychain.TokenData{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
		DPoPKey:      keyRef,
	}); err != nil {
		return err
	}
	persistLogin(profile, email, os.Stderr)
	return nil
}

// persistLogin records who logged in so later commands find the session
// without ZUANO_EMAIL: the human identity for a human login, a separate
// agent identity for an agent login, which must not change what the unmarked
// commands use. Failure only warns: the session is already stored.
func persistLogin(profile keychain.Profile, email string, diagnostic io.Writer) {
	if profile == keychain.ProfileAgent {
		if err := config.SetAgentEmail(email); err != nil {
			fmt.Fprintf(diagnostic, "Warning: could not persist login email to config: %v\n", err)
		}
		return
	}
	removeUnboundSession(email, diagnostic)
	if err := config.SetEmail(email); err != nil {
		fmt.Fprintf(diagnostic, "Warning: could not persist login email to config: %v\n", err)
	}
}

// removeUnboundSession deletes an email-only entry that no gateway-bound
// session can use, now that a bound session replaces it. Failure only warns:
// the new session is already stored.
func removeUnboundSession(email string, diagnostic io.Writer) {
	if err := keychain.DeleteUnboundToken(email); err != nil {
		fmt.Fprintf(diagnostic, "Warning: could not remove the old session entry: %v\n", err)
	}
}

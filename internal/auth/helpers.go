package auth

import (
	"fmt"
	"os"
	"time"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// writeToken is a shared helper to write a token to the OS keychain, and
// persist the logged-in email (config.SetEmail) so subsequent commands
// don't require RELAY_EMAIL to be set. Callers should propagate a non-nil
// return as a user-visible abort — the keychain write is the one that
// matters; a config-persist failure is logged but non-fatal (mirrors
// Login's same best-effort treatment in login.go).
func writeToken(email, accessToken, refreshToken string, expiresIn int) error {
	if err := keychain.WriteToken(email, keychain.TokenData{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		Email:        email,
		ExpiresIn:    expiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
	}); err != nil {
		return err
	}

	if err := config.SetEmail(email); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist login email to config: %v\n", err)
	}
	return nil
}

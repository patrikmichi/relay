// Package keychain provides secure token storage using the OS keychain.
// On macOS: Keychain. On Linux: libsecret/D-Bus. On Windows: Credential Manager.
//
// IMPORTANT: If WriteToken returns an error, the caller MUST abort with a user-visible
// error message. No fallback to disk storage is permitted under any circumstances.
package keychain

import (
	"encoding/json"
	"fmt"

	"github.com/zalando/go-keyring"
)

const serviceName = "relay-cli"

// TokenData holds the OAuth token pair and metadata stored in the keychain.
type TokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
	ExpiresIn    int    `json:"expires_in"`
	// ExpiresAt is the absolute Unix-seconds expiry of AccessToken, computed
	// at write time (now + ExpiresIn). ExpiresIn alone only records a
	// relative TTL as of mint time, which is useless for a later process to
	// determine "is this token stale" without also knowing when it was
	// minted — ExpiresAt makes that check possible without re-deriving it.
	// Zero on tokens written before this field existed; callers must treat
	// zero as "unknown / assume stale" rather than "never expires".
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// WriteToken stores a token in the OS keychain for the given email address.
// Returns a non-nil error if the keychain is unavailable. The caller MUST
// abort with a user-visible error — no disk fallback is permitted.
func WriteToken(email string, data TokenData) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	if err := keyring.Set(serviceName, accountKey(email), string(raw)); err != nil {
		return fmt.Errorf(
			"could not store token in system keychain for %s: %w\n"+
				"On Linux, ensure a D-Bus session and gnome-keyring or kwallet is running.",
			email, err,
		)
	}
	return nil
}

// ReadToken retrieves the stored token for the given email address.
// Returns ErrNotFound (via go-keyring) if no token exists.
func ReadToken(email string) (TokenData, error) {
	raw, err := keyring.Get(serviceName, accountKey(email))
	if err != nil {
		return TokenData{}, fmt.Errorf("read token for %s: %w", email, err)
	}
	var data TokenData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return TokenData{}, fmt.Errorf("unmarshal token for %s: %w", email, err)
	}
	return data, nil
}

// DeleteToken removes the stored token for the given email address.
// Returns nil if no token exists (idempotent).
func DeleteToken(email string) error {
	err := keyring.Delete(serviceName, accountKey(email))
	if err == keyring.ErrNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete token for %s: %w", email, err)
	}
	return nil
}

// accountKey returns the keychain account name for an email address.
// Format matches the plan spec: oauth-refresh-token:<email>
func accountKey(email string) string {
	return "oauth-refresh-token:" + email
}

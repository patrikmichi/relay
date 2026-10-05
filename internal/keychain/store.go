// Package keychain provides secure token storage using the OS keychain.
// On macOS: Keychain. On Linux: libsecret/D-Bus. On Windows: Credential Manager.
//
// IMPORTANT: If WriteToken returns an error, the caller MUST abort with a user-visible
// error message. No fallback to disk storage is permitted under any circumstances.
package keychain

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

const serviceName = "relay-cli"

// schemaVersion tags the shape of a persisted session. A version-2 session
// is bound to a gateway identity; version 1 (or the zero value, for entries
// written before this field existed) is a legacy email-only session with no
// trustworthy origin; client.Resolve migrates one only to the gateway saved
// in the config file.
const schemaVersion = 2

// TokenData holds the OAuth token pair and metadata stored in the keychain.
type TokenData struct {
	// SchemaVersion is set by WriteToken; callers never need to set it.
	SchemaVersion int `json:"schema_version,omitempty"`
	// GatewayOrigin is the normalized gateway identity (config.NormalizeGatewayURL
	// output) this session was issued for. Set by WriteToken; callers never
	// need to set it.
	GatewayOrigin string `json:"gateway_origin,omitempty"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	Email         string `json:"email"`
	ExpiresIn     int    `json:"expires_in"`
	// ExpiresAt is the absolute Unix-seconds expiry of AccessToken, computed
	// at write time (now + ExpiresIn). ExpiresIn alone only records a
	// relative TTL as of mint time, which is useless for a later process to
	// determine "is this token stale" without also knowing when it was
	// minted — ExpiresAt makes that check possible without re-deriving it.
	// Zero on tokens written before this field existed; callers must treat
	// zero as "unknown / assume stale" rather than "never expires".
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// WriteToken stores a token in the OS keychain, keyed by BOTH the gateway
// identity it was issued for and the account email — a session must never
// be readable, and therefore never usable, against any other gateway.
// gatewayOrigin must already be normalized (see config.NormalizeGatewayURL);
// both it and email are required so a session can never be persisted
// without a trustworthy origin binding. Returns a non-nil error if the
// keychain is unavailable. The caller MUST abort with a user-visible error —
// no disk fallback is permitted.
func WriteToken(gatewayOrigin, email string, data TokenData) error {
	if gatewayOrigin == "" {
		return fmt.Errorf("write token: gateway identity is required")
	}
	if email == "" {
		return fmt.Errorf("write token: email is required")
	}
	data.SchemaVersion = schemaVersion
	data.GatewayOrigin = gatewayOrigin
	data.Email = email

	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	if err := keyring.Set(serviceName, accountKey(gatewayOrigin, email), string(raw)); err != nil {
		return fmt.Errorf(
			"could not store token in system keychain for %s@%s: %w\n"+
				"On Linux, ensure a D-Bus session and gnome-keyring or kwallet is running",
			email, gatewayOrigin, err,
		)
	}
	return nil
}

// ReadToken retrieves the stored token for the given gateway identity and
// email address. Returns an error (via go-keyring's ErrNotFound, wrapped) if
// no session exists for that exact (gatewayOrigin, email) pair — it never
// falls back to a legacy or differently-scoped entry; see HasLegacyToken for
// detecting one to report to the user.
func ReadToken(gatewayOrigin, email string) (TokenData, error) {
	if gatewayOrigin == "" {
		return TokenData{}, fmt.Errorf("read token: gateway identity is required")
	}
	raw, err := keyringGet(accountKey(gatewayOrigin, email))
	if err != nil {
		return TokenData{}, fmt.Errorf("read token for %s@%s: %w", email, gatewayOrigin, err)
	}
	var data TokenData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return TokenData{}, fmt.Errorf("unmarshal token for %s@%s: %w", email, gatewayOrigin, err)
	}
	return data, nil
}

// DeleteToken removes the stored token for the given gateway identity and
// email address. Returns nil if no token exists (idempotent).
func DeleteToken(gatewayOrigin, email string) error {
	err := keyring.Delete(serviceName, accountKey(gatewayOrigin, email))
	if err == keyring.ErrNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete token for %s@%s: %w", email, gatewayOrigin, err)
	}
	return nil
}

// accountKey returns the keychain account name for a (gateway, email) pair.
func accountKey(gatewayOrigin, email string) string {
	return "oauth-refresh-token:" + gatewayOrigin + ":" + email
}

// legacyAccountKey returns the older keychain account name: email only, no
// gateway binding.
func legacyAccountKey(email string) string {
	return "oauth-refresh-token:" + email
}

// ErrNotFound reports that no session is stored under the requested key.
var ErrNotFound = keyring.ErrNotFound

// UnavailableError reports that the OS keychain itself could not be read,
// as opposed to holding no session.
type UnavailableError struct{ Err error }

func (e *UnavailableError) Error() string { return "keychain unavailable: " + e.Err.Error() }
func (e *UnavailableError) Unwrap() error { return e.Err }

func keyringGet(account string) (string, error) {
	raw, err := keyring.Get(serviceName, account)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return "", &UnavailableError{Err: err}
	}
	return raw, err
}

// HasLegacyToken reports whether an email-only session from an older relay
// version exists for email, without returning its contents.
func HasLegacyToken(email string) bool {
	_, err := keyring.Get(serviceName, legacyAccountKey(email))
	return err == nil
}

// ReadLegacyToken returns the email-only session for email. Callers may use
// it only after binding it to a gateway with WriteToken; see
// client.Resolve for when that is safe. Wraps ErrNotFound when none exists.
func ReadLegacyToken(email string) (TokenData, error) {
	raw, err := keyringGet(legacyAccountKey(email))
	if err != nil {
		return TokenData{}, fmt.Errorf("read legacy token for %s: %w", email, err)
	}
	var data TokenData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return TokenData{}, fmt.Errorf("unmarshal legacy token for %s: %w", email, err)
	}
	if data.AccessToken == "" && data.RefreshToken == "" {
		return TokenData{}, fmt.Errorf("legacy session for %s holds no token", email)
	}
	return data, nil
}

// DeleteLegacyToken removes a pre-gateway-identity session for email,
// letting a user explicitly clear it out after logging in again against a
// specific gateway. Idempotent.
func DeleteLegacyToken(email string) error {
	err := keyring.Delete(serviceName, legacyAccountKey(email))
	if err == keyring.ErrNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete legacy token for %s: %w", email, err)
	}
	return nil
}

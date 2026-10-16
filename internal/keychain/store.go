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

	"github.com/patrikmichi/zuano/internal/brand"
)

const serviceName = brand.Name + "-cli"

// schemaVersion tags the shape of a persisted session: bound to a gateway
// identity.
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
	// DPoPKey references the device key a strict organisation bound this
	// session to ("file:<id>" or "tpm:<id>", see internal/dpop). Empty for an
	// ordinary bearer session. Every request and refresh of the session must
	// carry a proof from that key.
	DPoPKey string `json:"dpop_key,omitempty"`
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
	return WriteProfileToken(ProfileHuman, gatewayOrigin, email, data)
}

// Profile separates the sessions of a person and of the coding agents acting
// for them, so both can be signed in to the same gateway and account at once.
type Profile string

const (
	ProfileHuman Profile = "human"
	ProfileAgent Profile = "agent"
)

// WriteProfileToken is WriteToken for an explicit profile.
func WriteProfileToken(profile Profile, gatewayOrigin, email string, data TokenData) error {
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
	if err := keyring.Set(serviceName, accountKey(profile, gatewayOrigin, email), string(raw)); err != nil {
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
// falls back to an unbound or differently-scoped entry; see HasUnboundToken
// for detecting one to report to the user.
func ReadToken(gatewayOrigin, email string) (TokenData, error) {
	return ReadProfileToken(ProfileHuman, gatewayOrigin, email)
}

// ReadProfileToken is ReadToken for an explicit profile.
func ReadProfileToken(profile Profile, gatewayOrigin, email string) (TokenData, error) {
	if gatewayOrigin == "" {
		return TokenData{}, fmt.Errorf("read token: gateway identity is required")
	}
	raw, err := keyringGet(accountKey(profile, gatewayOrigin, email))
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
	return DeleteProfileToken(ProfileHuman, gatewayOrigin, email)
}

// DeleteProfileToken is DeleteToken for an explicit profile.
func DeleteProfileToken(profile Profile, gatewayOrigin, email string) error {
	err := keyring.Delete(serviceName, accountKey(profile, gatewayOrigin, email))
	if err == keyring.ErrNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete token for %s@%s: %w", email, gatewayOrigin, err)
	}
	return nil
}

// accountKey returns the keychain account name for a (profile, gateway,
// email) triple. The human profile keeps the original name so existing
// sessions stay valid; a gateway origin always starts with a scheme, so the
// "agent:" prefix cannot collide with it.
func accountKey(profile Profile, gatewayOrigin, email string) string {
	if profile == ProfileAgent {
		return "oauth-refresh-token:agent:" + gatewayOrigin + ":" + email
	}
	return "oauth-refresh-token:" + gatewayOrigin + ":" + email
}

// unboundAccountKey is the keychain account name of an entry with no
// gateway binding: the email alone.
func unboundAccountKey(email string) string {
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

// HasUnboundToken reports whether an email-only entry, written before
// sessions were bound to a gateway, exists for email. It is never used to
// sign in: the entry carries no proof of which gateway issued it.
func HasUnboundToken(email string) bool {
	_, err := keyring.Get(serviceName, unboundAccountKey(email))
	return err == nil
}

// DeleteUnboundToken removes the email-only entry for email. Idempotent.
func DeleteUnboundToken(email string) error {
	err := keyring.Delete(serviceName, unboundAccountKey(email))
	if err == keyring.ErrNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete unbound token for %s: %w", email, err)
	}
	return nil
}

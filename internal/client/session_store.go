package client

import (
	"context"

	"github.com/patrikmichi/relay/internal/client/sessionlock"
	"github.com/patrikmichi/relay/internal/keychain"
)

// SessionStore abstracts persisted OAuth session storage — the OS keychain
// in production — so refresh coordination (see refreshLocked) can be
// exercised against an in-memory fake in tests instead of the real
// keychain. Implementations must treat TokenData as a plain value: never
// return a reference callers could mutate behind the Client's back.
type SessionStore interface {
	Read(gatewayOrigin, email string) (keychain.TokenData, error)
	Write(gatewayOrigin, email string, data keychain.TokenData) error
}

type keychainSessionStore struct{}

func (keychainSessionStore) Read(gatewayOrigin, email string) (keychain.TokenData, error) {
	return keychain.ReadToken(gatewayOrigin, email)
}

func (keychainSessionStore) Write(gatewayOrigin, email string, data keychain.TokenData) error {
	return keychain.WriteToken(gatewayOrigin, email, data)
}

// SessionUnlocker releases a lock acquired via SessionLocker.Lock.
type SessionUnlocker interface {
	Unlock() error
}

// SessionLocker abstracts the cross-process refresh-coordination lock (see
// internal/client/sessionlock) so tests can inject a fake, or drive the
// real file-lock mechanism directly with full control over timing.
type SessionLocker interface {
	Lock(ctx context.Context, gatewayOrigin, email string) (SessionUnlocker, error)
}

type flockSessionLocker struct{}

func (flockSessionLocker) Lock(ctx context.Context, gatewayOrigin, email string) (SessionUnlocker, error) {
	return sessionlock.Acquire(ctx, gatewayOrigin, email)
}

// ClientOption customizes a Client built by Resolve. Production code never
// needs one — it exists so tests can inject a fake SessionStore/
// SessionLocker for deterministic, real-lock-file concurrency coverage
// without touching the OS keychain.
type ClientOption func(*Client)

// WithSessionStore overrides the default OS-keychain-backed SessionStore.
func WithSessionStore(s SessionStore) ClientOption {
	return func(c *Client) { c.store = s }
}

// WithSessionLocker overrides the default OS-file-lock-backed SessionLocker.
func WithSessionLocker(l SessionLocker) ClientOption {
	return func(c *Client) { c.locker = l }
}

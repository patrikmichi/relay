package recipes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/patrikmichi/zuano/internal/signing"
)

// KeyCache keeps the published recipe keys on disk, so showing a recipe
// verifies it without a request for the keys each time.
type KeyCache struct {
	// Path is the cache file.
	Path string
	// TTL is how long a cached set is used.
	TTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Fetch reads the keys published by the origin.
	Fetch func(ctx context.Context, origin string) ([]signing.Key, error)
}

type cachedKeys struct {
	Origin    string        `json:"origin"`
	FetchedAt time.Time     `json:"fetchedAt"`
	Keys      []signing.Key `json:"keys"`
}

func (c KeyCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Keys returns the published keys. A cached set is used while it is fresh and
// holds wantKID; a key it lacks (a rotation) or a stale set is fetched again.
func (c KeyCache) Keys(ctx context.Context, origin, wantKID string) ([]signing.Key, error) {
	if raw, err := os.ReadFile(c.Path); err == nil {
		var cached cachedKeys
		if json.Unmarshal(raw, &cached) == nil && cached.Origin == origin && c.now().Sub(cached.FetchedAt) < c.TTL &&
			(wantKID == "" || slices.ContainsFunc(cached.Keys, func(k signing.Key) bool { return k.KID == wantKID })) {
			return cached.Keys, nil
		}
	}
	keys, err := c.Fetch(ctx, origin)
	if err != nil {
		return nil, err
	}
	if raw, err := json.Marshal(cachedKeys{Origin: origin, FetchedAt: c.now(), Keys: keys}); err == nil {
		if os.MkdirAll(filepath.Dir(c.Path), 0o755) == nil {
			// Not being able to cache costs a request next time, nothing more.
			_ = os.WriteFile(c.Path, raw, 0o644)
		}
	}
	return keys, nil
}

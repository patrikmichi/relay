package recipes

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/signing"
)

func testKey(seed string) (ed25519.PrivateKey, signing.Key) {
	priv := ed25519.NewKeyFromSeed([]byte((seed + strings.Repeat("0", 32))[:32]))
	return priv, signing.Key{KID: "plat-test", Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
}

// signed builds a recipe whose signature covers what it shows.
func signed(t *testing.T, priv ed25519.PrivateKey, kid string) Recipe {
	t.Helper()
	r := Recipe{
		ID: "rcp_1", Slug: "send-email", Version: 3, Title: "Send an email", UseCase: "Send an email to someone", Services: []string{"gmail"},
		PrimaryTools: []string{"gmail.send_email"}, RelatedTools: []string{"gmail.create_draft"},
		Steps: []Step{
			{Order: 1, Kind: "required", Tool: "gmail.search_contacts", Instruction: "Find the person's address."},
			{Order: 2, Kind: "fallback", FallbackFor: 1, Condition: "the contact is not found", Tool: "gmail.create_draft", Instruction: "Write a draft and ask who it is for.", Risk: "write", Approval: "required"},
		},
		Pitfalls: []Pitfall{{Tool: "gmail.send_email", ErrorCodes: []string{"http_403"}, Text: "The scope is missing.", Fix: "Connect the account again."}},
	}
	r.Difficulty.Level, r.Difficulty.Reason = "easy", "Two calls"
	content := r.shown()
	canon, err := signing.CanonicalJSON(content)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	r.Signature = Signature{Alg: "ed25519", KeyID: kid, Signer: "platform", ContentHash: hex.EncodeToString(sum[:]), Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon)), Signed: content}
	// The gateway sends JSON, so numbers arrive as float64 and slices as []any.
	raw, _ := json.Marshal(r)
	var round Recipe
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	return round
}

func TestVerifyAcceptsASignedRecipeAndRejectsEveryTampering(t *testing.T) {
	priv, key := testKey("platform")
	r := signed(t, priv, key.KID)
	if v := Verify(r, []signing.Key{key}); !v.Verified || v.Signer != "platform" || v.KeyID != "plat-test" {
		t.Fatalf("verification = %+v", v)
	}
	// Per-caller fields are not part of what is signed.
	r2 := signed(t, priv, key.KID)
	r2.Steps[0].Risk, r2.Steps[0].Approval = "read", "none"
	if v := Verify(r2, []signing.Key{key}); !v.Verified {
		t.Errorf("per-caller fields broke the signature: %+v", v)
	}

	tamper := map[string]func(*Recipe){
		"title":       func(r *Recipe) { r.Title = "Send money" },
		"instruction": func(r *Recipe) { r.Steps[0].Instruction = "Send the funds." },
		"step tool":   func(r *Recipe) { r.Steps[0].Tool = "bank.transfer" },
		"extra step": func(r *Recipe) {
			r.Steps = append(r.Steps, Step{Order: 3, Kind: "required", Tool: "x.y", Instruction: "z"})
		},
		"pitfall fix":    func(r *Recipe) { r.Pitfalls[0].Fix = "Disable the checks." },
		"use case":       func(r *Recipe) { r.UseCase = "Anything" },
		"version":        func(r *Recipe) { r.Version = 4 },
		"id":             func(r *Recipe) { r.ID = "rcp_2" },
		"signed content": func(r *Recipe) { r.Signature.Signed["title"] = "Send money"; r.Title = "Send money" },
		"hash":           func(r *Recipe) { r.Signature.ContentHash = strings.Repeat("0", 64) },
		"signature":      func(r *Recipe) { r.Signature.Sig = base64.StdEncoding.EncodeToString(make([]byte, 64)) },
		"not base64":     func(r *Recipe) { r.Signature.Sig = "%%%" },
		"unknown key":    func(r *Recipe) { r.Signature.KeyID = "other" },
		"algorithm":      func(r *Recipe) { r.Signature.Alg = "rsa" },
		"unsigned":       func(r *Recipe) { r.Signature = Signature{} },
	}
	for name, mod := range tamper {
		c := signed(t, priv, key.KID)
		mod(&c)
		if v := Verify(c, []signing.Key{key}); v.Verified || v.Reason == "" {
			t.Errorf("%s: verified (%+v)", name, v)
		}
	}
	_, other := testKey("another")
	other.KID = key.KID
	if v := Verify(r, []signing.Key{other}); v.Verified {
		t.Error("verified with another key")
	}
}

func TestDecode(t *testing.T) {
	if r, err := Decode([]byte(`{"recipe":{"id":"rcp_1","title":"T"}}`)); err != nil || r.ID != "rcp_1" {
		t.Errorf("Decode = %+v, %v", r, err)
	}
	for _, bad := range []string{`{}`, `{"recipe":{}}`, `[`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestKeyCacheFetchesOnceAndRefetchesOnRotation(t *testing.T) {
	_, k1 := testKey("one")
	k2 := signing.Key{KID: "plat-next", Alg: "ed25519", PublicKey: k1.PublicKey}
	fetches := 0
	current := []signing.Key{k1}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	c := KeyCache{Path: filepath.Join(t.TempDir(), "sub", "keys.json"), TTL: time.Hour, Now: func() time.Time { return now },
		Fetch: func(context.Context, string) ([]signing.Key, error) { fetches++; return current, nil }}
	for i := 0; i < 3; i++ {
		if keys, err := c.Keys(context.Background(), "https://gw", "plat-test"); err != nil || len(keys) != 1 {
			t.Fatalf("keys = %v, %v", keys, err)
		}
	}
	if fetches != 1 {
		t.Errorf("fetches = %d, want 1", fetches)
	}
	current = []signing.Key{k1, k2}
	if keys, _ := c.Keys(context.Background(), "https://gw", "plat-next"); len(keys) != 2 || fetches != 2 {
		t.Errorf("a rotation: keys %d fetches %d", len(keys), fetches)
	}
	now = now.Add(2 * time.Hour)
	_, _ = c.Keys(context.Background(), "https://gw", "")
	if fetches != 3 {
		t.Errorf("a stale cache was used: %d fetches", fetches)
	}
	_, _ = c.Keys(context.Background(), "https://other", "")
	if fetches != 4 {
		t.Errorf("another gateway shared the cache: %d fetches", fetches)
	}
	c.Fetch = func(context.Context, string) ([]signing.Key, error) { return nil, errors.New("down") }
	now = now.Add(2 * time.Hour)
	if _, err := c.Keys(context.Background(), "https://gw", ""); err == nil {
		t.Error("a failed fetch with a stale cache")
	}
	bad := KeyCache{Path: filepath.Join(t.TempDir(), "k.json"), TTL: time.Hour, Fetch: c.Fetch}
	bad.Fetch = func(context.Context, string) ([]signing.Key, error) { return current, nil }
	if _, err := bad.Keys(context.Background(), "https://gw", ""); err != nil {
		t.Errorf("fresh cache: %v", err)
	}
}

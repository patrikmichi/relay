// Package recipes holds the shape of a tool recipe (a playbook for using a
// few tools well) and checks its signature without asking the gateway again.
package recipes

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/patrikmichi/zuano/internal/signing"
)

// KeysPath is where the gateway publishes the keys recipes are signed with.
const KeysPath = "/api/v1/recipes/keys"

// Step is one step of a recipe.
type Step struct {
	Order       int    `json:"order"`
	Kind        string `json:"kind"`
	Condition   string `json:"condition,omitempty"`
	FallbackFor int    `json:"fallbackFor,omitempty"`
	Tool        string `json:"tool"`
	Instruction string `json:"instruction"`
	// Risk and Approval are worked out for the caller when served; they are
	// never signed.
	Risk     string `json:"risk,omitempty"`
	Approval string `json:"approval,omitempty"`
}

// Pitfall is a way a step goes wrong and what to do instead.
type Pitfall struct {
	Tool       string   `json:"tool"`
	ErrorCodes []string `json:"errorCodes,omitempty"`
	Text       string   `json:"text"`
	Fix        string   `json:"fix"`
}

// ToolHint says whether a tool call can be retried.
type ToolHint struct {
	Tool             string `json:"tool"`
	Idempotency      string `json:"idempotency"`
	IdempotencyParam string `json:"idempotencyParam,omitempty"`
}

// Evidence is how well a recipe has worked.
type Evidence struct {
	SuccessRate    *float64 `json:"successRate"`
	SuccessRateLow *float64 `json:"successRateLow"`
	SampleSize     *int     `json:"sampleSize"`
	WindowDays     int      `json:"windowDays"`
	LastEval       *struct {
		At     string  `json:"at"`
		Score  float64 `json:"score"`
		RunID  string  `json:"runId"`
		Passed bool    `json:"passed"`
	} `json:"lastEval"`
}

// Signature is what vouches for a recipe's content. Signed is the exact
// content that was signed, so a copy that was altered afterwards differs from it.
type Signature struct {
	Alg         string         `json:"alg"`
	KeyID       string         `json:"keyId"`
	Signer      string         `json:"signer"`
	ContentHash string         `json:"contentHash"`
	Sig         string         `json:"sig"`
	Signed      map[string]any `json:"signed"`
}

// Recipe is a recipe as the gateway serves it.
type Recipe struct {
	ID         string   `json:"id"`
	Slug       string   `json:"slug"`
	Version    int      `json:"version"`
	Title      string   `json:"title"`
	UseCase    string   `json:"useCase"`
	Services   []string `json:"services"`
	Difficulty struct {
		Level  string `json:"level"`
		Reason string `json:"reason"`
	} `json:"difficulty"`
	PrimaryTools []string   `json:"primaryTools"`
	RelatedTools []string   `json:"relatedTools"`
	Steps        []Step     `json:"steps"`
	Pitfalls     []Pitfall  `json:"pitfalls"`
	Tools        []ToolHint `json:"tools"`
	Source       string     `json:"source"`
	Overrides    string     `json:"overrides,omitempty"`
	Status       string     `json:"status"`
	Evidence     Evidence   `json:"evidence"`
	Signature    Signature  `json:"signature"`
	URL          string     `json:"url"`
}

// shown is the part of a recipe the signature covers, as it is displayed.
func (r Recipe) shown() map[string]any {
	steps := make([]any, len(r.Steps))
	for i, s := range r.Steps {
		m := map[string]any{"order": s.Order, "kind": s.Kind, "tool": s.Tool, "instruction": s.Instruction}
		if s.Condition != "" {
			m["condition"] = s.Condition
		}
		if s.FallbackFor != 0 {
			m["fallbackFor"] = s.FallbackFor
		}
		steps[i] = m
	}
	pits := make([]any, len(r.Pitfalls))
	for i, p := range r.Pitfalls {
		m := map[string]any{"tool": p.Tool, "text": p.Text, "fix": p.Fix}
		if len(p.ErrorCodes) > 0 {
			m["errorCodes"] = strs(p.ErrorCodes)
		}
		pits[i] = m
	}
	return map[string]any{
		"id": r.ID, "version": r.Version, "title": r.Title, "useCase": r.UseCase, "services": strs(r.Services),
		"difficulty":   map[string]any{"level": r.Difficulty.Level, "reason": r.Difficulty.Reason},
		"primaryTools": strs(r.PrimaryTools), "relatedTools": strs(r.RelatedTools), "steps": steps, "pitfalls": pits,
	}
}

func strs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// Verification is the outcome of checking a recipe's signature.
type Verification struct {
	Verified bool
	Signer   string
	KeyID    string
	Reason   string
}

// Verify checks that the signature is by a published key over the signed
// content, that the content hashes to what the signature says, and that the
// recipe shown is that content. keys are the published keys.
func Verify(r Recipe, keys []signing.Key) Verification {
	s := r.Signature
	v := Verification{Signer: s.Signer, KeyID: s.KeyID}
	fail := func(format string, a ...any) Verification {
		v.Reason = fmt.Sprintf(format, a...)
		return v
	}
	if s.Sig == "" || s.KeyID == "" || s.Signed == nil {
		return fail("the recipe is not signed")
	}
	if s.Alg != "" && s.Alg != "ed25519" {
		return fail("unsupported algorithm %q", s.Alg)
	}
	signed, err := signing.CanonicalJSON(s.Signed)
	if err != nil {
		return fail("the signed content cannot be read: %v", err)
	}
	sum := sha256.Sum256(signed)
	if s.ContentHash != "" && hex.EncodeToString(sum[:]) != s.ContentHash {
		return fail("the signed content does not match its hash")
	}
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil {
		return fail("the signature is not base64")
	}
	if err := signing.VerifyMessage(keys, s.KeyID, signed, sig); err != nil {
		return fail("%v", err)
	}
	want, err := signing.CanonicalJSON(subset(s.Signed, r.shown()))
	if err != nil {
		return fail("the signed content cannot be compared: %v", err)
	}
	got, err := signing.CanonicalJSON(r.shown())
	if err != nil {
		return fail("the recipe cannot be compared: %v", err)
	}
	if !bytes.Equal(want, got) {
		return fail("the recipe shown differs from the content that was signed")
	}
	v.Verified = true
	return v
}

// subset keeps from signed only the keys that shown has, normalised through
// JSON so numbers compare equal.
func subset(signed, shown map[string]any) map[string]any {
	out := map[string]any{}
	for k := range shown {
		if v, ok := signed[k]; ok {
			out[k] = v
		}
	}
	return out
}

// ErrNoKey is returned when a key set holds no key with the signature's id.
var ErrNoKey = errors.New("no published key has that id")

// Decode reads a recipe and its envelope.
func Decode(raw []byte) (Recipe, error) {
	var doc struct {
		Recipe Recipe `json:"recipe"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return Recipe{}, err
	}
	if doc.Recipe.ID == "" {
		return Recipe{}, errors.New("the answer holds no recipe")
	}
	return doc.Recipe, nil
}

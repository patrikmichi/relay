package signing

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Headers of a signed JSON response body.
const (
	HeaderContentSignature = "X-Content-Signature"
	HeaderContentKeyID     = "X-Content-Signature-Key-Id"
	// ExitContentSignature is the process exit status of a signed document that
	// does not verify.
	ExitContentSignature = 4
)

// ContentError is a signed document that failed verification: nothing from
// it was used.
type ContentError struct{ Reason string }

func (e *ContentError) Error() string { return "signature check failed: " + e.Reason }

// ExitCode is the process exit status for the failure.
func (*ContentError) ExitCode() int { return ExitContentSignature }

// VerifyContent checks the signature a gateway puts on a JSON response: an
// ed25519 signature over the canonical JSON of the body, by a pinned key. An
// unsigned body is refused, since the caller is about to act on it.
func VerifyContent(keys []Key, h http.Header, body []byte) error {
	sig := strings.TrimSpace(h.Get(HeaderContentSignature))
	kid := strings.TrimSpace(h.Get(HeaderContentKeyID))
	if sig == "" || kid == "" {
		return &ContentError{Reason: "the response is not signed"}
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return &ContentError{Reason: "the signature is not base64"}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return &ContentError{Reason: "the signed body is not JSON"}
	}
	msg, err := CanonicalJSON(doc)
	if err != nil {
		return &ContentError{Reason: fmt.Sprintf("the signed body cannot be canonicalised: %v", err)}
	}
	if err := VerifyMessage(keys, kid, msg, raw); err != nil {
		return &ContentError{Reason: err.Error()}
	}
	return nil
}

// Package webhooksign signs and verifies webhook requests as the Standard
// Webhooks specification describes, so a request forwarded to a local server
// verifies exactly as a production delivery does.
package webhooksign

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header names of a signed request.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// DefaultTolerance is how far a timestamp may be from now.
const DefaultTolerance = 300 * time.Second

const secretPrefix = "whsec_"

// GenerateSecret returns a new signing secret, whsec_ and 32 random bytes in base64.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

func key(secret string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if err != nil || len(raw) == 0 {
		return nil, errors.New("the signing secret must be whsec_ followed by base64")
	}
	return raw, nil
}

func mac(k []byte, id string, ts int64, body []byte) string {
	h := hmac.New(sha256.New, k)
	fmt.Fprintf(h, "%s.%d.", id, ts)
	_, _ = h.Write(body)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Sign returns the headers that authenticate body as the delivery id sent at ts.
func Sign(secret, id string, ts time.Time, body []byte) (http.Header, error) {
	k, err := key(secret)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
	h.Set(HeaderSignature, "v1,"+mac(k, id, ts.Unix(), body))
	return h, nil
}

// Verify checks a request's headers against its raw body. Any of the
// space-separated signatures may match, as during a secret rotation. A zero
// tolerance disables the timestamp check.
func Verify(secret string, h http.Header, body []byte, tolerance time.Duration, now time.Time) error {
	k, err := key(secret)
	if err != nil {
		return err
	}
	id, rawTS, sigs := h.Get(HeaderID), h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if id == "" || rawTS == "" || sigs == "" {
		return errors.New("missing webhook-id, webhook-timestamp or webhook-signature")
	}
	ts, err := strconv.ParseInt(rawTS, 10, 64)
	if err != nil {
		return errors.New("webhook-timestamp is not a number")
	}
	if tolerance > 0 {
		d := now.Sub(time.Unix(ts, 0))
		if d < 0 {
			d = -d
		}
		if d > tolerance {
			return fmt.Errorf("the timestamp is %s from now, more than the tolerance of %s", d.Round(time.Second), tolerance)
		}
	}
	want := mac(k, id, ts, body)
	for _, s := range strings.Fields(sigs) {
		v, sig, ok := strings.Cut(s, ",")
		if ok && v == "v1" && hmac.Equal([]byte(sig), []byte(want)) {
			return nil
		}
	}
	return errors.New("no signature matches")
}

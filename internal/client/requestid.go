package client

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// RequestIDHeader carries a request's correlation id in both directions: zuano
// sends its own cli_ id, and the gateway answers with the req_ id it recorded.
const RequestIDHeader = "X-Request-Id"

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID encodes now and 80 random bits as a 26-character ULID.
func newULID(now time.Time, entropy io.Reader) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		return "", err
	}
	var hi, lo uint64
	for i := 0; i < 8; i++ {
		hi = hi<<8 | uint64(b[i])
		lo = lo<<8 | uint64(b[8+i])
	}
	out := make([]byte, 26)
	for i := range out {
		shift := uint(125 - 5*i)
		var v uint64
		switch {
		case shift >= 64:
			v = hi >> (shift - 64)
		case shift > 59:
			v = lo>>shift | hi<<(64-shift)
		default:
			v = lo >> shift
		}
		out[i] = crockford[v&31]
	}
	return string(out), nil
}

// NewCorrelationID returns the cli_<ulid> id zuano sends with a request, so
// the gateway can tie its records to this client call.
func NewCorrelationID() string {
	id, err := newULID(time.Now(), rand.Reader)
	if err != nil {
		return "cli_" + fmt.Sprintf("%026d", time.Now().UnixNano())
	}
	return "cli_" + id
}

var (
	lastRequestID atomic.Value
	debugMu       sync.Mutex
	debugOut      io.Writer
)

// LastRequestID is the request id of the most recent gateway response that
// carried one, or "".
func LastRequestID() string {
	id, _ := lastRequestID.Load().(string)
	return id
}

// SetDebug makes every gateway response print its request id to w; nil stops it.
func SetDebug(w io.Writer) {
	debugMu.Lock()
	defer debugMu.Unlock()
	debugOut = w
}

func recordRequestID(req *http.Request, resp *http.Response) {
	id := resp.Header.Get(RequestIDHeader)
	if id == "" {
		return
	}
	lastRequestID.Store(id)
	debugMu.Lock()
	defer debugMu.Unlock()
	if debugOut != nil {
		fmt.Fprintf(debugOut, "%s: %s %s -> %d, request id %s\n", brand.Name, req.Method, req.URL.Path, resp.StatusCode, id)
	}
}

// requestIDSuffix is the text added to an error message so a person can quote
// the id to support or to `trace`.
func requestIDSuffix(id string) string {
	if id == "" {
		return ""
	}
	return "; request id: " + id
}

// RequestIDNote is requestIDSuffix for the most recent gateway response, for
// errors built from a status line.
func RequestIDNote() string { return requestIDSuffix(textsafe.Clean(LastRequestID())) }

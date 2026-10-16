package mcpwrap

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/patrikmichi/zuano/internal/signing"
)

const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// AuditLog is a local, hash-chained log of what the shim decided: each line
// holds the hash of the one before, so removing or editing a line shows.
type AuditLog struct {
	mu   sync.Mutex
	path string
	seq  int64
	prev string
}

// OpenAudit opens (creating) the log for a server and finds its end.
func OpenAudit(dir, name string) (*AuditLog, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%q is not a usable server name", name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &AuditLog{path: filepath.Join(dir, name+".jsonl"), prev: genesisHash}
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), MaxLine)
	for sc.Scan() {
		var rec struct {
			Seq  int64  `json:"seq"`
			Hash string `json:"hash"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Hash != "" {
			l.seq, l.prev = rec.Seq, rec.Hash
		}
	}
	return l, sc.Err()
}

// Path is where the log is kept.
func (l *AuditLog) Path() string { return l.path }

func chainHash(prev string, body map[string]any) (string, error) {
	canon, err := signing.CanonicalJSON(body)
	if err != nil {
		return "", err
	}
	prevBytes, _ := hex.DecodeString(prev)
	sum := sha256.Sum256(append(prevBytes, canon...))
	return hex.EncodeToString(sum[:]), nil
}

// Append adds an event; fields hold names, decisions and digests, never
// arguments or results.
func (l *AuditLog) Append(event string, fields map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	body := map[string]any{"seq": l.seq + 1, "ts": now(), "event": event}
	for k, v := range fields {
		body[k] = v
	}
	hash, err := chainHash(l.prev, body)
	if err != nil {
		return err
	}
	body["prev_hash"], body["hash"] = l.prev, hash
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	l.seq, l.prev = l.seq+1, hash
	return nil
}

// VerifyAudit recomputes a log's chain and returns the number of events, or
// an error naming the first line that does not check out.
func VerifyAudit(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return verifyAuditReader(f)
}

func verifyAuditReader(r io.Reader) (int64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), MaxLine)
	prev, n := genesisHash, int64(0)
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var body map[string]any
		if err := dec.Decode(&body); err != nil {
			return n, fmt.Errorf("line %d is not an event: %w", n+1, err)
		}
		hash, _ := body["hash"].(string)
		gotPrev, _ := body["prev_hash"].(string)
		delete(body, "hash")
		delete(body, "prev_hash")
		want, err := chainHash(prev, body)
		switch {
		case err != nil:
			return n, err
		case gotPrev != prev:
			return n, fmt.Errorf("event %d does not continue from the one before: a line was removed or reordered", n+1)
		case want != hash:
			return n, fmt.Errorf("event %d was changed after it was written", n+1)
		}
		prev, n = hash, n+1
	}
	return n, sc.Err()
}

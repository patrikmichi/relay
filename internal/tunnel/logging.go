package tunnel

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// forbiddenLogKeys can never be written: bodies, query strings and header
// values stay out of every log line, whoever asks.
var forbiddenLogKeys = map[string]bool{
	"body": true, "query": true, "headers": true, "header": true, "value": true, "authorization": true,
	"token": true, "secret": true, "password": true, "credentials_value": true, "url": true,
}

// Logger writes one JSON line per event to stdout and optionally to a rotated
// file, and keeps the lines the dashboard should receive.
type Logger struct {
	mu      sync.Mutex
	out     io.Writer
	file    *rotatingFile
	format  string
	upload  string
	pending []json.RawMessage
	Now     func() time.Time
}

// maxUploadLines bounds what one STATUS frame carries.
const maxUploadLines = 100

// NewLogger writes to out and, when file is set, to a rotated file.
func NewLogger(out io.Writer, format, file string, maxSizeMB, maxFiles int, upload string) (*Logger, error) {
	l := &Logger{out: out, format: format, upload: upload, Now: time.Now}
	if file != "" {
		f, err := newRotatingFile(file, int64(maxSizeMB)<<20, maxFiles)
		if err != nil {
			return nil, err
		}
		l.file = f
	}
	return l, nil
}

// Close closes the log file.
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Event writes one line. Fields named like a body, query or header value are
// dropped.
func (l *Logger) Event(level, event string, fields map[string]any) {
	rec := map[string]any{"ts": l.Now().UTC().Format(time.RFC3339Nano), "level": level, "event": event}
	for k, v := range fields {
		if forbiddenLogKeys[strings.ToLower(k)] {
			continue
		}
		rec[k] = v
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line := raw
	if l.format == "text" {
		line = []byte(textLine(rec))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_, _ = l.out.Write(append(append([]byte(nil), line...), '\n'))
	}
	if l.file != nil {
		_, _ = l.file.Write(append(append([]byte(nil), line...), '\n'))
	}
	if l.wantsUpload(level, event) {
		if len(l.pending) >= maxUploadLines {
			l.pending = l.pending[1:]
		}
		l.pending = append(l.pending, raw)
	}
}

func (l *Logger) wantsUpload(level, event string) bool {
	switch l.upload {
	case "metadata":
		return event == "exchange" || level == "warn" || level == "error"
	case "errors":
		return level == "warn" || level == "error"
	}
	return false
}

// TakeUpload returns the lines to send with the next STATUS and forgets them.
func (l *Logger) TakeUpload() []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.pending
	l.pending = nil
	return out
}

func textLine(rec map[string]any) string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		if k != "ts" && k != "level" && k != "event" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%v %v %v", rec["ts"], rec["level"], rec["event"])
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, rec[k])
	}
	return b.String()
}

// rotatingFile appends to path and, past max bytes, renames it to path.1 and
// shifts the older ones, keeping keep files.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func newRotatingFile(path string, max int64, keep int) (*rotatingFile, error) {
	r := &rotatingFile{path: path, max: max, keep: keep}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) rotate() error {
	_ = r.f.Close()
	_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	if r.keep >= 1 {
		if err := os.Rename(r.path, r.path+".1"); err != nil {
			return err
		}
	} else {
		_ = os.Remove(r.path)
	}
	return r.open()
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

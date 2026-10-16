// Package mcpwrap runs a local MCP server behind a governing shim: it speaks
// MCP over stdio to the agent and to the server, serves only pinned tool
// definitions, asks a governor before each call and screens each result.
package mcpwrap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"unicode/utf8"
)

// MaxLine is the longest message the shim relays.
const MaxLine = 16 << 20

// LineReader reads newline-delimited JSON-RPC messages.
type LineReader struct{ r *bufio.Reader }

// NewLineReader wraps r.
func NewLineReader(r io.Reader) *LineReader { return &LineReader{bufio.NewReaderSize(r, 64<<10)} }

// Next returns the next non-empty message, or io.EOF.
func (l *LineReader) Next() ([]byte, error) {
	for {
		var line []byte
		for {
			chunk, err := l.r.ReadSlice('\n')
			line = append(line, chunk...)
			if len(line) > MaxLine {
				return nil, fmt.Errorf("a message is longer than %d bytes", MaxLine)
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) == 0 {
				return nil, io.EOF
			}
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !utf8.Valid(line) {
			return nil, errors.New("a message is not valid UTF-8")
		}
		return line, nil
	}
}

// LineWriter writes messages one per line, safe for concurrent use.
type LineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewLineWriter wraps w.
func NewLineWriter(w io.Writer) *LineWriter { return &LineWriter{w: w} }

// Raw sends an already encoded message.
func (l *LineWriter) Raw(msg []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(append(bytes.TrimRight(msg, "\r\n"), '\n')); err != nil {
		return err
	}
	return nil
}

// JSON encodes and sends v.
func (l *LineWriter) JSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return l.Raw(raw)
}

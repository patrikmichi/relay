package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	defaultControlTimeout = 30 * time.Second
	defaultLongTimeout    = 150 * time.Second
	timeoutEnvVar         = "ZUANO_TIMEOUT"
)

type requestKind int

const (
	// controlRequest covers short gateway calls: discovery, whoami, tokens, logout.
	controlRequest requestKind = iota
	// toolCallRequest covers tools/call, which may run for minutes.
	toolCallRequest
	// transferRequest covers publish uploads and sync downloads.
	transferRequest
)

// TimeoutError reports that a gateway request hit zuano's client-side limit.
type TimeoutError struct {
	Limit  time.Duration
	Source string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("gateway request timed out after %s (%s); raise it with --timeout <duration> or %s, or use 0 for no limit", e.Limit, e.Source, timeoutEnvVar)
}

func (e *TimeoutError) Unwrap() error { return context.DeadlineExceeded }

type timeoutOverride struct {
	limit  time.Duration
	source string
}

var timeoutFlag *timeoutOverride

// SetTimeoutFlag records an explicit root --timeout value; it overrides
// ZUANO_TIMEOUT and both defaults.
func SetTimeoutFlag(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("--timeout must not be negative")
	}
	timeoutFlag = &timeoutOverride{limit: d, source: "set by --timeout"}
	return nil
}

func resetTimeoutFlag() { timeoutFlag = nil }

// requestLimit returns the client-side limit for kind and a description of
// where it came from. A zero limit means no client-side limit.
func requestLimit(kind requestKind) (time.Duration, string, error) {
	if timeoutFlag != nil {
		return timeoutFlag.limit, timeoutFlag.source, nil
	}
	if raw := os.Getenv(timeoutEnvVar); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return 0, "", fmt.Errorf("invalid %s %q: expected a non-negative duration such as 90s or 5m", timeoutEnvVar, raw)
		}
		return d, "set by " + timeoutEnvVar, nil
	}
	switch kind {
	case toolCallRequest:
		return defaultLongTimeout, "default limit for tool calls", nil
	case transferRequest:
		return defaultLongTimeout, "default limit for uploads and downloads", nil
	}
	return defaultControlTimeout, "default limit for gateway requests", nil
}

// requestContext derives a context bounded by the limit for kind. When the
// limit fires, context.Cause(ctx) is a *TimeoutError; see timeoutCause.
func requestContext(parent context.Context, kind requestKind) (context.Context, context.CancelFunc, error) {
	parent = nonNilContext(parent)
	limit, source, err := requestLimit(kind)
	if err != nil {
		return nil, nil, err
	}
	if limit == 0 {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, nil
	}
	ctx, cancel := context.WithTimeoutCause(parent, limit, &TimeoutError{Limit: limit, Source: source})
	return ctx, cancel, nil
}

// timeoutCause replaces err with the *TimeoutError that ended ctx, so the
// message names the limit that fired and the exit code reflects a timeout.
func timeoutCause(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if te, ok := context.Cause(ctx).(*TimeoutError); ok {
		return te
	}
	return err
}

// ExitError ends the process with a specific status. An empty Msg means the
// command already printed what the caller needs to see.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// ExitCode maps a command error to the process exit status: the code of an
// ExitError, 17 when a gateway request or a --wait hit its time limit, 1 for
// any other failure. 4 is only a signature or hash that does not verify.
func ExitCode(err error) int {
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return exitWaitTimeout
	}
	return 1
}

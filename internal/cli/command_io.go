package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func outputWriter(writers []io.Writer) io.Writer {
	if len(writers) > 0 && writers[0] != nil {
		return writers[0]
	}
	return os.Stdout
}

// Binds legacy workflow interfaces to Cobra cancellation. Production clients
// implement context-aware HTTP methods; old in-memory test doubles still work.
type commandDoer struct {
	base any
	ctx  context.Context
}

func (d commandDoer) Post(path, contentType string, body io.Reader) (*http.Response, error) {
	ctx, cancel, err := requestContext(d.ctx, transferRequest)
	if err != nil {
		return nil, err
	}
	if c, ok := d.base.(interface {
		PostContext(context.Context, string, string, io.Reader) (*http.Response, error)
	}); ok {
		response, err := c.PostContext(ctx, path, contentType, body)
		return responseWithCancel(response, timeoutCause(ctx, err), cancel)
	}
	cancel()
	return d.base.(publishDoer).Post(path, contentType, body)
}
func (d commandDoer) Get(path string) (*http.Response, error) {
	ctx, cancel, err := requestContext(d.ctx, transferRequest)
	if err != nil {
		return nil, err
	}
	if c, ok := d.base.(interface {
		GetContext(context.Context, string) (*http.Response, error)
	}); ok {
		response, err := c.GetContext(ctx, path)
		return responseWithCancel(response, timeoutCause(ctx, err), cancel)
	}
	cancel()
	if legacy, ok := d.base.(syncDoer); ok {
		return legacy.Get(path)
	}
	return nil, fmt.Errorf("client does not support GET")
}
func waitForPublish(d publishStatusDoer, duration time.Duration) error {
	if bound, ok := d.(commandDoer); ok {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-bound.ctx.Done():
			return bound.ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	sleepFn(duration)
	return nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body cancelBody) Close() error { defer body.cancel(); return body.ReadCloser.Close() }
func responseWithCancel(response *http.Response, err error, cancel context.CancelFunc) (*http.Response, error) {
	if err != nil {
		cancel()
		return response, err
	}
	response.Body = cancelBody{response.Body, cancel}
	return response, nil
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

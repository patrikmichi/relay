package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/signing"
)

// gatewayReply is a response with its headers, for routes whose answer is
// signed or conditional.
type gatewayReply struct {
	Status int
	Header http.Header
	Body   []byte
}

// gatewayExchange sends one control request. Refusals the client recognises
// (billing, quota, step-up) come back as their own typed errors.
func gatewayExchange(parent context.Context, c *client.Client, method, path string, body any, headers map[string]string) (gatewayReply, error) {
	ctx, cancel, err := requestContext(parent, controlRequest)
	if err != nil {
		return gatewayReply{}, err
	}
	defer cancel()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return gatewayReply{}, fmt.Errorf("marshal request body: %w", err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.GatewayURL+path, payload)
	if err != nil {
		return gatewayReply{}, fmt.Errorf("create request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return gatewayReply{}, timeoutCause(ctx, fmt.Errorf("%s %s: %w", method, path, err))
	}
	defer resp.Body.Close()
	raw, err := readLimitedResponse(resp.Body, maxApprovalBody)
	if err != nil {
		return gatewayReply{}, fmt.Errorf("read response: %w", err)
	}
	return gatewayReply{resp.StatusCode, resp.Header, raw}, nil
}

// ok reports a 2xx answer.
func (r gatewayReply) ok() bool { return r.Status >= 200 && r.Status < 300 }

// failure is the error for a non-2xx answer, in the gateway's own words.
func (r gatewayReply) failure() error { return gatewayStatusError(r.Status, r.Body) }

// code is the machine-readable error of a failed answer.
func (r gatewayReply) code() string {
	var doc struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &doc)
	return doc.Error
}

// verify checks the content signature of the body against the keys pinned
// for the gateway, pinning them on first use.
func (r gatewayReply) verify(ctx context.Context, c *client.Client) error {
	origin, err := config.NormalizeGatewayURL(c.GatewayURL)
	if err != nil {
		return err
	}
	keys, err := config.PinnedSigningKeys(origin)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(keys) == 0 {
		pinSigningKeys(ctx, origin, io.Discard)
		if keys, err = config.PinnedSigningKeys(origin); err != nil {
			return fmt.Errorf("load config: %w", err)
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("no signing key is pinned for this gateway, so a signed answer cannot be checked; run `%s`", brand.Cmd("config trust-key"))
	}
	return signing.VerifyContent(keys, r.Header, r.Body)
}

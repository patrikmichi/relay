package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// gatewayJSON sends one control request and decodes a 200 response into out
// (when out is not nil). It returns the raw body so --json can print it
// unchanged. A refusal the gateway client recognises (billing, quota,
// step-up) comes back as its own typed error.
func gatewayJSON(ctx context.Context, c *client.Client, method, path string, body, out any) ([]byte, error) {
	status, raw, err := approvalsDo(ctx, c, controlRequest, method, path, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusAccepted && status != http.StatusNoContent {
		return nil, gatewayStatusError(status, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return raw, nil
}

// gatewayWrite sends a change. An agent session's write may be held for
// approval; that is reported like a held call and exits 12. A refusal with a
// code is returned as the gateway's own sentence.
func gatewayWrite(cmd *cobra.Command, c *client.Client, jsonOut bool, method, path string, body, out any) ([]byte, error) {
	status, raw, err := approvalsDo(cmd.Context(), c, controlRequest, method, path, body)
	if err != nil {
		return nil, err
	}
	if held := pendingFromREST(status, raw); held != nil {
		if jsonOut {
			printRaw(cmd.OutOrStdout(), raw)
		}
		return nil, handlePending(cmd.Context(), c, held, jsonOut, callOpts{}, cmd.OutOrStdout())
	}
	if status < 200 || status > 299 {
		return nil, gatewayStatusError(status, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return raw, nil
}

// gatewayStatusError turns a failed response into an error that carries the
// gateway's own sentence.
func gatewayStatusError(status int, raw []byte) error {
	var doc struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &doc)
	msg := doc.Message
	if msg == "" {
		msg = doc.Error
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if status == http.StatusUnauthorized {
		return errTokenRevoked
	}
	return fmt.Errorf("%s (HTTP %d)%s", textsafe.Clean(msg), status, client.RequestIDNote())
}

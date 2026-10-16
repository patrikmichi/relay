package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/textsafe"
)

// The step-up wait is a variable so tests can shorten it.
var (
	stepUpPollInterval = 2 * time.Second
	stepUpTimeout      = 5 * time.Minute
	stepUpOut          = io.Writer(os.Stderr)
	// stepUpInteractive is true when a person can be sent to a browser: not an
	// agent session and a terminal on stdin.
	stepUpInteractive = func() bool { return !config.AgentMode() && canPrompt(os.Stdin) }
)

func init() { client.SetDefaultStepUpHandler(completeStepUp) }

// completeStepUp sends the person to the gateway's step-up page, waits for
// the passkey confirmation there and returns the id the retried request
// carries in X-Step-Up. Without a person at the keyboard it stops with exit
// status 14 and the link for a human to open.
func completeStepUp(ctx context.Context, ch client.StepUpChallenge, c *client.Client) (string, error) {
	link, err := stepUpLink(ch)
	if err != nil {
		return "", err
	}
	if !stepUpInteractive() {
		return "", &client.StepUpRequiredError{ID: ch.ID, URL: link}
	}
	fmt.Fprintln(stepUpOut, "This action needs your confirmation with a passkey.")
	presentLink(stepUpOut, link, true, "Confirm it there")
	fmt.Fprintf(stepUpOut, "Waiting up to %s...\n", stepUpTimeout)

	deadline := time.Now().Add(stepUpTimeout)
	for {
		status, err := stepUpStatus(ctx, c, ch.ID)
		if err != nil {
			return "", err
		}
		switch status {
		case "approved":
			return ch.ID, nil
		case "denied":
			return "", errors.New("the confirmation was declined; nothing was done")
		case "expired":
			return "", errors.New("the confirmation expired before it was completed; run the command again")
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no confirmation within %s; run the command again", stepUpTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(stepUpPollInterval):
		}
	}
}

// stepUpLink returns the challenge's link when it is a page on the gateway
// the request went to. A link anywhere else could send a person, and the
// passkey prompt they would trust, to another site.
func stepUpLink(ch client.StepUpChallenge) (string, error) {
	if ch.ID == "" || ch.URL == "" {
		return "", errors.New("the gateway asked for a step-up confirmation without a link to complete it")
	}
	link, err := url.Parse(ch.URL)
	base, baseErr := url.Parse(ch.Gateway)
	if err != nil || baseErr != nil || link.Scheme != base.Scheme || link.Host != base.Host || link.User != nil {
		return "", fmt.Errorf("the gateway's step-up link is not a page on %s; refusing to open it", textsafe.Clean(ch.Gateway))
	}
	return link.String(), nil
}

// stepUpStatus reads GET /api/stepup/{id}: {"status":"pending|approved|denied|expired"}.
func stepUpStatus(ctx context.Context, c *client.Client, id string) (string, error) {
	reqCtx, cancel, err := requestContext(ctx, controlRequest)
	if err != nil {
		return "", err
	}
	defer cancel()
	resp, err := c.GetContext(reqCtx, "/api/stepup/"+url.PathEscape(id))
	if err != nil {
		return "", fmt.Errorf("check the step-up confirmation: %w", err)
	}
	defer resp.Body.Close()
	body, err := readLimitedResponse(resp.Body, maxApprovalBody)
	if err != nil {
		return "", fmt.Errorf("read the step-up status: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("check the step-up confirmation: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("decode the step-up status: %w", err)
	}
	return strings.ToLower(doc.Status), nil
}

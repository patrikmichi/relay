package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
	"github.com/patrikmichi/zuano/internal/transcripts"
)

// Exit statuses of `zuano skill from-session` beyond the shared ones.
const (
	exitSessionUsage    = 2
	exitSessionTooLarge = 22
)

const sessionAPI = "/api/skills/from-session"

// consentStatement is the text a person accepts before a transcript leaves
// the machine; the gateway owns the words and their version.
type consentStatement struct {
	Version string `json:"statementVersion"`
	Text    string `json:"text"`
}

type captureReport struct {
	CaptureID string `json:"captureId"`
	Redaction struct {
		Counts       map[string]int `json:"counts"`
		Placeholders int            `json:"placeholders"`
	} `json:"redaction"`
	Turns     int    `json:"turns"`
	ToolCalls int    `json:"toolCalls"`
	ExpiresAt string `json:"expiresAt"`
}

type proposalDoc struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Instructions string          `json:"instructions"`
	Connectors   []string        `json:"connectors"`
	Cases        json.RawMessage `json:"cases"`
	Warnings     []string        `json:"warnings"`
}

func (p proposalDoc) caseCount() int {
	var cases []json.RawMessage
	_ = json.Unmarshal(p.Cases, &cases)
	return len(cases)
}

type extractionDoc struct {
	ID            string      `json:"id"`
	Status        string      `json:"status"`
	Error         string      `json:"error"`
	Proposal      proposalDoc `json:"proposal"`
	InjectionFlag bool        `json:"injectionFlag"`
}

// sessionPollDelay is how long to wait between checks of an extraction, and
// extractionWait how long --wait follows one.
var (
	sessionPollDelay = 2 * time.Second
	extractionWait   = 15 * time.Minute
)

func fetchConsent(ctx context.Context, c *client.Client) (consentStatement, error) {
	var doc consentStatement
	_, err := gatewayJSON(ctx, c, http.MethodGet, sessionAPI+"/consent", nil, &doc)
	if err == nil && doc.Version == "" {
		err = errors.New("the gateway did not state which consent text applies")
	}
	return doc, err
}

// uploadCapture sends the request and turns the two refusals this command
// owns into their exit statuses.
func uploadCapture(ctx context.Context, c *client.Client, body map[string]any) (captureReport, error) {
	r, err := gatewayExchange(ctx, c, http.MethodPost, sessionAPI+"/captures", body, nil)
	if err != nil {
		return captureReport{}, err
	}
	switch {
	case r.Status == http.StatusRequestEntityTooLarge:
		return captureReport{}, &ExitError{Code: exitSessionTooLarge, Msg: "the session is too large for your plan; trim it with --from-turn and --to-turn"}
	case r.Status == http.StatusUnprocessableEntity:
		msg := gatewayMessage(r.Body)
		if msg == "" {
			msg = "the gateway could not read the session"
		}
		return captureReport{}, &ExitError{Code: exitSessionUsage, Msg: msg}
	case !r.ok():
		return captureReport{}, r.failure()
	}
	var rep captureReport
	if err := json.Unmarshal(r.Body, &rep); err != nil || rep.CaptureID == "" {
		return captureReport{}, errors.New("the gateway did not return a capture id")
	}
	return rep, nil
}

func startExtraction(cmd *cobra.Command, c *client.Client, captureID string, body map[string]any) (string, error) {
	var res struct {
		ExtractionID string `json:"extractionId"`
	}
	if _, err := gatewayWrite(cmd, c, false, http.MethodPost, sessionAPI+"/captures/"+url.PathEscape(captureID)+"/extract", body, &res); err != nil {
		return "", err
	}
	if res.ExtractionID == "" {
		return "", errors.New("the gateway did not return an extraction id")
	}
	return res.ExtractionID, nil
}

// waitExtraction polls until the proposal is ready, the extraction failed or
// the time ends.
func waitExtraction(ctx context.Context, c *client.Client, id string, total time.Duration) (extractionDoc, error) {
	deadline := time.Now().Add(total)
	for {
		var doc struct {
			Extraction extractionDoc `json:"extraction"`
		}
		if _, err := gatewayJSON(ctx, c, http.MethodGet, sessionAPI+"/extractions/"+url.PathEscape(id), nil, &doc); err != nil {
			return extractionDoc{}, err
		}
		switch doc.Extraction.Status {
		case "ready":
			return doc.Extraction, nil
		case "failed", "superseded":
			msg := textsafe.Clean(doc.Extraction.Error)
			if msg == "" {
				msg = doc.Extraction.Status
			}
			return doc.Extraction, fmt.Errorf("extraction %s: %s", textsafe.Clean(id), msg)
		}
		if time.Now().After(deadline) {
			return doc.Extraction, &ExitError{Code: exitPending, Msg: fmt.Sprintf("extraction %s is still running; check it with `zuano skill from-session show`", textsafe.Clean(id))}
		}
		select {
		case <-ctx.Done():
			return extractionDoc{}, ctx.Err()
		case <-time.After(sessionPollDelay):
		}
	}
}

// sessionUpload is the request for a normalized, redacted local session.
func sessionUpload(s *transcripts.Session, consentVersion string, turnRange [2]int) map[string]any {
	return map[string]any{
		"source": s.Source, "session": s, "clientRedacted": true,
		"consent":   map[string]any{"statementVersion": consentVersion},
		"turnRange": map[string]any{"from": turnRange[0], "to": turnRange[1]},
	}
}

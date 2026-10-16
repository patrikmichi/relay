package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/patrikmichi/relay/internal/textsafe"
)

// Process exit statuses for the gateway refusals every command shares.
const (
	ExitQuotaExceeded = 13
	ExitStepUp        = 14
	// ExitPlanRequired is a feature or limit the plan does not include.
	ExitPlanRequired = 30
	// ExitUsageExhausted is a metered allowance or spend cap that is used up.
	ExitUsageExhausted = 31
	ExitBillingDown    = 32
	ExitSuspended      = 33
)

// maxErrorBody bounds how much of a refusal body is read to classify it.
const maxErrorBody = 64 << 10

// Entitlement codes a gateway answers with when billing refuses a request.
const (
	CodePlanRequired        = "plan_required"
	CodeLimitReached        = "limit_reached"
	CodeQuotaExceeded       = "quota_exceeded"
	CodeSpendCapReached     = "spend_cap_reached"
	CodeBillingUnavailable  = "billing_unavailable"
	CodeOrgSuspended        = "org_suspended"
	CodeVerificationNeeded  = "verification_required"
	CodeSeatSuspended       = "seat_suspended"
	codeStepUpRequired      = "step_up_required"
	codeReauthRequired      = "reauth_required"
	defaultEntitlementError = "the request is not covered by the organisation's plan"
)

// EntitlementError is a refusal by the gateway's billing gate: HTTP 402, 403
// for a suspended org or seat, and 503 when billing cannot be read.
type EntitlementError struct {
	Status       int
	Code         string
	Message      string
	Feature      string
	Limit        string
	Meter        string
	Current      string
	Max          string
	CurrentPlan  string
	RequiredPlan string
	UpgradeURL   string
	ResetsAt     string
	RetryAfter   string
}

// ExitCode is the process exit status for the refusal.
func (e *EntitlementError) ExitCode() int {
	switch e.Code {
	case CodeQuotaExceeded, CodeSpendCapReached:
		return ExitUsageExhausted
	case CodeBillingUnavailable:
		return ExitBillingDown
	case CodeOrgSuspended, CodeSeatSuspended:
		return ExitSuspended
	}
	return ExitPlanRequired
}

func (e *EntitlementError) Error() string {
	var b strings.Builder
	msg := textsafe.Clean(e.Message)
	if msg == "" {
		msg = defaultEntitlementError
	}
	b.WriteString(msg)
	fmt.Fprintf(&b, " (%s", e.Code)
	if e.CurrentPlan != "" {
		fmt.Fprintf(&b, ", plan %s", textsafe.Clean(e.CurrentPlan))
	}
	if e.RequiredPlan != "" {
		fmt.Fprintf(&b, ", needs %s", textsafe.Clean(e.RequiredPlan))
	}
	b.WriteString(")")
	if e.ResetsAt != "" {
		fmt.Fprintf(&b, "; resets %s", textsafe.Clean(e.ResetsAt))
	}
	if e.RetryAfter != "" {
		fmt.Fprintf(&b, "; retry after %ss", textsafe.Clean(e.RetryAfter))
	}
	if e.UpgradeURL != "" {
		fmt.Fprintf(&b, "\nUpgrade: %s", textsafe.Clean(e.UpgradeURL))
	}
	return b.String()
}

// QuotaExceededError is the 429 a gateway answers with when an organisation
// used up a metered quota.
type QuotaExceededError struct {
	Metric   string
	Limit    string
	Message  string
	ResetsAt string
}

// ExitCode is the process exit status for the refusal.
func (e *QuotaExceededError) ExitCode() int { return ExitQuotaExceeded }

func (e *QuotaExceededError) Error() string {
	msg := textsafe.Clean(e.Message)
	if msg == "" {
		msg = "quota exceeded"
	}
	var detail []string
	if e.Metric != "" {
		detail = append(detail, "metric "+textsafe.Clean(e.Metric))
	}
	if e.Limit != "" {
		detail = append(detail, "limit "+textsafe.Clean(e.Limit))
	}
	if e.ResetsAt != "" {
		detail = append(detail, "resets "+textsafe.Clean(e.ResetsAt))
	}
	if len(detail) == 0 {
		return msg
	}
	return msg + " (" + strings.Join(detail, ", ") + ")"
}

// StepUpRequiredError is the 401 a gateway answers with when an action needs
// a passkey confirmation from a person first.
type StepUpRequiredError struct {
	ID  string
	URL string
}

// ExitCode is the process exit status for the refusal.
func (e *StepUpRequiredError) ExitCode() int { return ExitStepUp }

func (e *StepUpRequiredError) Error() string {
	msg := "this action needs a passkey confirmation from a person"
	if e.URL != "" {
		msg += ": open " + textsafe.Clean(e.URL)
	}
	return msg
}

// refusalBody is the union of the fields the gateway's refusals carry. Number
// fields are raw because the gateway sends a limit key as text for a plan
// limit and as a number for a quota.
type refusalBody struct {
	Error        string          `json:"error"`
	Message      string          `json:"message"`
	Feature      string          `json:"feature"`
	Limit        json.RawMessage `json:"limit"`
	Meter        string          `json:"meter"`
	Metric       string          `json:"metric"`
	Current      json.RawMessage `json:"current"`
	Max          json.RawMessage `json:"max"`
	CurrentPlan  string          `json:"current_plan"`
	RequiredPlan string          `json:"required_plan"`
	UpgradeURL   string          `json:"upgrade_url"`
	ResetsAt     string          `json:"resets_at"`
	StepUpURL    string          `json:"step_up_url"`
	StepUpID     string          `json:"step_up_id"`
}

func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// classify turns a gateway refusal into a typed error and closes the
// response. Any other response is returned intact.
func classify(resp *http.Response) (*http.Response, error) {
	if refusal := refusalOf(resp); refusal != nil {
		_ = resp.Body.Close()
		return nil, refusal
	}
	return resp, nil
}

// refusalOf recognises a refusal in a 401, 402, 403, 429 or 503 response. It
// reads at most maxErrorBody bytes of the body and puts them back, so the
// caller can still read the whole body of a response that is not a refusal.
func refusalOf(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
	default:
		return nil
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	orig := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), orig), orig}
	if err != nil {
		return nil
	}

	var b refusalBody
	if json.Unmarshal(head, &b) != nil {
		return nil
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized && b.Error == codeStepUpRequired:
		return &StepUpRequiredError{ID: b.StepUpID, URL: b.StepUpURL}
	case resp.StatusCode == http.StatusTooManyRequests && b.Error == CodeQuotaExceeded:
		return &QuotaExceededError{Metric: firstNonEmpty(b.Metric, b.Meter), Limit: rawText(b.Limit), Message: b.Message, ResetsAt: b.ResetsAt}
	case isEntitlementCode(resp.StatusCode, b.Error):
		return &EntitlementError{
			Status: resp.StatusCode, Code: b.Error, Message: b.Message, Feature: b.Feature,
			Limit: rawText(b.Limit), Meter: b.Meter, Current: rawText(b.Current), Max: rawText(b.Max),
			CurrentPlan: b.CurrentPlan, RequiredPlan: b.RequiredPlan, UpgradeURL: b.UpgradeURL,
			ResetsAt: b.ResetsAt, RetryAfter: resp.Header.Get("Retry-After"),
		}
	}
	return nil
}

func isEntitlementCode(status int, code string) bool {
	switch status {
	case http.StatusPaymentRequired:
		switch code {
		case CodePlanRequired, CodeLimitReached, CodeQuotaExceeded, CodeSpendCapReached, CodeVerificationNeeded:
			return true
		}
	case http.StatusForbidden:
		return code == CodeOrgSuspended || code == CodeSeatSuspended
	case http.StatusServiceUnavailable:
		return code == CodeBillingUnavailable
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

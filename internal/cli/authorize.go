package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/textsafe"
	"github.com/spf13/cobra"
)

var ErrServiceAuthorizationUnsupported = errors.New("gateway does not support this service authorization request")

const serviceAuthorizationPath = "/api/cli/services/authorize"

var authorizationPollInterval = 3 * time.Second

type serviceAuthorizationRequest struct {
	Service string   `json:"service"`
	Account string   `json:"account,omitempty"`
	Scopes  []string `json:"scopes"`
}
type serviceAuthorizationResult struct {
	Service           string   `json:"service"`
	Account           *string  `json:"account"`
	Scopes            []string `json:"scopes"`
	State             string   `json:"state"`
	AccountLinked     bool     `json:"accountLinked"`
	MissingScopes     []string `json:"missingScopes"`
	AuthorizationPath string   `json:"authorizationPath"`
	ScopeKind         string   `json:"scopeKind"`
}

// AuthorizeCmd requests gateway-tool grants and waits for verified account linkage.
func AuthorizeCmd() *cobra.Command {
	var gateway, account string
	var scopes []string
	var timeout time.Duration
	cmd := &cobra.Command{Use: "authorize <service> --tool <tool> [--account <label>]", Short: "Request and verify service-specific gateway tool access", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(scopes) == 0 {
			return fmt.Errorf("at least one --tool gateway tool name is required")
		}
		if timeout <= 0 || timeout > 30*time.Minute {
			return fmt.Errorf("--wait-timeout must be greater than zero and at most 30m")
		}
		scopes = uniqueSorted(scopes)
		request := serviceAuthorizationRequest{args[0], account, scopes}
		g, err := resolveGatewayURLOrFailClosed(gateway)
		if err != nil {
			return err
		}
		c, err := client.Resolve(g)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		body, err := json.Marshal(request)
		if err != nil {
			return err
		}
		response, err := c.PostContext(ctx, serviceAuthorizationPath, "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("request service authorization: %w", err)
		}
		result, err := readServiceAuthorization(response, request)
		if err != nil {
			return err
		}
		if result.State != "authorized" {
			if result.AuthorizationPath != "/admin" && result.AuthorizationPath != "/settings/connections" {
				return fmt.Errorf("invalid authorization navigation path")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Requested %s tool access: %s\nComplete administrator approval and connect account at %s%s\n", request.Service, strings.Join(request.Scopes, ", "), g, result.AuthorizationPath)
		}
		query := url.Values{"service": {request.Service}, "scope": request.Scopes}
		if account != "" {
			query.Set("account", account)
		}
		failures := 0
		for result.State != "authorized" {
			// Approval and account linking take a person's time, so a
			// passing network error, 429 or 5xx must not end the wait: back
			// off and ask again.
			delay := authorizationPollInterval << min(failures, 5)
			timer := time.NewTimer(min(delay, maxAuthorizationBackoff))
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("service authorization not completed: %w", ctx.Err())
			case <-timer.C:
			}
			var pollErr error
			response, err = c.GetContext(ctx, serviceAuthorizationPath+"?"+query.Encode())
			if err != nil {
				pollErr = fmt.Errorf("verify service authorization: %w", err)
			} else {
				var next serviceAuthorizationResult
				if next, pollErr = readServiceAuthorization(response, request); pollErr == nil {
					result, failures = next, 0
					continue
				}
			}
			var transient *transientAuthorizationError
			if ctx.Err() != nil || failures >= maxAuthorizationFailures || (err == nil && !errors.As(pollErr, &transient)) {
				return pollErr
			}
			failures++
			fmt.Fprintf(cmd.ErrOrStderr(), "%v; retrying (%d/%d)\n", pollErr, failures, maxAuthorizationFailures)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Authorized gateway tools for %s: %s (account connection verified)\n", request.Service, strings.Join(request.Scopes, ", "))
		return err
	}}
	cmd.Flags().StringVar(&gateway, "gateway-url", "", "Gateway URL override")
	cmd.Flags().StringVar(&account, "account", "", "Account connection label")
	cmd.Flags().StringSliceVar(&scopes, "tool", nil, "Gateway tool name to authorize (repeatable or comma-separated)")
	cmd.Flags().DurationVar(&timeout, "wait-timeout", 10*time.Minute, "Maximum time to wait for approval and account connection")
	return cmd
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

const (
	maxAuthorizationFailures = 5
	maxAuthorizationBackoff  = 30 * time.Second
)

// transientAuthorizationError is a 429 or 5xx: the request may succeed when asked again.
type transientAuthorizationError struct{ status int }

func (e *transientAuthorizationError) Error() string {
	return fmt.Sprintf("service authorization temporarily unavailable (HTTP %d)", e.status)
}

func readServiceAuthorization(response *http.Response, request serviceAuthorizationRequest) (serviceAuthorizationResult, error) {
	defer response.Body.Close()
	var result serviceAuthorizationResult
	switch status := response.StatusCode; {
	case status == 404 || status == 405:
		return result, ErrServiceAuthorizationUnsupported
	case status == 401:
		return result, errTokenRevoked
	case status == 403:
		return result, fmt.Errorf("service authorization denied (HTTP 403): the account may not request %s access", request.Service)
	case status == 429 || status >= 500:
		return result, &transientAuthorizationError{status}
	case status == 400 || status == 409 || status == 422:
		body, _ := readLimitedResponse(response.Body, 1<<16)
		msg := gatewayMessage(body)
		if msg == "" {
			msg = "the gateway refused the request"
		}
		return result, fmt.Errorf("service authorization rejected (HTTP %d): %s", status, textsafe.Clean(msg))
	case status == 200:
	default:
		return result, fmt.Errorf("service authorization unavailable (HTTP %d)", status)
	}
	body, err := readLimitedResponse(response.Body, 1<<20)
	if err != nil {
		return result, fmt.Errorf("authorization response unreadable or too large")
	}
	if json.Unmarshal(body, &result) != nil {
		return result, fmt.Errorf("invalid service authorization response")
	}
	account := ""
	if result.Account != nil {
		account = *result.Account
	}
	if result.Service != request.Service || account != request.Account || result.ScopeKind != "gateway_tool" || !reflect.DeepEqual(uniqueSorted(result.Scopes), uniqueSorted(request.Scopes)) {
		return result, fmt.Errorf("authorization response does not match requested service, account and scopes")
	}
	if result.State != "pending" && result.State != "authorized" {
		return result, fmt.Errorf("service authorization denied or expired")
	}
	if result.State == "authorized" && (!result.AccountLinked || len(result.MissingScopes) != 0) {
		return result, fmt.Errorf("gateway did not confirm all required grants and account linkage")
	}
	return result, nil
}

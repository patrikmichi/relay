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
	cmd := &cobra.Command{Use: "authorize <service> --scope <tool> [--account <label>]", Short: "Request and verify service-specific gateway tool access", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(scopes) == 0 {
			return fmt.Errorf("at least one --scope gateway tool name is required")
		}
		if timeout <= 0 || timeout > 30*time.Minute {
			return fmt.Errorf("--timeout must be greater than zero and at most 30m")
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
		for result.State != "authorized" {
			timer := time.NewTimer(authorizationPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("service authorization not completed: %w", ctx.Err())
			case <-timer.C:
			}
			response, err = c.GetContext(ctx, serviceAuthorizationPath+"?"+query.Encode())
			if err != nil {
				return fmt.Errorf("verify service authorization: %w", err)
			}
			result, err = readServiceAuthorization(response, request)
			if err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Authorized gateway tools for %s: %s (account connection verified)\n", request.Service, strings.Join(request.Scopes, ", "))
		return err
	}}
	cmd.Flags().StringVar(&gateway, "gateway-url", "", "Gateway URL override")
	cmd.Flags().StringVar(&account, "account", "", "Account connection label")
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "Gateway tool name to authorize (repeatable or comma-separated)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Maximum time to wait for approval and account connection")
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
func readServiceAuthorization(response *http.Response, request serviceAuthorizationRequest) (serviceAuthorizationResult, error) {
	defer response.Body.Close()
	var result serviceAuthorizationResult
	switch response.StatusCode {
	case 404, 405:
		return result, ErrServiceAuthorizationUnsupported
	case 401, 403:
		return result, fmt.Errorf("service authorization denied (HTTP %d)", response.StatusCode)
	case 200:
	default:
		return result, fmt.Errorf("service authorization unavailable (HTTP %d)", response.StatusCode)
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

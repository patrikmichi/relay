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

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
)

var envPrefix = strings.ToUpper(brand.Name)

// errNoCICredentials means a CI job has nothing to sign in with.
var errNoCICredentials = errors.New("this CI job has no OIDC token to sign in with (a pull request from a fork gets none)")

// ciHTTP is the client CI sign-in uses; a variable for tests.
var ciHTTP = client.NoRedirectHTTPClient

// ciIDToken returns the job's OIDC token for audience: GitHub Actions' token
// endpoint, or the variable a GitLab job declares under id_tokens.
func ciIDToken(ctx context.Context, audience string) (provider, token string, err error) {
	if reqURL, bearer := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"); reqURL != "" && bearer != "" {
		u, perr := url.Parse(reqURL)
		if perr != nil {
			return "", "", perr
		}
		q := u.Query()
		q.Set("audience", audience)
		u.RawQuery = q.Encode()
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if rerr != nil {
			return "", "", rerr
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, derr := ciHTTP().Do(req)
		if derr != nil {
			return "", "", fmt.Errorf("request the GitHub OIDC token: %w", derr)
		}
		defer resp.Body.Close()
		var doc struct {
			Value string `json:"value"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc) != nil || doc.Value == "" {
			return "", "", fmt.Errorf("GitHub did not issue an OIDC token (HTTP %d)", resp.StatusCode)
		}
		return "github", doc.Value, nil
	}
	if t := os.Getenv(envPrefix + "_ID_TOKEN"); t != "" {
		return "gitlab", t, nil
	}
	return "", "", errNoCICredentials
}

// ciSignIn exchanges the CI job's OIDC token for a short-lived workload token
// and makes it this process's API key, so the policy commands run as the
// repository's workload identity. It does nothing when credentials already
// exist.
func ciSignIn(ctx context.Context, gatewayURL string) error {
	if os.Getenv(envPrefix+"_API_KEY") != "" {
		return nil
	}
	gw, err := resolveGatewayURLOrFailClosed(gatewayURL)
	if err != nil {
		return err
	}
	if !runningInCI() {
		return nil
	}
	provider, idToken, err := ciIDToken(ctx, gw)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"provider": provider, "idToken": idToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(gw, "/")+"/api/policy/ci/token", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ciHTTP().Do(req)
	if err != nil {
		return fmt.Errorf("exchange the OIDC token: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc)
	if resp.StatusCode != http.StatusOK || doc.Token == "" {
		msg := doc.Error
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("the gateway did not accept this job's OIDC token (HTTP %d: %s)", resp.StatusCode, msg)
	}
	return os.Setenv(envPrefix+"_API_KEY", doc.Token)
}

func runningInCI() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true" || os.Getenv("GITLAB_CI") == "true" || os.Getenv("CI") == "true"
}

// policyClient is approvalsClient for the policy commands: inside a CI job
// without credentials it first signs in with the job's OIDC token.
func policyClient(ctx context.Context, gatewayURL string) (*client.Client, error) {
	if err := ciSignIn(ctx, gatewayURL); err != nil {
		return nil, err
	}
	return approvalsClient(gatewayURL)
}

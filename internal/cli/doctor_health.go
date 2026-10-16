package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// doctorHealth reports the gateway's public health and where it publishes its
// status page. Both routes need no credentials, so they answer even when the
// session is broken, which is when a person most needs to know whether the
// gateway itself is up.
func doctorHealth(ctx context.Context, gateway string, add func(string, bool, string)) {
	plain := client.NoRedirectHTTPClient()
	get := func(path string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway, "/")+path, nil)
		if err != nil {
			return 0, nil, err
		}
		resp, err := plain.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, err := readLimitedResponse(resp.Body, 1<<20)
		return resp.StatusCode, raw, err
	}

	status, raw, err := get("/api/health")
	switch {
	case err != nil:
		add("health", false, "the gateway's health endpoint could not be reached")
	case status == http.StatusOK:
		var doc struct {
			Degraded []string `json:"degraded"`
		}
		_ = json.Unmarshal(raw, &doc)
		if len(doc.Degraded) > 0 {
			add("health", true, "ready, but degraded: "+textsafe.Clean(strings.Join(doc.Degraded, ", ")))
		} else {
			add("health", true, "ready")
		}
	case status == http.StatusNotFound:
		add("health", true, "this gateway does not publish a health endpoint")
	default:
		add("health", false, fmt.Sprintf("the gateway reports it is not ready (HTTP %d)", status))
	}

	status, raw, err = get("/api/version")
	var doc struct {
		StatusPageURL string `json:"statusPageUrl"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(raw, &doc) != nil || !publicHTTPURL(doc.StatusPageURL) {
		add("status-page", true, "none published by this gateway")
		return
	}
	add("status-page", true, textsafe.Clean(doc.StatusPageURL))
}

func publicHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
)

const (
	riskUnknown   = "unknown"
	riskCacheFile = "risk-catalog.json"
	riskTimeout   = 10 * time.Second
)

type riskEntry struct {
	Service string `json:"service"`
	Tool    string `json:"tool"`
	Risk    string `json:"risk"`
	Version string `json:"version"`
}

// riskCatalog mirrors GET /api/catalog/risk and remembers the ETag it was served with.
type riskCatalog struct {
	GatewayURL string      `json:"gatewayUrl,omitempty"`
	ETag       string      `json:"etag,omitempty"`
	Version    string      `json:"version"`
	Tools      []riskEntry `json:"tools"`
}

// decisionForRisk lets reads run and asks for everything else, including tools the catalog does not know.
func decisionForRisk(risk string) string {
	if risk == "read" {
		return "allow"
	}
	return "ask"
}

func (c *riskCatalog) lookup(service, tool string) string {
	if c != nil {
		for _, e := range c.Tools {
			if e.Service == service && e.Tool == tool {
				return e.Risk
			}
		}
	}
	return riskUnknown
}

func riskCachePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, riskCacheFile), nil
}

func loadRiskCache() *riskCatalog {
	path, err := riskCachePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cat riskCatalog
	if json.Unmarshal(data, &cat) != nil {
		return nil
	}
	return &cat
}

func saveRiskCache(cat *riskCatalog) {
	path, err := riskCachePath()
	if err != nil {
		return
	}
	if data, err := json.Marshal(cat); err == nil {
		_ = os.WriteFile(path, data, 0o600)
	}
}

// fetchRiskCatalog revalidates the cached catalog with If-None-Match and returns the fresh or still-valid copy.
func fetchRiskCatalog(c *client.Client, cached *riskCatalog) (*riskCatalog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), riskTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.GatewayURL+"/api/catalog/risk", nil)
	if err != nil {
		return nil, err
	}
	if cached != nil && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /api/catalog/risk: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		if cached == nil {
			return nil, fmt.Errorf("risk catalog: 304 without a cached copy")
		}
		return cached, nil
	case http.StatusOK:
		var cat riskCatalog
		if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
			return nil, fmt.Errorf("decode risk catalog: %w", err)
		}
		cat.GatewayURL = c.GatewayURL
		cat.ETag = resp.Header.Get("ETag")
		saveRiskCache(&cat)
		return &cat, nil
	default:
		return nil, fmt.Errorf("risk catalog request failed (%d)", resp.StatusCode)
	}
}

// currentRiskCatalog returns the freshest catalog available: revalidated against the gateway when online and signed in,
// otherwise the cached copy, otherwise nil.
func currentRiskCatalog(gatewayOverride string) *riskCatalog {
	cached := loadRiskCache()
	gURL, err := resolveGatewayURLOrFailClosed(gatewayOverride)
	if err != nil {
		return cached
	}
	if cached != nil && cached.GatewayURL != "" && cached.GatewayURL != gURL {
		cached = nil
	}
	c, err := client.Resolve(gURL)
	if err != nil {
		return cached
	}
	if fresh, err := fetchRiskCatalog(c, cached); err == nil {
		return fresh
	}
	return cached
}

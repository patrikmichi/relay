package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/patrikmichi/relay/internal/client"
)

// WhoamiResponse is the JSON returned by /api/cli/whoami.
type WhoamiResponse struct {
	Email     string   `json:"email"`
	GoogleSub string   `json:"googleSub"`
	Services  []string `json:"services"`
	IssuedAt  string   `json:"issuedAt"`
	Groups    []string `json:"groups,omitempty"`
}

func Whoami(ctx context.Context, c *client.Client, full bool) (*WhoamiResponse, error) {
	path := "/api/cli/whoami"
	if full {
		path += "?groups=true"
	}

	resp, err := c.GetContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("token expired or revoked — run `relay login` to re-authenticate")
	}

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("whoami failed (%d): %s", resp.StatusCode, errBody["error"])
	}

	var wr WhoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return nil, fmt.Errorf("decode whoami response: %w", err)
	}
	return &wr, nil
}

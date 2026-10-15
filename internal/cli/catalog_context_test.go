package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/patrikmichi/relay/internal/client"
)

func TestCatalogDiscoveryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := client.New("http://127.0.0.1:1", "test-token")
	for name, call := range map[string]func() error{
		"skills": func() error { _, err := searchSkills(ctx, c, "review"); return err },
		"mcp":    func() error { _, err := listMcpServers(ctx, c); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation without dialing: %v", err)
			}
		})
	}
}

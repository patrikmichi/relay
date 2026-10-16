package client_test

import (
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
)

func apiKeyEnv(t *testing.T, savedGateway, boundOrigin string) {
	t.Helper()
	withTempHome(t)
	t.Setenv("ZUANO_API_KEY", "synthetic-key")
	t.Setenv("ZUANO_API_KEY_ORIGIN", boundOrigin)
	t.Setenv("ZUANO_AGENT", "")
	if savedGateway != "" {
		if err := config.Save(config.Config{GatewayURL: savedGateway}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAPIKeyStaysBoundToSavedGateway(t *testing.T) {
	apiKeyEnv(t, "https://gw.example.com", "")

	if _, err := client.Resolve("https://gw.example.com"); err != nil {
		t.Fatalf("saved gateway refused: %v", err)
	}
	_, err := client.Resolve("https://evil.example.net")
	if err == nil || !strings.Contains(err.Error(), "bound to https://gw.example.com") {
		t.Fatalf("override to another origin = %v, want a bound-origin refusal", err)
	}
}

func TestAPIKeyOriginEnvOverridesSavedGateway(t *testing.T) {
	apiKeyEnv(t, "https://saved.example.com", "https://ci.example.com")

	if _, err := client.Resolve("https://ci.example.com"); err != nil {
		t.Fatalf("ZUANO_API_KEY_ORIGIN gateway refused: %v", err)
	}
	if _, err := client.Resolve("https://saved.example.com"); err == nil {
		t.Fatal("saved gateway accepted although ZUANO_API_KEY_ORIGIN names another")
	}
}

func TestUnboundAPIKeyIsRefusedForRemoteGateways(t *testing.T) {
	apiKeyEnv(t, "", "")

	_, err := client.Resolve("https://anywhere.example.com")
	if err == nil || !strings.Contains(err.Error(), "ZUANO_API_KEY_ORIGIN") {
		t.Fatalf("unbound key to a remote gateway = %v, want a refusal naming ZUANO_API_KEY_ORIGIN", err)
	}
	if _, err := client.Resolve("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("loopback development gateway refused: %v", err)
	}
}

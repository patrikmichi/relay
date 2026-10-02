package config

import "testing"

func TestPublicBuildHasNoDefaultGateway(t *testing.T) {
	if DefaultGatewayURL != "" {
		t.Fatal("public builds must require an explicitly selected gateway")
	}
}

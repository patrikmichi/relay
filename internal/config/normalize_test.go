package config_test

import (
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
)

func TestNormalizeGatewayURL_OneIdentityPerHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://Gateway.Example.COM":      "https://gateway.example.com",
		"https://gateway.example.com:443":  "https://gateway.example.com",
		"https://gateway.example.com:443/": "https://gateway.example.com",
		"https://gateway.example.com.":     "https://gateway.example.com",
		"HTTPS://gateway.example.com:8443": "https://gateway.example.com:8443",
		"http://LOCALHOST:80":              "http://localhost",
		"http://localhost:3000":            "http://localhost:3000",
		"http://[::1]:3000":                "http://[::1]:3000",
		"http://[::1]:80":                  "http://[::1]",
	} {
		got, err := config.NormalizeGatewayURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeGatewayURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

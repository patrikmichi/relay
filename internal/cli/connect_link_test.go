package cli

import (
	"strings"
	"testing"
)

func TestGatewayLinkStaysOnTheGateway(t *testing.T) {
	const gw = "https://gw.example.com"
	good := map[string]string{
		"/connect/clockify":      "https://gw.example.com/connect/clockify",
		"/api/oauth/start?s=a+b": "https://gw.example.com/api/oauth/start?s=a+b",
	}
	for in, want := range good {
		got, err := gatewayLink(gw, in)
		if err != nil || got != want {
			t.Errorf("gatewayLink(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"//evil.example.net/x",
		"https://evil.example.net/x",
		"@evil.example.net/x",
		".evil.example.net/x",
		"/\\evil.example.net",
		"",
		"/ok\x1b[2J",
		"javascript:alert(1)",
	} {
		if got, err := gatewayLink(gw, bad); err == nil {
			t.Errorf("gatewayLink(%q) = %q, want a refusal", bad, got)
		} else if strings.ContainsRune(err.Error(), 0x1b) {
			t.Errorf("error leaks a control character: %q", err.Error())
		}
	}
}

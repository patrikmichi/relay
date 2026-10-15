package cli

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var credentialReferenceName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]{0,127}$`)

// Built from parts so secret scanners don't mistake the list for a token.
var rawSecretPrefixes = []string{"sk-", "gh" + "p_", "github" + "_pat_", "eyj", "xox", "akia"}

func hasRawSecretPrefix(lower string) bool {
	for _, prefix := range rawSecretPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// Validates the public registration contract. The gateway additionally performs
// its DNS-aware SSRF, credential collision, and secret-scanning checks.
func validateMcpDescriptor(d mcpDescriptor) error {
	if d.V < 0 || len(d.RuntimeEgressHosts) > 20 {
		return fmt.Errorf("MCP descriptor version or egress host limit invalid")
	}
	for _, host := range d.RuntimeEgressHosts {
		if len(host) == 0 || len(host) > 253 || strings.ContainsAny(host, "/*:@?# ") {
			return fmt.Errorf("MCP runtime egress entries must be hostnames")
		}
	}

	if d.Source != "" && d.Source != "builtin" && d.Source != "url" && d.Source != "repo" {
		return fmt.Errorf("MCP source must be builtin, url, or repo")
	}
	if d.AuthScope != "" && d.AuthScope != "org" && d.AuthScope != "user" {
		return fmt.Errorf("MCP authScope must be org or user")
	}
	switch d.AuthType {
	case "":
	case "api_key":
		if d.AuthRef == "" {
			return fmt.Errorf("MCP api_key authentication requires an authRef")
		}
	case "none", "oauth":
		if d.AuthRef != "" {
			return fmt.Errorf("MCP none/oauth authentication forbids authRef")
		}
	default:
		return fmt.Errorf("MCP authType must be none, api_key, or oauth")
	}
	if d.AuthRef != "" {
		lower := strings.ToLower(d.AuthRef)
		if !credentialReferenceName.MatchString(d.AuthRef) || hasRawSecretPrefix(lower) {
			return fmt.Errorf("MCP authRef must be a credential reference name, never a secret")
		}
	}
	switch d.Transport {
	case "http", "sse":
		if d.Endpoint == "" && d.Source != "repo" {
			return fmt.Errorf("MCP network transport requires an endpoint")
		}
		if d.Command != "" {
			return fmt.Errorf("MCP network transport must not include command")
		}
	case "stdio-command":
		if d.Command == "" && d.Source != "repo" {
			return fmt.Errorf("MCP stdio-command transport requires a command")
		}
		if d.Endpoint != "" {
			return fmt.Errorf("MCP stdio-command transport must not include endpoint")
		}
	default:
		return fmt.Errorf("MCP transport must be http, sse, or stdio-command")
	}
	if d.Endpoint != "" {
		u, err := url.Parse(d.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(d.Endpoint) > 2048 {
			return fmt.Errorf("MCP endpoint must be an HTTP(S) URL without embedded credentials or fragment")
		}
	}
	if len(d.Command) > 512 || len(d.DeclaredTools) > 256 {
		return fmt.Errorf("MCP descriptor exceeds contract limits")
	}
	for _, tool := range d.DeclaredTools {
		if len(tool) == 0 || len(tool) > 128 {
			return fmt.Errorf("MCP tool names must contain 1 to 128 bytes")
		}
	}
	return nil
}

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/patrikmichi/zuano/internal/keychain"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// gatewayMessage reads the human message out of a gateway error body, which is
// either {"error":"text"} or {"error":{"message":"text"}}. Control characters
// are removed: the text comes from the network and is printed on a terminal.
func gatewayMessage(body []byte) string {
	return textsafe.Clean(parseGatewayMessage(body))
}

func parseGatewayMessage(body []byte) string {
	var doc struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Reason  string          `json:"reason"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	if doc.Reason != "" {
		return doc.Reason
	}
	var text string
	if json.Unmarshal(doc.Error, &text) == nil && text != "" {
		return strings.TrimPrefix(text, "Forbidden: ")
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(doc.Error, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return doc.Message
}

// agentSessionDenied turns the 403 an agent session gets on a person-facing
// mutation into guidance. It returns nil for every other response, so callers
// fall through to their own handling.
func agentSessionDenied(profile keychain.Profile, status int, body []byte) error {
	if status != http.StatusForbidden || profile != keychain.ProfileAgent {
		return nil
	}
	reason := gatewayMessage(body)
	if reason == "" {
		reason = "agent sessions cannot change this"
	}
	return fmt.Errorf("the gateway refused this for an agent session: %s\n"+
		"Run it with your personal profile (unset RELAY_AGENT) or use the MCP tools, which ask a person to approve", reason)
}

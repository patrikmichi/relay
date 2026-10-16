package cli

import (
	"encoding/json"

	"github.com/patrikmichi/relay/internal/textsafe"
)

const policyOffReason = "policy_off"

// policyRefusal recognises a call the organisation's or the platform's risk
// policy turned off: a body, or an MCP result, whose reason is policy_off. It
// ends with exit status 10, the status of a denial, and the gateway's
// sentence, which names the policy source and who can change it.
func policyRefusal(body []byte, env *mcpEnvelope) error {
	candidates := [][]byte{body}
	if env != nil && env.Result != nil && env.Result.IsError && len(env.Result.StructuredContent) > 0 {
		candidates = append(candidates, env.Result.StructuredContent)
	}
	for _, raw := range candidates {
		var doc struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(raw, &doc) != nil || doc.Reason != policyOffReason {
			continue
		}
		msg := doc.Message
		if msg == "" {
			msg = doc.Error
		}
		if msg == "" {
			msg = "blocked by policy"
		}
		return &ExitError{Code: exitDenied, Msg: textsafe.Clean(msg)}
	}
	return nil
}

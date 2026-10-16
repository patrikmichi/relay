package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

type explainResult struct {
	Service  string `json:"service"`
	Tool     string `json:"tool"`
	Risk     string `json:"risk"`
	Decision string `json:"decision"`
	// Approval is what an agent session needs first: "approval" or "grant".
	Approval string `json:"approval,omitempty"`
}

func explainCall(cat *riskCatalog, service, tool string) explainResult {
	risk := cat.lookup(service, tool)
	return explainResult{Service: service, Tool: tool, Risk: risk, Decision: decisionForRisk(risk), Approval: approvalForRisk(risk)}
}

func (r explainResult) String() string {
	s := fmt.Sprintf("%s %s: %s -> %s", r.Service, r.Tool, r.Risk, r.Decision)
	if r.Approval != "" {
		s += " (" + r.Approval + ")"
	}
	return s
}

// ExplainCmd returns `relay explain`, which reports how a command would be treated without running it.
func ExplainCmd() *cobra.Command {
	explain := &cobra.Command{Use: "explain", Short: "Explain how a command would be treated without running it"}

	var gatewayURL string
	var asJSON bool
	call := &cobra.Command{
		Use:   "call <service> <tool>",
		Short: "Report the risk class and decision for a tool call",
		Long: `Reports the tool's risk class (read, write, destructive, outward) from the gateway's risk catalog and the resulting decision:
allow for read, ask for everything else. A tool the catalog does not list has risk "unknown" and decision "ask".
For an agent session the verdict also names what the gateway requires before the call runs: "approval" for destructive and outward
tools (a human approves each call), "grant" for writes (a human approves the call or opens a time-boxed grant for the service).
Reads need neither unless an org rule holds the call, which the catalog cannot show.
Uses the cached catalog when offline or when the gateway cannot be reached.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res := explainCall(currentRiskCatalog(gatewayURL), args[0], args[1])
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), res)
			return err
		},
	}
	call.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	call.Flags().BoolVar(&asJSON, "json", false, "Print {service, tool, risk, decision, approval} as JSON")
	explain.AddCommand(call)
	return explain
}

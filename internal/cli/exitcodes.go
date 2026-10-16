package cli

import (
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/signing"
)

// Exit statuses without a command of their own yet, named as in the registry
// in docs/reference/gateway-contracts.md.
const (
	exitUsage            = 2
	exitInvalidManifest  = 16
	exitWaitTimeout      = 17
	exitRunFailed        = 20
	exitEvalRegression   = 21
	exitEvalInconclusive = 23
	exitEvalThreshold    = 24
	exitNetworkOffline   = 34
	exitToolChanged      = 43
	exitQuarantined      = 44
	exitA2AChainRefused  = 50
	exitChainKilled      = 51
	exitChainBudget      = 52
	exitAgentCardChanged = 53
	exitSpecInvalid      = 60
	exitSpecUnsupported  = 61
	exitConnectorClash   = 62
	exitQualityGate      = 63
)

// exitCodeEntry is one row of the exit-code registry.
type exitCodeEntry struct {
	code int
	name string
}

// exitRegistry is every status the CLI can end with. The table in
// docs/reference/gateway-contracts.md must list exactly these.
var exitRegistry = []exitCodeEntry{
	{0, "ok"},
	{1, "error"},
	{exitUsage, "usage"},
	{signing.ExitContentSignature, "signature_invalid"},
	{exitDenied, "denied"},
	{exitExpired, "approval_expired"},
	{exitPending, "pending_approval"},
	{client.ExitRateLimited, "rate_limited"},
	{client.ExitStepUp, "step_up_required"},
	{exitPlanStale, "plan_stale"},
	{exitInvalidManifest, "invalid_manifest"},
	{exitWaitTimeout, "wait_timeout"},
	{exitRunFailed, "run_failed"},
	{exitEvalRegression, "eval_regression"},
	{exitSessionTooLarge, "session_too_large"},
	{exitEvalInconclusive, "eval_inconclusive"},
	{exitEvalThreshold, "eval_threshold_not_met"},
	{client.ExitPlanRequired, "plan_required"},
	{client.ExitUsageExhausted, "quota_exceeded"},
	{client.ExitBillingDown, "billing_unavailable"},
	{client.ExitSuspended, "suspended"},
	{exitNetworkOffline, "network_offline"},
	{exitPolicyInvalid, "policy_invalid"},
	{exitPolicyConflict, "policy_conflict"},
	{exitPolicyManaged, "policy_managed_in_git"},
	{exitToolChanged, "tool_definition_changed"},
	{exitQuarantined, "upstream_quarantined"},
	{exitBundleInvalid, "evidence_bundle_invalid"},
	{exitKeysMissing, "evidence_keys_missing"},
	{exitA2AChainRefused, "a2a_chain_refused"},
	{exitChainKilled, "chain_killed"},
	{exitChainBudget, "chain_budget_exhausted"},
	{exitAgentCardChanged, "agent_card_changed"},
	{exitSpecInvalid, "spec_invalid"},
	{exitSpecUnsupported, "spec_unsupported"},
	{exitConnectorClash, "connector_conflict"},
	{exitQualityGate, "quality_gate_failed"},
}

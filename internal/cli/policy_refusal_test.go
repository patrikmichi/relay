package cli

import (
	"strings"
	"testing"
)

const policyOffText = "Blocked by org policy: stripe.create_refund (destructive) is Off — set by the organisation's risk matrix. Can be changed by: org admins."

func TestCall_PolicyOffInAToolResultExitsWithTheDenialStatus(t *testing.T) {
	refusingGateway(t, 200, nil, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"`+policyOffText+`"}],"structuredContent":{"reason":"policy_off","message":"`+policyOffText+`"}}}`)
	_, err := runCommand(t, CallCmd(), "stripe", "create_refund")
	if got := exitCodeOf(err); got != exitDenied {
		t.Fatalf("exit = %d (%v), want %d", got, err, exitDenied)
	}
	if err == nil || err.Error() != policyOffText {
		t.Errorf("message = %v", err)
	}
}

func TestCall_PolicyOffInARESTBodyExitsWithTheDenialStatus(t *testing.T) {
	refusingGateway(t, 403, nil, `{"error":"Blocked","reason":"policy_off","message":"`+policyOffText+`"}`)
	_, err := runCommand(t, CallCmd(), "stripe", "create_refund")
	if got := exitCodeOf(err); got != exitDenied || err == nil || !strings.Contains(err.Error(), "Can be changed by: org admins") {
		t.Fatalf("exit = %d, err = %v", got, err)
	}
}

func TestCall_PolicyMessageHasControlCharactersRemoved(t *testing.T) {
	refusingGateway(t, 403, nil, `{"reason":"policy_off","message":"off\u001b[2J here"}`)
	_, err := runCommand(t, CallCmd(), "stripe", "create_refund")
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("err = %q", err)
	}
}

func TestCall_AToolErrorWithAnotherReasonStaysAFailure(t *testing.T) {
	refusingGateway(t, 200, nil, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"upstream failed"}],"structuredContent":{"reason":"upstream_error"}}}`)
	_, err := runCommand(t, CallCmd(), "stripe", "create_refund")
	if got := exitCodeOf(err); got != 1 {
		t.Fatalf("exit = %d (%v), want 1", got, err)
	}
}

func TestCall_HeldCallInAPersonalSessionExplainsItFollowsAgentApprovals(t *testing.T) {
	refusingGateway(t, 200, nil, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{"status":"pending_approval","approval_id":"apr_9","service":"stripe","tool":"create_refund","risk":"write","expires_at":"2026-10-16T12:15:00.000Z"}}}`)
	out, err := runCommand(t, CallCmd(), "stripe", "create_refund")
	if got := exitCodeOf(err); got != exitPending {
		t.Fatalf("exit = %d (%v), want %d", got, err, exitPending)
	}
	for _, want := range []string{"apr_9", "Personal CLI sessions follow the same approvals as agents", "relay approvals approve apr_9"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCall_HeldCallInAnAgentSessionDoesNotClaimItIsPersonal(t *testing.T) {
	refusingGateway(t, 200, nil, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{"status":"pending_approval","approval_id":"apr_9","service":"stripe","tool":"create_refund","risk":"write","expires_at":"2026-10-16T12:15:00.000Z"}}}`)
	t.Setenv("ZUANO_AGENT", "1")
	out, _ := runCommand(t, CallCmd(), "stripe", "create_refund")
	if strings.Contains(out, "Personal CLI sessions") {
		t.Errorf("an agent session was told it is personal:\n%s", out)
	}
}

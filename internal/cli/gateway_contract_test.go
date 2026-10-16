package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/signing"
)

// These bodies have the shapes the gateway routes build, so a drift between a
// route and the client shows up here rather than against a live gateway.
const (
	// app/api/approvals/[id]/route.ts, default and ?full=1.
	contractApprovalPreview = `{"approval":{"approval_id":"apr_1","status":"pending","service":"stripe","tool":"create_refund","risk":"destructive","args_digest":"9b2e","requester":"dev@example.com","reason":"destructive tool","created_at":"2026-10-16T12:00:00.000Z","expires_at":"2026-10-16T12:15:00.000Z"},"args_preview":{"charge":"ch_1"},"truncated":false,"args_digest":"9b2e"}`
	contractApprovalFull    = `{"approval":{"approval_id":"apr_1","status":"pending","service":"stripe","tool":"create_refund","risk":"destructive","args_digest":"9b2e","requester":"dev@example.com","reason":"destructive tool","created_at":"2026-10-16T12:00:00.000Z","expires_at":"2026-10-16T12:15:00.000Z"},"args":{"charge":"ch_1","memo":"full text"},"truncated":false}`
	contractApprovalGone    = `{"approval":{"approval_id":"apr_1","status":"pending","service":"stripe","tool":"create_refund","risk":"destructive","args_digest":"9b2e","requester":"dev@example.com","reason":"destructive tool","created_at":"2026-10-16T12:00:00.000Z","expires_at":"2026-10-16T12:15:00.000Z"},"args_preview":null,"truncated":false}`

	// lib/approvals/result.ts pendingMcpResult.
	contractPendingMCP = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\n  \"status\": \"pending_approval\",\n  \"approval_id\": \"apr_1\",\n  \"service\": \"stripe\",\n  \"tool\": \"create_refund\",\n  \"risk\": \"destructive\",\n  \"args_digest\": \"9b2e\",\n  \"expires_at\": \"2026-10-16T12:15:00.000Z\",\n  \"next\": \"A human has to approve this call.\"\n}"}],"structuredContent":{"status":"pending_approval","approval_id":"apr_1","service":"stripe","tool":"create_refund","risk":"destructive","args_digest":"9b2e","expires_at":"2026-10-16T12:15:00.000Z"},"isError":false}}`

	// app/api/marketplace/skills/[id]/requirements/route.ts.
	contractRequirements = `{"requirements":[{"slug":"fakturoid","tools":["list_invoices"],"connected":true},{"slug":"slack","connected":false}],"satisfied":false,"missing":["slack"],"skillId":"res_1","version":"1.2.0"}`

	// app/api/marketplace/skills/[id]/evals/runs/[runId]: run, results and progress.
	contractEvalRun = `{"run":{"id":"evr_1","setId":"evs_1","kind":"trigger","status":"succeeded","repeats":3,"splits":["train","validation"],"model":"anthropic/claude-sonnet-5.5","judgeModel":"anthropic/claude-sonnet-5.5","variants":[{"label":"1.1.0","versionId":"rev_1"},{"label":"1.2.0","versionId":"rev_2"}],"maxTokens":200000,"inputTokens":1200,"outputTokens":300,"costUsd":0.01,"error":null,"summary":{"total":30,"completed":30,"variants":[{"label":"1.1.0","versionId":"rev_1","trigger":{"train":{"precision":{"mean":1,"stddev":0,"n":3},"recall":{"mean":1,"stddev":0,"n":3},"f1":{"mean":1,"stddev":0,"n":3},"cases":5},"validation":{"precision":{"mean":0.8,"stddev":0,"n":3},"recall":{"mean":0.8,"stddev":0,"n":3},"f1":{"mean":0.8,"stddev":0,"n":3},"cases":3}},"inputTokens":600,"outputTokens":150,"costUsd":0.005,"latencyMs":{"mean":420,"stddev":80,"n":30},"errors":0,"skipped":0},{"label":"1.2.0","versionId":"rev_2","trigger":{"validation":{"precision":{"mean":0.5,"stddev":0,"n":3},"recall":{"mean":0.5,"stddev":0,"n":3},"f1":{"mean":0.5,"stddev":0,"n":3},"cases":3}},"inputTokens":600,"outputTokens":150,"costUsd":0.005,"latencyMs":{"mean":400,"stddev":70,"n":30},"errors":0,"skipped":0}],"comparisons":[{"baseline":"1.1.0","variant":"1.2.0","deltas":[{"caseId":"evc_1","caseName":"asks","split":"validation","baseline":1,"variant":0,"delta":-1}],"winLoss":{"all":{"wins":0,"losses":1,"ties":4},"validation":{"wins":0,"losses":1,"ties":2}},"significance":{"all":{"meanDelta":-0.2,"ciLow":-0.4,"ciHigh":0,"pValue":0.04,"significant":true},"validation":{"meanDelta":-0.3,"ciLow":-0.5,"ciHigh":-0.1,"pValue":0.01,"significant":true}}}],"regressions":[{"versionId":"rev_2","previousVersionId":"rev_1","kind":"trigger","score":0.5,"previous":0.8}]},"createdAt":"2026-10-16T12:00:00.000Z","startedAt":"2026-10-16T12:00:01.000Z","finishedAt":"2026-10-16T12:00:30.000Z"},"results":[{"id":"evr_x","runId":"evr_1","caseId":"evc_1","caseName":"asks","variant":"1.2.0","repeat":0,"split":"validation","outcome":"fail","score":0,"detail":{},"inputTokens":10,"outputTokens":3,"latencyMs":400,"agentRunId":null}],"progress":{"done":30,"total":30}}`

	// GET /evals/sets/{setId}.
	contractEvalSet = `{"set":{"id":"evs_1","skillId":"res_1","name":"smoke","description":"","versionId":"rev_1","trainPercent":70,"counts":{"trigger":8,"task":0,"train":5,"validation":3},"cases":[{"id":"evc_1","kind":"trigger","name":"asks","input":"invoice Acme","shouldTrigger":true,"split":"train","requiredConnectors":[]}]}}`
)

// lib/marketplace/generate-manifest.ts MarketplaceManifestSchema output: the
// owner is an object, not a string.
const contractMarketplaceManifest = `{"name":"org-abc123-marketplace","owner":{"name":"org_abc123","email":"admin@example.com"},"plugins":[{"name":"searcher","source":"./plugins/res_abc123","version":"1.2.3","description":"Search","displayName":"Searcher"}],"description":"Plugins","version":"2026.10.1"}`

func TestContract_MarketplaceManifestDecodes(t *testing.T) {
	var m marketplaceManifest
	if err := json.Unmarshal([]byte(contractMarketplaceManifest), &m); err != nil {
		t.Fatalf("the gateway's manifest does not decode: %v", err)
	}
	if m.Owner.Name != "org_abc123" || m.Owner.Email != "admin@example.com" || len(m.Plugins) != 1 || m.Plugins[0].Source != "./plugins/res_abc123" {
		t.Errorf("manifest = %+v", m)
	}
	var empty marketplaceManifest
	if err := json.Unmarshal([]byte(`{"name":"org-marketplace","owner":{"name":"org"},"plugins":[]}`), &empty); err != nil || empty.Owner.Name != "org" {
		t.Errorf("empty manifest = %+v, %v", empty, err)
	}
}

func TestContract_ApprovalShowReadsArgsPreview(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals/apr_1": reply(200, contractApprovalPreview),
	})
	out, err := runCommand(t, ApprovalsCmd(), "show", "apr_1")
	if err != nil || !strings.Contains(out, `"charge": "ch_1"`) {
		t.Fatalf("show = %q, %v", out, err)
	}
}

func TestContract_ApproveShowsTheFullArgsTheGatewayReturns(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals/apr_1?full=1":   reply(200, contractApprovalFull),
		"POST /api/approvals/apr_1/approve": reply(200, `{"ok":true,"approval":{"approval_id":"apr_1","status":"approved"}}`),
	})
	out, err := decide(t, "y\n", "approve", "apr_1")
	if err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if !strings.Contains(out, "full text") {
		t.Errorf("the review omitted the untruncated arguments:\n%s", out)
	}
}

func TestContract_ApproveRefusesWhenTheGatewayHoldsNoArgs(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals/apr_1?full=1": reply(200, contractApprovalGone),
	})
	if _, err := decide(t, "y\n", "approve", "apr_1"); err == nil || !strings.Contains(err.Error(), "blind") {
		t.Errorf("err = %v, want a blind-approval refusal", err)
	}
}

func TestContract_PendingMCPResult(t *testing.T) {
	env, err := decodeMCPEnvelope([]byte(contractPendingMCP))
	if err != nil {
		t.Fatal(err)
	}
	p := pendingFromResult(env.Result)
	if p == nil || p.ApprovalID != "apr_1" || p.Risk != "destructive" || p.Service != "stripe" || p.Tool != "create_refund" {
		t.Fatalf("pending = %+v", p)
	}
}

func TestContract_Requirements(t *testing.T) {
	var r skillRequirements
	if err := json.Unmarshal([]byte(contractRequirements), &r); err != nil {
		t.Fatal(err)
	}
	if r.Satisfied || r.SkillID != "res_1" || r.Version != "1.2.0" || len(r.Missing) != 1 || r.Missing[0] != "slack" {
		t.Fatalf("requirements = %+v", r)
	}
	if len(r.Requirements) != 2 || !r.Requirements[0].Connected || r.Requirements[1].Connected || r.Requirements[0].Tools[0] != "list_invoices" {
		t.Errorf("statuses = %+v", r.Requirements)
	}
}

func TestContract_EvalRunAndSet(t *testing.T) {
	var d runDoc
	if err := json.Unmarshal([]byte(contractEvalRun), &d); err != nil {
		t.Fatal(err)
	}
	s := d.Run.Summary
	if d.Run.Status != "succeeded" || s == nil || s.Completed != 30 || d.Progress.Total != 30 || d.Run.errorText() != "" {
		t.Fatalf("run = %+v", d)
	}
	if got := s.Variants[0].Trigger["validation"].F1.Mean; got != 0.8 {
		t.Errorf("validation f1 = %v", got)
	}
	if regs := findRegressions(d.Run); len(regs) != 2 {
		t.Errorf("regressions = %v, want the stored alert and the significant comparison", regs)
	}

	var set struct {
		Set evalSet `json:"set"`
	}
	if err := json.Unmarshal([]byte(contractEvalSet), &set); err != nil {
		t.Fatal(err)
	}
	if set.Set.Counts.Trigger != 8 || set.Set.Cases[0].ShouldTrigger == nil || !*set.Set.Cases[0].ShouldTrigger {
		t.Errorf("set = %+v", set.Set)
	}
}

func TestContract_SigningKeysDocument(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"keys":[{"kid":"2026-10","alg":"ed25519","publicKey":"` + base64.StdEncoding.EncodeToString(pub) + `"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/relay-signing-keys" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()
	keys, err := signing.FetchKeys(context.Background(), srv.URL)
	if err != nil || len(keys) != 1 || keys[0].KID != "2026-10" {
		t.Fatalf("keys = %v, %v", keys, err)
	}
}

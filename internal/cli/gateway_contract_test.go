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

	"github.com/patrikmichi/zuano/internal/signing"
)

// The documents these tests decode are the ones the gateway's route tests
// write; see contract_fixtures_test.go for where they are read from.

func TestContract_MarketplaceManifestDecodes(t *testing.T) {
	var m marketplaceManifest
	if err := json.Unmarshal([]byte(contractFixture(t, "marketplace-manifest.json")), &m); err != nil {
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
		"GET /api/approvals/apr_1": reply(200, contractFixture(t, "approval-preview.json")),
	})
	out, err := runCommand(t, ApprovalsCmd(), "show", "apr_1")
	if err != nil || !strings.Contains(out, `"charge": "ch_1"`) {
		t.Fatalf("show = %q, %v", out, err)
	}
}

func TestContract_ApprovalBodiesDecodeToTheSameRecord(t *testing.T) {
	for name, file := range map[string]string{"preview": "approval-preview.json", "full": "approval-full.json", "args gone": "approval-args-gone.json"} {
		var doc struct {
			Approval approvalRecord `json:"approval"`
		}
		if err := json.Unmarshal([]byte(contractFixture(t, file)), &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a := doc.Approval; a.ApprovalID != "apr_1" || a.Status != "pending" || a.Service != "stripe" || a.Risk != "destructive" {
			t.Errorf("%s: approval = %+v", name, a)
		}
	}
}

func TestContract_ApproveOpensThePageForAnApprovalTheGatewayReturns(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals/apr_1": reply(200, contractFixture(t, "approval-preview.json")),
	})
	stubBrowserWith(t, nil)
	out, err := runCommand(t, ApprovalsCmd(), "approve", "apr_1", "--no-browser")
	if err != nil || !strings.Contains(out, "/approvals/apr_1?decision=approve") {
		t.Fatalf("approve = %v\n%s", err, out)
	}
}

func TestContract_PendingMCPResult(t *testing.T) {
	env, err := decodeMCPEnvelope([]byte(contractFixture(t, "mcp-pending-approval.json")))
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
	if err := json.Unmarshal([]byte(contractFixture(t, "skill-requirements.json")), &r); err != nil {
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
	if err := json.Unmarshal([]byte(contractFixture(t, "eval-run.json")), &d); err != nil {
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
	if err := json.Unmarshal([]byte(contractFixture(t, "eval-set.json")), &set); err != nil {
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
		if r.URL.Path != "/.well-known/zuano-signing-keys" {
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

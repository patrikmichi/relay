package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/signing/signingtest"
)

type packServer struct {
	*httptest.Server
	signer *signingtest.Signer
	mu     sync.Mutex
	bodies map[string][]map[string]any
	hits   map[string]int
	// override, when set, answers every request instead of the routes.
	override atomic.Pointer[http.HandlerFunc]
	// lenient lets a request to an unknown route end in 404 without failing the test.
	lenient bool
}

func (s *packServer) replace(h http.HandlerFunc) { s.override.Store(&h) }

func (s *packServer) body(route string, i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[route][i]
}

func (s *packServer) count(route string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[route]
}

// signed answers with a signed JSON body.
func (s *packServer) signed(w http.ResponseWriter, status int, body string) {
	s.signer.Sign(w.Header(), []byte(body))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// packGateway serves routes keyed by "METHOD path", pins the signing key the
// way a first login does and points the CLI at it.
func packGateway(t *testing.T, routes map[string]func(s *packServer, w http.ResponseWriter, r *http.Request)) *packServer {
	t.Helper()
	return packGatewayAt(t, routes, false)
}

// packGatewayAt is packGateway; with saved set, the gateway is stored in the
// config file of the HOME the test already prepared instead of a new one.
func packGatewayAt(t *testing.T, routes map[string]func(s *packServer, w http.ResponseWriter, r *http.Request), saved bool) *packServer {
	t.Helper()
	ps := &packServer{signer: signingtest.New("kid1"), bodies: map[string][]map[string]any{}, hits: map[string]int{}}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		var body map[string]any
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		ps.mu.Lock()
		ps.hits[route]++
		ps.bodies[route] = append(ps.bodies[route], body)
		ps.mu.Unlock()
		if o := ps.override.Load(); o != nil {
			(*o)(w, r)
			return
		}
		h, ok := routes[route]
		if !ok {
			if !ps.lenient {
				t.Errorf("unexpected request %s", route)
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h(ps, w, r)
	}))
	t.Cleanup(ps.Close)
	if saved {
		t.Setenv("ZUANO_API_KEY", syntheticKey("api"))
		saveGateway(t, ps.URL)
	} else {
		withAPIKeyGateway(t, ps.Server)
	}
	t.Setenv("ZUANO_AGENT", "")
	origin, err := config.NormalizeGatewayURL(ps.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.PinSigningKeys(origin, []config.SigningKey{ps.signer.Key}); err != nil {
		t.Fatal(err)
	}
	old := jobPollDelay
	jobPollDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { jobPollDelay = old })
	return ps
}

func jsonReply(status int, body string) func(*packServer, http.ResponseWriter, *http.Request) {
	return func(_ *packServer, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func signedReply(status int, body string) func(*packServer, http.ResponseWriter, *http.Request) {
	return func(s *packServer, w http.ResponseWriter, _ *http.Request) { s.signed(w, status, body) }
}

const planDoc = `{"planId":"ppl_1","planHash":"hash1","mode":"install","pack":{"slug":"finance","version":"1.0.0","name":"Finance"},
"params":{"currency":"EUR"},
"ops":[
 {"seq":1,"kind":"connector.enable","target":{"slug":"stripe"},"before":null,"after":{"enabled":true},"status":"apply","loosens":false},
 {"seq":2,"kind":"risk.cell","target":{"service":"stripe","risk":"outward"},"before":"off","after":"approval","status":"conflict","loosens":false,"reason":"edited by an admin"}],
"credentialPrompts":[{"slug":"stripe","name":"Stripe","scope":"org","status":"missing","neededBy":["invoice-reconcile"],"cliCommand":"zuano connect stripe"},
 {"slug":"google-workspace","name":"Google","scope":"personal","status":"missing","neededBy":[],"cliCommand":"zuano connect google-workspace"}],
"summary":{"apply":1,"noop":0,"conflict":1,"skipped":0,"loosening":0},"expiresAt":"2026-10-16T13:00:00Z"}`

const jobDone = `{"job":{"id":"job_1","kind":"packs.apply","status":"succeeded"}}`

func applyRoutes() map[string]func(*packServer, http.ResponseWriter, *http.Request) {
	return map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"POST /api/packs/finance/plans":        signedReply(200, planDoc),
		"POST /api/packs/plans/ppl_1/apply":    jsonReply(202, `{"installId":"pki_1","jobId":"job_1"}`),
		"GET /api/jobs/job_1":                  jsonReply(200, jobDone),
		"GET /api/packs/installs":              jsonReply(200, `{"installs":[{"id":"pki_1","status":"installed","channel":"stable","updateAvailable":"1.1.0","pack":{"slug":"finance","version":"1.0.0"}}]}`),
		"POST /api/packs/installs/pki_1/plans": signedReply(200, planDoc),
	}
}

func TestPackList_PrintsATableAndPassesFilters(t *testing.T) {
	var gotQuery string
	packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"GET /api/packs": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			_, _ = io.WriteString(w, `{"packs":[{"slug":"finance","version":"1.0.0","category":"finance","source":"platform","summary":"Invoices","fit":"works on your plan"},{"slug":"solo","version":"2.0.0","source":"org","summary":"s","installed":true}]}`)
		},
	})
	out, err := runCommand(t, PackCmd(), "list", "--category", "finance", "--installed")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"finance  1.0.0", "works on your plan", "solo", "installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if gotQuery != "category=finance&source=installed" {
		t.Errorf("query = %q", gotQuery)
	}
	out, err = runCommand(t, PackCmd(), "list", "--json")
	if err != nil || !strings.Contains(out, `"packs"`) {
		t.Errorf("json = %q, %v", out, err)
	}
}

func TestPackList_Empty(t *testing.T) {
	packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){"GET /api/packs": jsonReply(200, `{"packs":[]}`)})
	if out, err := runCommand(t, PackCmd(), "list"); err != nil || !strings.Contains(out, "No packs.") {
		t.Errorf("out = %q, %v", out, err)
	}
}

func TestPackShow_PrintsTheContentsAndVersion(t *testing.T) {
	var gotQuery string
	packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"GET /api/packs/finance": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			_, _ = io.WriteString(w, `{"pack":{"slug":"finance","displayName":"Finance","version":"1.0.0","summary":"Invoice","versions":["1.0.0","0.9.0"],
			"connectors":[{"slug":"stripe","required":true,"credentialScope":"org","why":"payments"},{"slug":"x","required":false}],
			"skills":["invoice-reconcile"],"bundles":["finance-toolkit"],"rules":["money-safety"]}}`)
		},
	})
	out, err := runCommand(t, PackCmd(), "show", "finance@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Finance 1.0.0 (finance)", "Versions: 1.0.0, 0.9.0", "connector stripe (required, org): payments", "connector x (optional, -)", "skills: invoice-reconcile", "bundles: finance-toolkit", "rules: money-safety", "zuano pack plan finance"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if gotQuery != "version=1.0.0" {
		t.Errorf("query = %q", gotQuery)
	}
	if out, err := runCommand(t, PackCmd(), "show", "finance", "--json"); err != nil || !strings.Contains(out, `"displayName"`) {
		t.Errorf("json = %q, %v", out, err)
	}
	if _, err := runCommand(t, PackCmd(), "show", "bad/name"); err == nil {
		t.Error("a path as a pack name was accepted")
	}
}

func TestPackPlan_VerifiesThePlanAndPrintsIt(t *testing.T) {
	ps := packGateway(t, applyRoutes())
	dir := t.TempDir()
	pf := filepath.Join(dir, "params.yaml")
	if err := os.WriteFile(pf, []byte("currency: USD\nlimit: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, PackCmd(), "plan", "finance@1.0.0", "--params-file", pf, "--param", "currency=EUR", "--param", "strict=true", "--param", "threshold=1000.5")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"install finance 1.0.0", "Connectors", "Risk and approvals", "1 to apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	b := ps.body("POST /api/packs/finance/plans", 0)
	params := b["params"].(map[string]any)
	if b["mode"] != "install" || b["version"] != "1.0.0" || params["currency"] != "EUR" || params["limit"] != float64(5) || params["strict"] != true || params["threshold"] != 1000.5 {
		t.Errorf("request = %v", b)
	}
	if out, err := runCommand(t, PackCmd(), "plan", "finance", "--json"); err != nil || !strings.Contains(out, `"planHash":"hash1"`) {
		t.Errorf("json = %q, %v", out, err)
	}
}

func TestPackPlan_RefusesAPlanThatDoesNotVerify(t *testing.T) {
	routes := map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"POST /api/packs/finance/plans": func(s *packServer, w http.ResponseWriter, _ *http.Request) {
			s.signer.Sign(w.Header(), []byte(planDoc))
			_, _ = io.WriteString(w, strings.Replace(planDoc, `"hash1"`, `"hash2"`, 1))
		},
	}
	packGateway(t, routes)
	_, err := runCommand(t, PackCmd(), "plan", "finance")
	if err == nil || exitCodeOf(err) != 4 {
		t.Errorf("tampered: exit %d, %v", exitCodeOf(err), err)
	}

	packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){"POST /api/packs/finance/plans": jsonReply(200, planDoc)})
	_, err = runCommand(t, PackCmd(), "plan", "finance")
	if err == nil || exitCodeOf(err) != 4 || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("unsigned: exit %d, %v", exitCodeOf(err), err)
	}
}

func TestPackPlan_RefusalsKeepTheirExitCodes(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		exit   int
	}{
		"plan required": {402, `{"error":"plan_required","message":"packs need Pro","required_plan":"pro","upgrade_url":"https://x/upgrade"}`, 30},
		"quota":         {402, `{"error":"quota_exceeded","message":"used up"}`, 31},
		"rate limited":  {429, `{"error":"rate_limited","message":"slow down"}`, 13},
		"other":         {500, `{"error":"boom"}`, 1},
	} {
		packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){"POST /api/packs/finance/plans": jsonReply(c.status, c.body)})
		_, err := runCommand(t, PackCmd(), "plan", "finance")
		if exitCodeOf(err) != c.exit {
			t.Errorf("%s: exit %d, %v", name, exitCodeOf(err), err)
		}
	}
}

func TestPackInstall_AppliesTheShownPlanWithDecisions(t *testing.T) {
	ps := packGateway(t, applyRoutes())
	var prompted []string
	old := connectConnector
	connectConnector = func(_ *cobra.Command, slug, _ string) error { prompted = append(prompted, slug); return nil }
	t.Cleanup(func() { connectConnector = old })

	out, err := runCommand(t, PackCmd(), "install", "finance", "--yes", "--decide", "2=take", "--no-connect")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b := ps.body("POST /api/packs/plans/ppl_1/apply", 0)
	dec := b["decisions"].(map[string]any)
	if b["planHash"] != "hash1" || dec["2"] != "take" {
		t.Errorf("apply body = %v", b)
	}
	for _, want := range []string{"Applying (install pki_1)", "Done.", "Still to connect:", "stripe (your organisation): `zuano connect stripe`", "google-workspace (each member)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(prompted) != 0 {
		t.Errorf("--no-connect connected %v", prompted)
	}

	if _, err := runCommand(t, PackCmd(), "install", "finance", "--yes", "--keep-conflicts"); err != nil {
		t.Fatal(err)
	}
	if dec := ps.body("POST /api/packs/plans/ppl_1/apply", 1)["decisions"].(map[string]any); dec["2"] != "keep" {
		t.Errorf("decisions = %v", dec)
	}
}

func TestPackInstall_NeedsADecisionForEveryConflictAndAConfirmation(t *testing.T) {
	ps := packGateway(t, applyRoutes())
	if _, err := runCommand(t, PackCmd(), "install", "finance", "--yes"); err == nil || !strings.Contains(err.Error(), "operation 2 conflicts") {
		t.Errorf("undecided conflict: %v", err)
	}
	for name, args := range map[string][]string{
		"both":     {"install", "finance", "--yes", "--keep-conflicts", "--take-conflicts"},
		"bad":      {"install", "finance", "--yes", "--decide", "x=keep"},
		"bad word": {"install", "finance", "--yes", "--decide", "2=maybe"},
	} {
		if _, err := runCommand(t, PackCmd(), args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := runCommand(t, PackCmd(), "install", "finance", "--keep-conflicts"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("no terminal and no --yes: %v", err)
	}
	if ps.count("POST /api/packs/plans/ppl_1/apply") != 0 {
		t.Error("a plan was applied without a decision or confirmation")
	}
}

func TestPackInstall_ConfirmationOnATerminalAndConnectWalk(t *testing.T) {
	packGateway(t, applyRoutes())
	var prompted []string
	old := connectConnector
	connectConnector = func(_ *cobra.Command, slug, _ string) error {
		prompted = append(prompted, slug)
		return errors.New("nope")
	}
	t.Cleanup(func() { connectConnector = old })

	cmd := PackCmd()
	cmd.SetIn(iotest.OneByteReader(strings.NewReader("y\ny\n")))
	out, err := runCommand(t, cmd, "install", "finance", "--keep-conflicts")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(prompted) != 1 || prompted[0] != "stripe" {
		t.Errorf("connected %v; only organisation connections are walked", prompted)
	}
	if !strings.Contains(out, "Apply this to your organisation?") || !strings.Contains(out, "Connect stripe now?") {
		t.Errorf("output:\n%s", out)
	}

	cmd = PackCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	if _, err := runCommand(t, cmd, "install", "finance", "--keep-conflicts"); exitCodeOf(err) != 1 || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("declined: %v", err)
	}
}

func TestPackInstall_StalePlanStepUpAndFailedJob(t *testing.T) {
	routes := applyRoutes()
	routes["POST /api/packs/plans/ppl_1/apply"] = jsonReply(409, `{"error":"plan_stale"}`)
	packGateway(t, routes)
	_, err := runCommand(t, PackCmd(), "install", "finance", "--yes", "--keep-conflicts")
	if err == nil || !strings.Contains(err.Error(), "changed since this plan was made") || exitCodeOf(err) != 15 {
		t.Errorf("stale: exit %d, %v", exitCodeOf(err), err)
	}

	routes["POST /api/packs/plans/ppl_1/apply"] = jsonReply(409, `{"error":"already_installed","message":"installed already"}`)
	packGateway(t, routes)
	if _, err = runCommand(t, PackCmd(), "install", "finance", "--yes", "--keep-conflicts"); err == nil || !strings.Contains(err.Error(), "installed already") {
		t.Errorf("other 409: %v", err)
	}

	routes["POST /api/packs/plans/ppl_1/apply"] = func(_ *packServer, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_id":"su_1","step_up_url":"http://`+r.Host+`/stepup/su_1"}`)
	}
	packGateway(t, routes)
	_, err = runCommand(t, PackCmd(), "install", "finance", "--yes", "--keep-conflicts")
	if exitCodeOf(err) != 14 {
		t.Errorf("step-up: exit %d, %v", exitCodeOf(err), err)
	}

	routes["POST /api/packs/plans/ppl_1/apply"] = jsonReply(202, `{"installId":"pki_1","jobId":"job_1"}`)
	routes["GET /api/jobs/job_1"] = jsonReply(200, `{"job":{"id":"job_1","status":"failed","error":"boom"}}`)
	packGateway(t, routes)
	_, err = runCommand(t, PackCmd(), "install", "finance", "--yes", "--keep-conflicts")
	if err == nil || !strings.Contains(err.Error(), "not fully applied") || !strings.Contains(err.Error(), "zuano pack uninstall finance") {
		t.Errorf("failed job: %v", err)
	}

	routes["POST /api/packs/plans/ppl_1/apply"] = jsonReply(202, `{}`)
	packGateway(t, routes)
	if _, err = runCommand(t, PackCmd(), "install", "finance", "--yes", "--keep-conflicts"); err == nil || !strings.Contains(err.Error(), "did not say which job") {
		t.Errorf("no job: %v", err)
	}
}

func TestPackInstall_NothingToChangeAndJSON(t *testing.T) {
	empty := strings.Replace(strings.Replace(planDoc, `"status":"apply"`, `"status":"noop"`, 1), `"status":"conflict"`, `"status":"noop"`, 1)
	empty = strings.Replace(empty, `"apply":1,"noop":0,"conflict":1`, `"apply":0,"noop":2,"conflict":0`, 1)
	routes := applyRoutes()
	routes["POST /api/packs/finance/plans"] = signedReply(200, empty)
	ps := packGateway(t, routes)
	out, err := runCommand(t, PackCmd(), "install", "finance")
	if err != nil || !strings.Contains(out, "Nothing to change.") {
		t.Errorf("out = %q, %v", out, err)
	}
	if ps.count("POST /api/packs/plans/ppl_1/apply") != 0 {
		t.Error("an empty plan was applied")
	}

	packGateway(t, applyRoutes())
	out, err = runCommand(t, PackCmd(), "install", "finance", "--yes", "--take-conflicts", "--json")
	if err != nil || !strings.Contains(out, `"planHash":"hash1"`) || !strings.Contains(out, `"status":"succeeded"`) {
		t.Errorf("json = %q, %v", out, err)
	}
}

func TestPack_ChangesAreRefusedForAgents(t *testing.T) {
	packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){})
	t.Setenv("ZUANO_AGENT", "1")
	dir := t.TempDir()
	for _, args := range [][]string{
		{"install", "finance"}, {"update", "finance"}, {"uninstall", "finance"}, {"rollback", "finance", "--to", "1.0.0"}, {"publish", dir},
	} {
		_, err := runCommand(t, PackCmd(), args...)
		if exitCodeOf(err) != 10 || !strings.Contains(err.Error(), "for a person") {
			t.Errorf("%v: exit %d, %v", args, exitCodeOf(err), err)
		}
	}
}

func TestPackUpdateUninstallRollback(t *testing.T) {
	routes := applyRoutes()
	ps := packGateway(t, routes)
	if _, err := runCommand(t, PackCmd(), "update", "finance", "--to-version", "1.1.0", "--yes", "--keep-conflicts"); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/packs/finance/plans", 0); b["mode"] != "update" || b["version"] != "1.1.0" {
		t.Errorf("update body = %v", b)
	}
	if _, err := runCommand(t, PackCmd(), "uninstall", "finance", "--yes", "--keep-conflicts", "--disable-connectors"); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/packs/installs/pki_1/plans", 0); b["mode"] != "uninstall" || b["disableConnectors"] != true {
		t.Errorf("uninstall body = %v", b)
	}
	if _, err := runCommand(t, PackCmd(), "rollback", "finance", "--to", "0.9.0", "--yes", "--keep-conflicts"); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/packs/installs/pki_1/plans", 1); b["mode"] != "rollback" || b["toVersion"] != "0.9.0" {
		t.Errorf("rollback body = %v", b)
	}
	if _, err := runCommand(t, PackCmd(), "rollback", "finance"); err == nil {
		t.Error("rollback without --to")
	}
	if _, err := runCommand(t, PackCmd(), "uninstall", "other", "--yes"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("uninstall of a pack that is not installed: %v", err)
	}
}

func TestPackStatus(t *testing.T) {
	routes := applyRoutes()
	routes["GET /api/packs/installs/pki_1"] = jsonReply(200, `{"install":{"id":"pki_1","status":"installed","pack":{"slug":"finance","version":"1.0.0"},
	 "ops":[{"applySeq":1,"seq":1,"kind":"risk.cell","target":{"service":"stripe"},"status":"applied"},{"applySeq":1,"seq":2,"kind":"group.create","target":{"name":"F"},"status":"skipped","reason":"needs Pro"}]}}`)
	packGateway(t, routes)
	out, err := runCommand(t, PackCmd(), "status")
	if err != nil || !strings.Contains(out, "finance  1.0.0    installed  stable   1.1.0") {
		t.Errorf("list = %q, %v", out, err)
	}
	out, err = runCommand(t, PackCmd(), "status", "finance")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"finance 1.0.0: installed", "applied", "risk.cell service=stripe", "skipped", "(needs Pro)"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}
	for _, args := range [][]string{{"status", "--json"}, {"status", "finance", "--json"}} {
		if out, err := runCommand(t, PackCmd(), args...); err != nil || !strings.Contains(out, "pki_1") {
			t.Errorf("%v = %q, %v", args, out, err)
		}
	}
}

func TestPackStatus_NoInstalls(t *testing.T) {
	packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){"GET /api/packs/installs": jsonReply(200, `{"installs":[]}`)})
	if out, err := runCommand(t, PackCmd(), "status"); err != nil || !strings.Contains(out, "No packs installed.") {
		t.Errorf("out = %q, %v", out, err)
	}
}

func TestPackAuthoring_InitValidatePublish(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mine")
	if out, err := runCommand(t, PackCmd(), "init", dir); err != nil || !strings.Contains(out, "zuano pack validate") {
		t.Fatalf("init = %q, %v", out, err)
	}
	if _, err := runCommand(t, PackCmd(), "init", dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second init: %v", err)
	}

	ps := packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"POST /api/packs/validate":        jsonReply(200, `{"errors":[{"path":"skills[0]","message":"no such skill"}]}`),
		"POST /api/packs":                 jsonReply(201, `{"pack":{"slug":"my-pack"}}`),
		"POST /api/packs/my-pack/publish": jsonReply(200, `{"ok":true}`),
	})
	if _, err := runCommand(t, PackCmd(), "validate", dir); err == nil || !strings.Contains(err.Error(), "skills[0]: no such skill") {
		t.Errorf("remote validation: %v", err)
	}
	if out, err := runCommand(t, PackCmd(), "publish", dir, "--channel", "beta"); err != nil || !strings.Contains(out, "Published my-pack 0.1.0 on the beta channel") {
		t.Errorf("publish = %q, %v", out, err)
	}
	if b := ps.body("POST /api/packs/my-pack/publish", 0); b["version"] != "0.1.0" || b["channel"] != "beta" {
		t.Errorf("publish body = %v", b)
	}
	if out, err := runCommand(t, PackCmd(), "publish", dir, "--json"); err != nil || !strings.Contains(out, `"ok":true`) {
		t.Errorf("publish json = %q, %v", out, err)
	}
	if _, err := runCommand(t, PackCmd(), "publish", dir, "--channel", "nightly"); err == nil {
		t.Error("an unknown channel was accepted")
	}

	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })
	if out, err := runCommand(t, PackCmd(), "validate", dir); err != nil || !strings.Contains(out, "my-pack 0.1.0 is valid.") {
		t.Errorf("offline validate = %q, %v", out, err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, PackCmd(), "validate", bad); err == nil {
		t.Error("a directory without pack.yaml was valid")
	}
	if err := os.WriteFile(filepath.Join(bad, "pack.yaml"), []byte("format: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, PackCmd(), "validate", bad); err == nil {
		t.Error("an invalid pack was valid")
	}
}

func TestPackParams(t *testing.T) {
	if _, err := parseParams([]string{"novalue"}, ""); err == nil {
		t.Error("--param without = accepted")
	}
	if _, err := parseParams(nil, filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing params file was accepted")
	}
	bad := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(bad, []byte("- a\n- b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseParams(nil, bad); err == nil {
		t.Error("a YAML list was accepted as parameters")
	}
}

package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/packs"
)

const assignmentsDoc = `{"revision":3,"profiles":[{"id":"spf_1","source":"pack","name":"finance","pack":{"slug":"finance","version":"1.0.0"},
"rules":[{"name":"money-safety","description":"Never send without a person.","alwaysApply":true,"agents":["all"],"body":"Show the invoice and wait for approval.","sha256":"x"},
         {"name":"claude-only","alwaysApply":true,"agents":["claude","bogus"],"body":"Only claude reads this line."}],
"skills":[{"resourceId":"res_1","versionId":"rv_1","name":"invoice-reconcile","version":"2.1.0"}],
"bundles":[{"name":"finance-toolkit","version":"1.4.0","channel":"stable"}]}],
"connectRequired":[{"slug":"google-workspace","scope":"personal","reason":"Needed by: invoice-reconcile"}]}`

const emptyAssignments = `{"revision":4,"profiles":[],"connectRequired":[]}`

type syncPackEnv struct {
	home      string
	gw        *packServer
	installed *[]string
	failSkill *bool
}

// syncPackSetup prepares agent homes and a signed-in gateway that serves the
// member's pack assignments, and records what the sync installs.
func syncPackSetup(t *testing.T, doc func() string) *syncPackEnv {
	t.Helper()
	e := &syncPackEnv{home: syncHome(t), installed: new([]string), failSkill: new(bool)}
	e.gw = packGatewayAt(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){
		"GET /api/cli/sync/assignments": func(s *packServer, w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") == `"3"` && doc() == assignmentsDoc {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			s.signed(w, 200, doc())
		},
	}, true)
	oldL, oldS, oldB := packSyncLoader, installPackSkill, installPackBundle
	packSyncLoader = loadPackSync
	installPackSkill = func(_ *cobra.Command, _ string, it packItem) error {
		if *e.failSkill {
			return errors.New("no agent")
		}
		*e.installed = append(*e.installed, "skill "+it.name+" "+it.version)
		return nil
	}
	installPackBundle = func(_ *cobra.Command, _ string, it packItem) error {
		*e.installed = append(*e.installed, "bundle "+it.name+" "+it.version)
		return nil
	}
	t.Cleanup(func() { packSyncLoader, installPackSkill, installPackBundle = oldL, oldS, oldB })
	return e
}

func fixed(doc string) func() string { return func() string { return doc } }

func (e *syncPackEnv) rules(t *testing.T, agentDir, file string) string {
	t.Helper()
	return readTestFile(t, filepath.Join(e.home, agentDir, file))
}

func TestSyncWritesPackRulesInstallsItemsAndRemembersTheRevision(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	out, err := runCommand(t, SyncCmd(), "--to", "claude,codex", "--kind", "rules", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	claude, codex := e.rules(t, ".claude", "CLAUDE.md"), e.rules(t, ".codex", "AGENTS.md")
	if !strings.Contains(claude, "zuano-pack-finance-money-safety") || !strings.Contains(claude, "zuano-pack-finance-claude-only") {
		t.Errorf("claude rules:\n%s", claude)
	}
	if !strings.Contains(codex, "zuano-pack-finance-money-safety") || strings.Contains(codex, "claude-only") {
		t.Errorf("codex rules:\n%s", codex)
	}
	if got := strings.Join(*e.installed, ","); got != "skill invoice-reconcile 2.1.0,bundle finance-toolkit 1.4.0" {
		t.Errorf("installed = %q", got)
	}
	for _, want := range []string{"Installed pack skill invoice-reconcile 2.1.0", "Connect google-workspace: `zuano connect google-workspace`"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	path, _ := packs.StatePath()
	if st, _ := packs.LoadState(path, e.gw.URL); st.Revision != 3 {
		t.Errorf("saved revision = %d", st.Revision)
	}

	*e.installed = nil
	if out, err = runCommand(t, SyncCmd(), "--to", "claude,codex", "--kind", "rules", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(*e.installed) != 0 || e.gw.count("GET /api/cli/sync/assignments") != 2 {
		t.Errorf("second sync installed %v after %d fetches", *e.installed, e.gw.count("GET /api/cli/sync/assignments"))
	}
	if !strings.Contains(e.rules(t, ".claude", "CLAUDE.md"), "zuano-pack-finance-money-safety") {
		t.Error("the pack rules were dropped on an unchanged revision")
	}
}

func TestSyncRemovesRulesOfAPackThatIsNoLongerAssignedAndSaysWhatToUninstall(t *testing.T) {
	doc := assignmentsDoc
	e := syncPackSetup(t, func() string { return doc })
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes"); err != nil {
		t.Fatal(err)
	}
	doc = emptyAssignments
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(e.rules(t, ".claude", "CLAUDE.md"), "zuano-pack-finance") {
		t.Error("a rule of a removed pack is still in CLAUDE.md")
	}
	for _, want := range []string{"skill invoice-reconcile is no longer assigned: remove it with `zuano skill uninstall invoice-reconcile`", "bundle finance-toolkit is no longer assigned: remove it with `zuano plugin remove finance-toolkit`"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSyncKeepsAnEditedPackRuleBlockInsteadOfOverwritingIt(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.home, ".claude", "CLAUDE.md")
	edited := strings.Replace(readTestFile(t, path), "Show the invoice and wait for approval.", "Do whatever.", 1)
	writeTestFile(t, path, edited)
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(readTestFile(t, path), "Do whatever.") || !strings.Contains(out, "conflict") {
		t.Errorf("an edited block was overwritten or not reported:\n%s", out)
	}
}

func TestSyncDoesNotRecordTheRevisionWhenAnItemFailsToInstall(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	*e.failSkill = true
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes")
	if exitCodeOf(err) != 1 || !strings.Contains(err.Error(), "1 pack item(s) were not installed") || !strings.Contains(out, "warning: pack skill invoice-reconcile was not installed: no agent") {
		t.Errorf("exit %d, %v\n%s", exitCodeOf(err), err, out)
	}
	path, _ := packs.StatePath()
	if st, _ := packs.LoadState(path, e.gw.URL); st.Revision != 0 {
		t.Errorf("revision %d recorded after a failure", st.Revision)
	}
}

func TestSyncRefusesPackRulesThatDoNotVerify(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	e.gw.replace(func(w http.ResponseWriter, _ *http.Request) {
		e.gw.signer.Sign(w.Header(), []byte(assignmentsDoc))
		_, _ = io.WriteString(w, strings.Replace(assignmentsDoc, "Show the invoice", "Send the invoice", 1))
	})
	_, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes")
	if exitCodeOf(err) != 4 {
		t.Errorf("exit %d, %v", exitCodeOf(err), err)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.home, ".claude", "CLAUDE.md")); strings.Contains(string(raw), "Send the invoice") {
		t.Error("an unverified rule was written")
	}
}

func TestSyncSkipsPacksWhenAskedOrWhenTheGatewayCannotBeAsked(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes", "--no-packs"); err != nil {
		t.Fatal(err)
	}
	if e.gw.count("GET /api/cli/sync/assignments") != 0 {
		t.Error("--no-packs fetched the assignments")
	}

	SetOffline(true)
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes"); err != nil {
		t.Fatal(err)
	}
	SetOffline(false)
	if e.gw.count("GET /api/cli/sync/assignments") != 0 {
		t.Error("--offline fetched the assignments")
	}

	e.gw.replace(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes")
	if err != nil || !strings.Contains(out, "warning: packs were not synced") {
		t.Errorf("a failing gateway: %v\n%s", err, out)
	}

	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_EMAIL", "")
	out, err = runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes")
	if err != nil || strings.Contains(out, "packs were not synced") {
		t.Errorf("not signed in: %v\n%s", err, out)
	}
}

func TestSyncDryRunPreviewsPackItemsAndWritesNothing(t *testing.T) {
	e := syncPackSetup(t, fixed(assignmentsDoc))
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"[dry-run] would install skill invoice-reconcile 2.1.0", "[dry-run] would install bundle finance-toolkit 1.4.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(*e.installed) != 0 {
		t.Errorf("a dry run installed %v", *e.installed)
	}
	path, _ := packs.StatePath()
	if st, _ := packs.LoadState(path, e.gw.URL); st.Revision != 0 {
		t.Error("a dry run recorded the revision")
	}
}

func TestPackRulesIgnoreUnknownAgentsAndAll(t *testing.T) {
	var p packs.Profile
	p.Pack.Slug = "x"
	p.Rules = []packs.AssignmentRule{{Name: "r", Agents: []string{"all", "nope", "codex"}, Body: "b"}}
	rules := packRules([]packs.Profile{p})
	if len(rules) != 1 || len(rules[0].Agents) != 1 || rules[0].Agents[0] != "codex" || rules[0].Name != "zuano-pack-x-r" {
		t.Errorf("rules = %+v", rules)
	}
}

func TestDoctorPacksReportsRevisionDrift(t *testing.T) {
	doc := assignmentsDoc
	e := syncPackSetup(t, func() string { return doc })
	c, err := approvalsClient("")
	if err != nil {
		t.Fatal(err)
	}
	check := func() (bool, string) {
		var ok bool
		var detail string
		doctorPacks(context.Background(), c, func(name string, o bool, d string) {
			if name != "packs" {
				t.Errorf("check name %q", name)
			}
			ok, detail = o, d
		})
		return ok, detail
	}
	if ok, d := check(); ok || !strings.Contains(d, "revision 3 is available and 0 was last synced") {
		t.Errorf("never synced: %v %q", ok, d)
	}
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes"); err != nil {
		t.Fatal(err)
	}
	if ok, d := check(); !ok || !strings.Contains(d, "1 pack(s), revision 3 is synced") {
		t.Errorf("synced: %v %q", ok, d)
	}
	doc = emptyAssignments
	if ok, _ := check(); ok {
		t.Error("a newer revision passed")
	}
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "rules", "--yes"); err != nil {
		t.Fatal(err)
	}
	if ok, d := check(); !ok || !strings.Contains(d, "0 pack(s)") {
		t.Errorf("after removal: %v %q", ok, d)
	}

	e.gw.replace(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	if ok, d := check(); !ok || !strings.Contains(d, "not available from this gateway") {
		t.Errorf("unreachable: %v %q", ok, d)
	}
	e.gw.replace(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "[") })
	if ok, d := check(); ok || !strings.Contains(d, "cannot be read") {
		t.Errorf("garbage: %v %q", ok, d)
	}
}

func TestDoctorPacksReportsNoPacksAssigned(t *testing.T) {
	syncPackSetup(t, fixed(`{"revision":0,"profiles":[]}`))
	c, err := approvalsClient("")
	if err != nil {
		t.Fatal(err)
	}
	doctorPacks(context.Background(), c, func(_ string, ok bool, d string) {
		if !ok || d != "no packs assigned" {
			t.Errorf("%v %q", ok, d)
		}
	})
}

func TestInstallingPackItemsFromTheGatewayFailsClosed(t *testing.T) {
	packGateway(t, map[string]func(*packServer, http.ResponseWriter, *http.Request){}).lenient = true
	cmd := SyncCmd()
	cmd.SetContext(context.Background())
	if err := installPackBundleFromGateway(cmd, "", packItem{kind: "bundle", name: "toolkit", version: "1.0.0"}); err == nil {
		t.Error("a bundle installed from a gateway with no feed")
	}
	if err := installPackSkillFromGateway(cmd, "", packItem{kind: "skill", name: "s", version: "1.0.0", resourceID: "res_1"}); err == nil {
		t.Error("a skill installed from a gateway with no catalog")
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/packs"
)

func formFields(t *testing.T, req *capturedRequest) map[string]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(req.contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(req.body), params["boundary"])
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return fields
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(part)
		fields[part.FormName()] = string(b)
	}
}

func TestPublishSendsTheScopeOfASkill(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, json.RawMessage(`{"resourceId":"res_p1","versionId":"ver_1","semver":"1.0.0","state":"published","scope":"personal"}`))
	defer srv.Close()
	path := writeSkillFile(t, t.TempDir(), "SKILL.md")
	doer := &publishTestDoer{srv: srv}
	var out bytes.Buffer
	if err := runPublish(doer, path, publishOpts{channel: "stable", scope: "personal", out: &out}); err != nil {
		t.Fatal(err)
	}
	if f := formFields(t, doer.lastRequest()); f["scope"] != "personal" {
		t.Errorf("fields = %v", f)
	}
	for _, want := range []string{"Scope:       personal", "Only you can see this skill", "zuano sync", "zuano skill promote res_p1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// Without the flag the gateway chooses by role, and the choice is shown.
	out.Reset()
	if err := runPublish(doer, path, publishOpts{channel: "stable", out: &out}); err != nil {
		t.Fatal(err)
	}
	if f := formFields(t, doer.lastRequest()); f["scope"] != "" {
		if _, present := f["scope"]; present {
			t.Errorf("a scope was sent without the flag: %v", f)
		}
	}
	if !strings.Contains(out.String(), "Scope:       personal") {
		t.Errorf("the gateway's choice is not shown:\n%s", out.String())
	}

	out.Reset()
	doer.srv = buildPublishServer(t, http.StatusCreated, json.RawMessage(`{"resourceId":"res_o1","versionId":"ver_2","semver":"1.0.0","state":"pending_review","scope":"org","reviewUrl":"https://gw.example/review/ver_2"}`))
	if err := runPublish(doer, path, publishOpts{channel: "stable", scope: "org", out: &out}); err != nil || !strings.Contains(out.String(), "An administrator reviews it first: https://gw.example/review/ver_2") {
		t.Errorf("org scope: %v\n%s", err, out.String())
	}

	if err := runPublish(doer, path, publishOpts{channel: "stable", scope: "team"}); err == nil || !strings.Contains(err.Error(), "personal or org") {
		t.Errorf("bad scope: %v", err)
	}
	agentPath := filepath.Join(t.TempDir(), "agent.md")
	if err := os.WriteFile(agentPath, []byte("---\ntype: agent\nname: A\nslug: a\nversion: 1.0.0\n---\nDo things.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runPublish(doer, agentPath, publishOpts{channel: "stable", scope: "personal"}); err == nil || !strings.Contains(err.Error(), "applies to skills") {
		t.Errorf("a scope on an agent: %v", err)
	}
	var dry bytes.Buffer
	if err := runPublish(nil, path, publishOpts{channel: "stable", scope: "personal", dryRun: true, out: &dry}); err != nil || !strings.Contains(dry.String(), "[dry-run] scope:   personal") {
		t.Errorf("dry run: %v\n%s", err, dry.String())
	}
}

func TestSkillListPersonalAndPromote(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/marketplace/skills":                    jsonReply(200, `{"skills":[{"id":"res_p1","slug":"mine","name":"My skill","version":"1.0.0","sizeBytes":2048,"promotion":{"state":"pending_review"}},{"id":"res_p2","name":"Other","version":"0.1.0","sizeBytes":12}]}`),
		"POST /api/marketplace/resources/res_p1/promote": jsonReply(202, `{"versionId":"ver_9","state":"pending_review","reviewUrl":"/marketplace/review/ver_9"}`),
		"POST /api/marketplace/resources/res_p2/promote": jsonReply(402, `{"error":"plan_required","feature":"marketplace.publish_private","message":"Publishing to the organisation needs the Pro plan."}`),
	})
	out, err := runCommand(t, SkillListCmd(), "--personal")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"res_p1", "My skill", "2.0 KiB", "pending_review", "12 B", "zuano sync"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if out, err := runCommand(t, SkillListCmd(), "--personal", "--json"); err != nil || !strings.Contains(out, `"skills"`) {
		t.Errorf("--json: %v", err)
	}
	out, err = runCommand(t, SkillPromoteCmd(), "res_p1")
	if err != nil || !strings.Contains(out, "Submitted res_p1 to your organisation (pending_review)") || !strings.Contains(out, gw.URL+"/marketplace/review/ver_9") {
		t.Errorf("promote: %v\n%s", err, out)
	}
	if _, err := runCommand(t, SkillPromoteCmd(), "res_p2"); exitCodeOf(err) != client.ExitPlanRequired {
		t.Errorf("a plan without private publishing: exit %d (%v)", exitCodeOf(err), err)
	}
	if _, err := runCommand(t, SkillPromoteCmd(), "a/b"); err == nil {
		t.Error("a path in the id")
	}
	if out, err := runCommand(t, SkillPromoteCmd(), "res_p1", "--json"); err != nil || !strings.Contains(out, "versionId") {
		t.Errorf("promote --json: %v", err)
	}
}

func TestSkillListPersonalWhenThereAreNone(t *testing.T) {
	a2aGateway(t, routeFns{"GET /api/marketplace/skills": jsonReply(200, `{"skills":[]}`)})
	if out, err := runCommand(t, SkillListCmd(), "--personal"); err != nil || !strings.Contains(out, "No personal skills") {
		t.Errorf("empty: %v %q", err, out)
	}
}

func TestSyncInstallsPersonalSkillsAndReportsTheirRemoval(t *testing.T) {
	ps := &packSync{
		gatewayURL: "https://gw.example",
		state: packs.State{Assignments: packs.Assignments{PersonalSkills: []packs.AssignmentSkill{
			{ResourceID: "res_old", Name: "gone", Version: "1.0.0"}, {ResourceID: "res_keep", Name: "keep", Version: "1.0.0"},
		}}},
		assigned: packs.Assignments{PersonalSkills: []packs.AssignmentSkill{
			{ResourceID: "res_keep", Name: "keep", Version: "1.1.0"}, {ResourceID: "res_new", Name: "fresh", Version: "0.1.0"},
		}},
	}
	install, removed := ps.pending()
	if len(install) != 2 || !install[0].personal || install[0].name != "keep" || install[1].name != "fresh" {
		t.Errorf("install = %+v", install)
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "personal skill gone is gone from your account") {
		t.Errorf("removed = %v", removed)
	}
	var out bytes.Buffer
	ps.preview(&out)
	if !strings.Contains(out.String(), "[dry-run] would install skill keep 1.1.0 from your personal skills") {
		t.Errorf("preview:\n%s", out.String())
	}
}

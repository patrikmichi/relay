package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

const routeJSON = `{"id":"rte_1","name":"Finance writes","priority":10,"match":{"services":["stripe","fakturoid"],"tools":["create_*"],"risks":["write","outward"],"sources":["agent"],"routeHints":["finance"]},
"targets":[{"kind":"dm_requester"},{"kind":"dm_group","groupId":"grp_fin"},{"kind":"channel","installationId":"chi_1","conversationId":"C1","name":"#finance"}],
"restrictDeciders":true,"fourEyes":true,"detail":"keys","reminders":[0.5]}`

func TestApprovalsRoutes(t *testing.T) {
	var testBody map[string]any
	ps := packGateway(t, routeMap{
		"GET /api/admin/approval-routes":       jsonReply(200, `{"routes":[`+routeJSON+`,{"id":"rte_d","name":"Default","priority":999,"default":true,"match":{},"targets":[{"kind":"dm_requester"}]}]}`),
		"GET /api/admin/approval-routes/rte_1": jsonReply(200, `{"route":`+routeJSON+`}`),
		"POST /api/admin/approval-routes/test": jsonReply(200, `{"route":{"name":"Finance writes","detail":"keys"},
		 "recipients":[{"kind":"dm_requester","name":"Ann","channel":"slack","status":"mapped"},{"kind":"dm_group","name":"Bob","channel":"","status":"unmapped"}],
		 "channels":[{"installation":"chi_1","name":"#finance","warning":"public channel: detail limited"}]}`),
	})
	out, err := runCommand(t, ApprovalsCmd(), "routes", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rte_1", "Finance writes", "stripe,fakturoid", "write,outward", "dm_requester, dm_group grp_fin, channel #finance", "Default (default)", "any"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, ApprovalsCmd(), "routes", "show", "rte_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Route:     Finance writes (rte_1)", "Hints:     finance", "only the route's groups", "the requester cannot decide their own call", "Detail:    keys"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, ApprovalsCmd(), "routes", "test", "--service", "stripe", "--tool", "create_refund", "--risk", "write", "--source", "agent", "--requester", "ann@example.com")
	if err != nil {
		t.Fatal(err)
	}
	testBody = ps.body("POST /api/admin/approval-routes/test", 0)
	if testBody["service"] != "stripe" || testBody["tool"] != "create_refund" || testBody["risk"] != "write" || testBody["source"] != "agent" || testBody["requester"] != "ann@example.com" {
		t.Errorf("body = %v", testBody)
	}
	for _, want := range []string{`Matches route "Finance writes"`, "dm_requester Ann via slack: mapped", "dm_group Bob via -: unmapped", "channel #finance (chi_1): public channel", "Nothing was sent."} {
		if !strings.Contains(out, want) {
			t.Errorf("test lacks %q:\n%s", want, out)
		}
	}
	for _, args := range [][]string{{"list", "--json"}, {"show", "rte_1", "--json"}, {"test", "--service", "a", "--tool", "b", "--risk", "read", "--json"}} {
		if out, err := runCommand(t, ApprovalsCmd(), append([]string{"routes"}, args...)...); err != nil || !strings.Contains(out, "{") {
			t.Errorf("%v: %q %v", args, out, err)
		}
	}
	for _, args := range [][]string{{"test"}, {"test", "--service", "a", "--tool", "b", "--risk", "bogus"}} {
		if _, err := runCommand(t, ApprovalsCmd(), append([]string{"routes"}, args...)...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestApprovalsRoutes_AgentSessionsMayRead(t *testing.T) {
	packGateway(t, routeMap{"GET /api/admin/approval-routes": jsonReply(200, `{"routes":[]}`)})
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, ApprovalsCmd(), "routes", "list"); err != nil {
		t.Errorf("agent read refused: %v", err)
	}
}

func TestApprovalsShow_PrintsWhereItWasSent(t *testing.T) {
	packGateway(t, routeMap{"GET /api/approvals/apr_1": jsonReply(200, `{"approval":{"approval_id":"apr_1","status":"pending","service":"stripe","tool":"create_refund","risk":"write",
	 "notified":[{"channel":"slack","target":"#finance","state":"delivered"},{"channel":"email","state":""}]},"args_preview":null}`)})
	out, err := runCommand(t, ApprovalsCmd(), "show", "apr_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Sent to:   slack #finance: delivered", "           email: -"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
}

func chatRoutes(installs, identities string) routeMap {
	return routeMap{
		"GET /api/me/chat-identities":       jsonReply(200, `{"identities":`+identities+`}`),
		"GET /api/admin/chat/installations": jsonReply(200, `{"installations":`+installs+`}`),
		"GET /api/admin/chat/reachability":  jsonReply(200, `{"eligible":5,"reachable":3}`),
	}
}

func TestChatStatus(t *testing.T) {
	packGateway(t, chatRoutes(`[{"id":"chi_1","provider":"slack","workspace":"Acme","status":"active"},{"id":"chi_2","provider":"teams","workspace":"Acme Tenant","status":"consent_pending","detail":"admin consent needed"}]`,
		`[{"id":"cid_1","provider":"slack","workspace":"Acme","name":"ann","dmOptOut":true}]`))
	out, err := runCommand(t, ChatCmd(), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Slack", "Acme", "Microsoft Teams", "admin consent needed", "3 of 5 approvers can be reached", "Linked: Slack as ann in Acme (direct messages off)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if out, err := runCommand(t, ChatCmd(), "status", "--json"); err != nil || !strings.Contains(out, `"reachability":{"eligible":5`) {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestChatStatus_MemberWithoutAdminViewsAndNoLinks(t *testing.T) {
	routes := chatRoutes(`[]`, `[]`)
	routes["GET /api/admin/chat/installations"] = jsonReply(403, `{"error":"forbidden"}`)
	packGateway(t, routes)
	out, err := runCommand(t, ChatCmd(), "status")
	if err != nil || !strings.Contains(out, "No chat app is set up") || !strings.Contains(out, "zuano chat link slack") {
		t.Errorf("%q %v", out, err)
	}
}

func TestChatLink(t *testing.T) {
	ps := packGateway(t, routeMap{})
	out, err := runCommand(t, ChatCmd(), "link", "slack")
	if err != nil || !strings.Contains(out, ps.URL+"/api/chat/link/slack/start") || !strings.Contains(out, "Link your Slack account there") {
		t.Errorf("slack: %q %v", out, err)
	}
	if out, err := runCommand(t, ChatCmd(), "link", "teams", "--no-browser"); err != nil || !strings.Contains(out, "/api/chat/link/teams/start") {
		t.Errorf("teams: %q %v", out, err)
	}
	if _, err := runCommand(t, ChatCmd(), "link", "discord"); err == nil {
		t.Error("an unknown app was accepted")
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, ChatCmd(), "link", "slack"); exitCodeOf(err) != 14 {
		t.Errorf("agent: exit %d, %v", exitCodeOf(err), err)
	}
}

func TestDoctorChat(t *testing.T) {
	ps := packGateway(t, routeMap{})
	var installs, identities = `[{"provider":"slack"},{"provider":"slack"},{"provider":"teams"}]`, `[{"provider":"slack"}]`
	status := 200
	ps.replace(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		switch r.URL.Path {
		case "/api/me/chat-identities":
			_, _ = io.WriteString(w, `{"identities":`+identities+`}`)
		case "/api/admin/chat/installations":
			_, _ = io.WriteString(w, `{"installations":`+installs+`}`)
		default:
			w.WriteHeader(404)
		}
	})
	c, err := approvalsClient("")
	if err != nil {
		t.Fatal(err)
	}
	check := func() (bool, string) {
		var ok bool
		var d string
		doctorChat(context.Background(), c, func(_ string, o bool, detail string) { ok, d = o, detail })
		return ok, d
	}
	if ok, d := check(); !ok || d != "approvals reachable in: Slack (linked), Microsoft Teams (not linked)" {
		t.Errorf("linked: %v %q", ok, d)
	}
	installs = `[]`
	if _, d := check(); d != "no chat app installed" {
		t.Errorf("none: %q", d)
	}
	status = 500
	if ok, d := check(); !ok || !strings.Contains(d, "not available") {
		t.Errorf("unavailable: %v %q", ok, d)
	}
}

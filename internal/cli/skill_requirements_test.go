package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentsync"
)

// requirementsGateway serves a catalog skill (res_req) that needs the
// connectors in required, plus the requirements and search routes.
type requirementsGateway struct {
	*httptest.Server
	mu        sync.Mutex
	connected map[string]bool
	required  []string
	status    int
	checks    int
}

func newRequirementsGateway(t *testing.T, required ...string) *requirementsGateway {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")
	t.Setenv("RELAY_AGENT", "")
	g := &requirementsGateway{connected: map[string]bool{}, required: required, status: http.StatusOK}
	bundle := buildCanonicalSkillBundle(t, "weekly-timesheet")
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch r.URL.Path {
		case "/api/catalog/skills/res_req/download":
			w.Header().Set("X-Skill-Content-Sha256", sha256HexOfBundle(bundle))
			w.Header().Set("X-Skill-Scan-Verdict", "approved")
			w.Header().Set("X-Skill-Version", "1.2.0")
			w.Header().Set("X-Skill-Catalog-Id", "res_req")
			_, _ = w.Write(bundle)
		case "/api/marketplace/skills/res_req/requirements":
			g.checks++
			if g.status != http.StatusOK {
				w.WriteHeader(g.status)
				return
			}
			var reqs []map[string]any
			var missing []string
			for _, slug := range g.required {
				reqs = append(reqs, map[string]any{"slug": slug, "scopes": []string{"read"}, "connected": g.connected[slug]})
				if !g.connected[slug] {
					missing = append(missing, slug)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"requirements": reqs, "satisfied": len(missing) == 0, "missing": append([]string{}, missing...),
				"skillId": "res_req", "version": "1.2.0",
			})
		case "/api/catalog/skills/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": []map[string]any{
				{"id": "res_req", "slug": "weekly-timesheet", "description": "Summarises last week", "version": "1.2.0", "trustScore": 90},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

// stubConnect replaces the connect flow with one that links the connector on
// the gateway, and records the slugs it was asked for.
func (g *requirementsGateway) stubConnect(t *testing.T, succeed bool) *[]string {
	t.Helper()
	var asked []string
	prev := connectConnector
	connectConnector = func(_ *cobra.Command, slug, _ string) error {
		asked = append(asked, slug)
		if succeed {
			g.mu.Lock()
			g.connected[slug] = true
			g.mu.Unlock()
		}
		return nil
	}
	t.Cleanup(func() { connectConnector = prev })
	return &asked
}

func runInstall(t *testing.T, g *requirementsGateway, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(append([]string{"res_req", "--to", "claude", "--gateway-url", g.URL}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func installedSkillFile() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "skills", "weekly-timesheet", "SKILL.md")
}

func TestSkillInstall_SatisfiedRequirementsInstallWithoutPrompt(t *testing.T) {
	g := newRequirementsGateway(t, "clockify")
	g.connected["clockify"] = true
	asked := g.stubConnect(t, true)

	out, err := runInstall(t, g, "")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if len(*asked) != 0 || strings.Contains(out, "Connect") {
		t.Errorf("connected skill prompted: asked=%v\n%s", *asked, out)
	}
	if _, err := os.Stat(installedSkillFile()); err != nil {
		t.Errorf("skill not installed: %v", err)
	}
}

func TestSkillInstall_MissingConnectorOfferedThenRechecked(t *testing.T) {
	g := newRequirementsGateway(t, "clockify", "slack")
	g.connected["slack"] = true
	asked := g.stubConnect(t, true)

	out, err := runInstall(t, g, "y\n")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if strings.Join(*asked, ",") != "clockify" {
		t.Errorf("connect asked for %v, want only clockify", *asked)
	}
	for _, want := range []string{"clockify", "Connect clockify now? [Y/n]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if g.checks != 2 {
		t.Errorf("requirements checked %d times, want 2 (before and after connecting)", g.checks)
	}
	if _, err := os.Stat(installedSkillFile()); err != nil {
		t.Errorf("skill not installed after connecting: %v", err)
	}
}

func TestSkillInstall_DecliningOrEmptyInputFailsWithoutWriting(t *testing.T) {
	for name, stdin := range map[string]string{"no": "n\n", "end of input": ""} {
		t.Run(name, func(t *testing.T) {
			g := newRequirementsGateway(t, "clockify")
			asked := g.stubConnect(t, true)
			out, err := runInstall(t, g, stdin)
			if err == nil || !strings.Contains(err.Error(), "clockify") || !strings.Contains(err.Error(), "--ignore-requirements") {
				t.Fatalf("err = %v\n%s", err, out)
			}
			if len(*asked) != 0 {
				t.Errorf("connected %v after a decline", *asked)
			}
			if _, statErr := os.Stat(installedSkillFile()); !os.IsNotExist(statErr) {
				t.Errorf("skill installed despite unmet requirements: %v", statErr)
			}
		})
	}
}

func TestSkillInstall_YesConnectsWithoutAsking(t *testing.T) {
	g := newRequirementsGateway(t, "clockify", "slack")
	asked := g.stubConnect(t, true)

	out, err := runInstall(t, g, "", "--yes")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if strings.Join(*asked, ",") != "clockify,slack" || strings.Contains(out, "[Y/n]") {
		t.Errorf("asked=%v\n%s", *asked, out)
	}
}

func TestSkillInstall_NoConnectOnlyReports(t *testing.T) {
	g := newRequirementsGateway(t, "clockify")
	asked := g.stubConnect(t, true)

	out, err := runInstall(t, g, "y\n", "--no-connect")
	if err == nil {
		t.Fatalf("install succeeded with a missing connector\n%s", out)
	}
	if len(*asked) != 0 || strings.Contains(out, "[Y/n]") || g.checks != 1 {
		t.Errorf("asked=%v checks=%d\n%s", *asked, g.checks, out)
	}
	if !strings.Contains(out, "you have not connected: clockify") {
		t.Errorf("missing connectors not listed:\n%s", out)
	}
}

func TestSkillInstall_StillUnmetFailsUnlessIgnored(t *testing.T) {
	g := newRequirementsGateway(t, "clockify")
	g.stubConnect(t, false)

	if out, err := runInstall(t, g, "y\n"); err == nil {
		t.Fatalf("install succeeded although connect failed\n%s", out)
	}
	if _, err := os.Stat(installedSkillFile()); !os.IsNotExist(err) {
		t.Fatalf("skill installed: %v", err)
	}

	out, err := runInstall(t, g, "y\n", "--ignore-requirements")
	if err != nil {
		t.Fatalf("--ignore-requirements: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning: installing without the connectors it needs (clockify)") {
		t.Errorf("no warning:\n%s", out)
	}
	if _, err := os.Stat(installedSkillFile()); err != nil {
		t.Errorf("skill not installed with --ignore-requirements: %v", err)
	}
}

func TestSkillInstall_DryRunReportsWithoutConnecting(t *testing.T) {
	g := newRequirementsGateway(t, "clockify")
	asked := g.stubConnect(t, true)

	out, err := runInstall(t, g, "y\n", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if len(*asked) != 0 || !strings.Contains(out, "clockify") {
		t.Errorf("asked=%v\n%s", *asked, out)
	}
	if _, err := os.Stat(installedSkillFile()); !os.IsNotExist(err) {
		t.Errorf("dry run wrote the skill: %v", err)
	}
}

func TestSkillInstall_UnavailableRequirementsDoNotBlock(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		g := newRequirementsGateway(t, "clockify")
		g.status = status
		out, err := runInstall(t, g, "")
		if err != nil {
			t.Fatalf("status %d blocked the install: %v\n%s", status, err, out)
		}
		if _, err := os.Stat(installedSkillFile()); err != nil {
			t.Errorf("status %d: skill not installed: %v", status, err)
		}
	}
}

func TestSkillInstall_YesAndNoConnectConflict(t *testing.T) {
	g := newRequirementsGateway(t, "clockify")
	if _, err := runInstall(t, g, "", "--yes", "--no-connect"); err == nil {
		t.Fatal("--yes with --no-connect was accepted")
	}
}

func TestSkillInfo_ShowsRequirementsAndStatus(t *testing.T) {
	g := newRequirementsGateway(t, "clockify", "slack")
	g.connected["slack"] = true

	out, err := runCommand(t, SkillInfoCmd(), "weekly-timesheet", "--gateway-url", g.URL)
	if err != nil {
		t.Fatalf("info: %v\n%s", err, out)
	}
	for _, want := range []string{"res_req", "weekly-timesheet", "1.2.0", "clockify", "missing", "slack", "connected", "not ready, connect: clockify", "Installed:   no"} {
		if !strings.Contains(out, want) {
			t.Errorf("info lacks %q:\n%s", want, out)
		}
	}

	g.connected["clockify"] = true
	out, err = runCommand(t, SkillInfoCmd(), "res_req", "--gateway-url", g.URL)
	if err != nil || !strings.Contains(out, "ready, every connector is connected") {
		t.Errorf("ready status: %v\n%s", err, out)
	}
}

func TestSkillInfo_JSON(t *testing.T) {
	g := newRequirementsGateway(t, "clockify")
	stdout, stderr, err := runSplit(t, SkillInfoCmd(), "weekly-timesheet", "--json", "--gateway-url", g.URL)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Command string `json:"command"`
		Skill   struct {
			ID string `json:"id"`
		} `json:"skill"`
		Requirements struct {
			Satisfied bool     `json:"satisfied"`
			Missing   []string `json:"missing"`
		} `json:"requirements"`
		Installed []installedSkill `json:"installed"`
	}
	decodeOne(t, stdout, &doc)
	if doc.Command != "skill info" || doc.Skill.ID != "res_req" || doc.Requirements.Satisfied || len(doc.Requirements.Missing) != 1 || doc.Installed == nil {
		t.Errorf("doc = %+v", doc)
	}
	if !strings.Contains(stderr, "Requires:") {
		t.Errorf("the text view belongs on stderr, got %q", stderr)
	}

	stdout, _, err = runSplit(t, SkillInfoCmd(), "no-such-skill", "--json", "--gateway-url", g.URL)
	if err == nil {
		t.Fatal("an unknown skill succeeded")
	}
	var failed map[string]any
	decodeOne(t, stdout, &failed)
	if failed["command"] != "skill info" || failed["error"] == nil {
		t.Errorf("a failed info must say why in the document: %v", failed)
	}
}

func TestSkillInfo_UnknownSkill(t *testing.T) {
	g := newRequirementsGateway(t)
	if _, err := runCommand(t, SkillInfoCmd(), "no-such-skill", "--gateway-url", g.URL); err == nil || !strings.Contains(err.Error(), "no-such-skill") {
		t.Fatalf("err = %v", err)
	}
}

func TestSkillFrontmatterRequires(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"objects":         {"---\nname: x\nrequires:\n  connectors:\n    - slug: clockify\n      scopes: [read]\n    - slug: slack\n---\nbody", "clockify,slack"},
		"scalars":         {"---\nrequires:\n  connectors: [notion]\n---\nbody", "notion"},
		"none":            {"---\nname: x\n---\nbody", ""},
		"no front matter": {"just text", ""},
		"invalid yaml":    {"---\nrequires: [\n---\n", ""},
		"crlf":            {"---\r\nrequires:\r\n  connectors:\r\n    - slug: a\r\n---\r\n", "a"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(skillFrontmatterRequires([]byte(tc.in)), ","); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSync_WarnsAboutSkillsThatRequireConnectors(t *testing.T) {
	home := syncHome(t)
	writeSkillFiles(t, filepath.Join(home, ".claude", "skills", "timesheet"), map[string][]byte{
		"SKILL.md": []byte("---\nname: timesheet\ndescription: weekly hours\nrequires:\n  connectors:\n    - slug: clockify\n---\nbody\n"),
	})

	out, err := runCommand(t, SyncCmd(), "--to", "claude,cursor", "--kind", "skills", "--dry-run")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"timesheet" requires connectors clockify`) {
		t.Errorf("no requirements warning:\n%s", out)
	}

	out, err = runCommand(t, SyncCmd(), "--to", "claude,cursor", "--kind", "rules", "--dry-run")
	if err != nil || strings.Contains(out, "requires connectors") {
		t.Errorf("rules-only sync warned about skills: %v\n%s", err, out)
	}
}

func TestSkillRequirementWarnings_NameLocalSkillsThatNeedConnectors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSkillFiles(t, filepath.Join(home, ".claude", "skills", "timesheet"), map[string][]byte{
		"SKILL.md": []byte("---\nname: timesheet\ndescription: weekly hours\nrequires:\n  connectors:\n    - slug: clockify\n---\nbody\n"),
	})
	writeSkillFiles(t, filepath.Join(home, ".claude", "skills", "plain"), map[string][]byte{
		"SKILL.md": []byte("---\nname: plain\ndescription: nothing needed\n---\nbody\n"),
	})

	warns := skillRequirementWarnings("", []agentsync.Scope{agentsync.ScopeUser})
	if len(warns) != 1 || !strings.Contains(warns[0], `"timesheet"`) || !strings.Contains(warns[0], "clockify") || !strings.Contains(warns[0], "relay connect") {
		t.Fatalf("warnings = %q", warns)
	}
}

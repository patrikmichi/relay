package cli

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const auditBase = "GET /api/org/current/observability/audit?"

func auditEventJSON(n int, tool string) string {
	return `{"ts":"2026-10-1` + string(rune('0'+n)) + `T10:00:00Z","event":"tool_call","user_email":"dev@example.com","service":"stripe","tool":"` + tool + `","outcome":"ok","risk":"read","duration_ms":12,"approval_id":null,"request_id":"req_` + string(rune('0'+n)) + `","trace_id":"t"}`
}

func TestAuditExport_PagesThroughTheCursorAndWritesJSONLines(t *testing.T) {
	first := `{"events":[` + auditEventJSON(1, "a") + `,` + auditEventJSON(2, "b") + `],"next_cursor":"c1"}`
	second := `{"events":[` + auditEventJSON(3, "c") + `],"next_cursor":null}`
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		auditBase + "from=2026-10-01&limit=200&service=stripe&to=2026-10-16&user=dev%40example.com":           reply(200, first),
		auditBase + "from=2026-10-01&limit=200&service=stripe&to=2026-10-16&user=dev%40example.com&cursor=c1": reply(200, second),
	})
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	_, err := runCommand(t, AuditCmd(), "export", "--from", "2026-10-01", "--to", "2026-10-16", "--service", "stripe", "--user", "dev@example.com", "-o", path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"tool":"c"`) {
		t.Fatalf("file = %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".cursor"); err == nil {
		t.Error("the position file survived a finished export")
	}
}

func TestAuditExport_CSVHasAHeaderAndDefusesFormulas(t *testing.T) {
	page := `{"events":[` + strings.Replace(auditEventJSON(1, "=HYPERLINK(1)"), `"approval_id":null`, `"approval_id":"apr_1"`, 1) + `],"next_cursor":null}`
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){auditBase + "limit=200": reply(200, page)})
	out, err := runCommand(t, AuditCmd(), "export", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "ts,event,user_email,service,tool,outcome,risk,duration_ms,approval_id,request_id,trace_id" {
		t.Fatalf("csv = %q", out)
	}
	if want := "2026-10-11T10:00:00Z,tool_call,dev@example.com,stripe,'=HYPERLINK(1),ok,read,12,apr_1,req_1,t"; lines[1] != want {
		t.Errorf("row = %q, want %q", lines[1], want)
	}
}

func TestAuditExport_ResumesFromTheSavedCursor(t *testing.T) {
	var failSecond atomic.Bool
	failSecond.Store(true)
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		auditBase + "limit=200": reply(200, `{"events":[`+auditEventJSON(1, "a")+`],"next_cursor":"c1"}`),
		auditBase + "limit=200&cursor=c1": func(w http.ResponseWriter, _ map[string]any) {
			if failSecond.Load() {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":"upstream"}`))
				return
			}
			_, _ = w.Write([]byte(`{"events":[` + auditEventJSON(2, "b") + `],"next_cursor":null}`))
		},
	})
	path := filepath.Join(t.TempDir(), "audit.csv")
	_, err := runCommand(t, AuditCmd(), "export", "--format", "csv", "-o", path)
	if err == nil || !strings.Contains(err.Error(), "export stopped after 1 rows; run the same command with --resume") {
		t.Fatalf("err = %v", err)
	}
	if _, err := runCommand(t, AuditCmd(), "export", "--format", "csv", "-o", path); err == nil || !strings.Contains(err.Error(), "use --resume") {
		t.Errorf("a second plain run err = %v", err)
	}
	if _, err := runCommand(t, AuditCmd(), "export", "--format", "csv", "-o", path, "--resume", "--user", "other@example.com"); err == nil || !strings.Contains(err.Error(), "differ from the interrupted export") {
		t.Errorf("resume with other filters err = %v", err)
	}

	failSecond.Store(false)
	if _, err := runCommand(t, AuditCmd(), "export", "--format", "csv", "-o", path, "--resume"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "req_2") || strings.Count(string(got), "ts,event") != 1 {
		t.Fatalf("resumed file = %q", got)
	}
	if _, err := os.Stat(path + ".cursor"); err == nil {
		t.Error("the position file survived a finished export")
	}
}

func TestAuditExport_RejectsBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"format":             {"--format", "xml"},
		"resume to stdout":   {"--resume"},
		"bad date":           {"--from", "soon"},
		"nothing to resume":  {"--resume", "-o", filepath.Join(t.TempDir(), "none.jsonl")},
		"existing no resume": {"-o", existingFile(t)},
	} {
		if _, err := runCommand(t, AuditCmd(), append([]string{"export"}, args...)...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestAuditExport_ARefusalCarriesTheGatewaysSentence(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		auditBase + "limit=200": reply(403, `{"error":"forbidden","message":"audit export needs an admin"}`),
	})
	if _, err := runCommand(t, AuditCmd(), "export"); err == nil || !strings.Contains(err.Error(), "audit export needs an admin (HTTP 403)") {
		t.Fatalf("err = %v", err)
	}
}

func existingFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "there.jsonl")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuditExportQueryIsStable(t *testing.T) {
	f := auditFilter{from: "2026-10-01", user: "a@b.c"}
	q, err := f.query(url.Values{"limit": {"200"}})
	if err != nil || q.Encode() != "from=2026-10-01&limit=200&user=a%40b.c" {
		t.Errorf("query = %q, %v", q.Encode(), err)
	}
}

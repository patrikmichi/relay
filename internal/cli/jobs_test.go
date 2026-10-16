package cli

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastJobPolls(t *testing.T) {
	t.Helper()
	old := jobPollDelay
	jobPollDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { jobPollDelay = old })
}

func jobStates(states ...string) func(http.ResponseWriter, map[string]any) {
	var n atomic.Int32
	return func(w http.ResponseWriter, _ map[string]any) {
		i := min(int(n.Add(1))-1, len(states)-1)
		reply(200, `{"job":{"id":"job_01J9ZK3","kind":"agent_run","status":"`+states[i]+`","attempts":2,"error":"upstream 500"}}`)(w, nil)
	}
}

func TestRuns_ListPrintsATableAndTheNextCursor(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/jobs?kind=agent_run&status=dead": reply(200, contractFixture(t, "jobs.json")),
		"GET /api/jobs": reply(200, contractFixture(t, "jobs.json")),
	})
	out, err := runCommand(t, RunsCmd(), "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID", "job_01J9ZK3", "agent_run", "running", "job_01J9ZH8", "webhook_delivery", "dead", "zuano runs list --cursor cur_2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := runCommand(t, RunsCmd(), "list", "--kind", "agent_run", "--status", "dead", "--json"); err != nil {
		t.Fatal(err)
	}
}

func TestRuns_ShowAJobAndARun(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/jobs/job_01J9ZK3":    reply(200, contractFixture(t, "job.json")),
		"GET /api/runtime/runs/run_42": reply(200, `{"run":{"id":"run_42","status":"succeeded"},"job_status":"retrying"}`),
	})
	out, err := runCommand(t, RunsCmd(), "show", "job_01J9ZK3")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"job_01J9ZK3 (running)", "agent_run", "Attempts: 1", `"steps_done": 2`, "2026-10-16T12:00:02Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, RunsCmd(), "show", "run_42")
	if err != nil || !strings.Contains(out, "run_42 (retrying)") {
		t.Fatalf("a run reports its job status: %q, %v", out, err)
	}
}

func TestRuns_WaitExitStatuses(t *testing.T) {
	fastJobPolls(t)
	for _, tc := range []struct {
		name    string
		states  []string
		timeout string
		code    int
		want    string
	}{
		{"succeeded", []string{"queued", "running", "succeeded"}, "5", 0, "succeeded"},
		{"failed", []string{"running", "failed"}, "5", 1, "failed: upstream 500"},
		{"dead", []string{"retrying", "dead"}, "5", 1, "dead: upstream 500"},
		{"cancelled", []string{"cancelled"}, "5", 1, "cancelled"},
		{"still going", []string{"running"}, "0", exitPending, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
				"GET /api/jobs/job_01J9ZK3": jobStates(tc.states...),
			})
			out, err := runCommand(t, RunsCmd(), "wait", "job_01J9ZK3", "--wait-timeout", tc.timeout)
			if got := exitCodeOf(err); got != tc.code {
				t.Fatalf("exit = %d (%v), want %d", got, err, tc.code)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) && !strings.Contains(out, tc.want) {
				t.Errorf("neither error %v nor output %q has %q", err, out, tc.want)
			}
		})
	}
}

func TestRuns_CancelAndRetryUseTheJobRoutes(t *testing.T) {
	_, bodies := approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/jobs/job_01J9ZK3/cancel":    reply(200, `{"job":{"id":"job_01J9ZK3","status":"cancelled"}}`),
		"POST /api/jobs/job_01J9ZK3/retry":     reply(200, `{"job":{"id":"job_01J9ZK3","status":"queued"}}`),
		"POST /api/runtime/runs/run_42/cancel": reply(200, `{"ok":true,"job_status":"cancelled"}`),
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"cancel", "job_01J9ZK3"}, "Cancel requested: job_01J9ZK3 (cancelled)"},
		{[]string{"retry", "job_01J9ZK3"}, "Retry started: job_01J9ZK3 (queued)"},
		{[]string{"cancel", "run_42"}, "Cancel requested: run_42"},
	} {
		out, err := runCommand(t, RunsCmd(), tc.args...)
		if err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("%v: %q, %v", tc.args, out, err)
		}
	}
	if len(*bodies) != 3 {
		t.Errorf("posts = %d", len(*bodies))
	}
	if _, err := runCommand(t, RunsCmd(), "retry", "run_42"); err == nil || !strings.Contains(err.Error(), "job id") {
		t.Errorf("retry of a run id: %v", err)
	}
}

func TestRuns_ARefusedRetryReportsTheGatewaysSentence(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/jobs/job_01J9ZK3/retry": reply(403, `{"error":"forbidden","message":"only organisation admins can retry a job"}`),
	})
	if _, err := runCommand(t, RunsCmd(), "retry", "job_01J9ZK3"); err == nil || !strings.Contains(err.Error(), "only organisation admins can retry a job (HTTP 403)") {
		t.Fatalf("err = %v", err)
	}
}

func TestCall_AStartedJobIsReportedAndWaitFollowsItToTheEnd(t *testing.T) {
	fastJobPolls(t)
	startedBody := `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":` + contractFixture(t, "run-accepted.json") + `}}`
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/agents/mcp":      reply(200, startedBody),
		"GET /api/jobs/job_01J9ZK3": jobStates("queued", "running", "succeeded"),
	})
	out, err := runCommand(t, CallCmd(), "agents", "run_agent")
	if err != nil {
		t.Fatalf("a started job is not a failure: %v", err)
	}
	if !strings.Contains(out, "Started on the gateway: job_01J9ZK3 (queued)") || !strings.Contains(out, "zuano runs show job_01J9ZK3") {
		t.Errorf("output:\n%s", out)
	}

	out, err = runCommand(t, CallCmd(), "agents", "run_agent", "--wait")
	if err != nil || !strings.Contains(out, "job_01J9ZK3 (succeeded)") {
		t.Fatalf("--wait: %q, %v", out, err)
	}
}

func TestCall_ARESTAcceptedJobWithWaitReportsAFailedRun(t *testing.T) {
	fastJobPolls(t)
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/agents/mcp":      reply(202, contractFixture(t, "run-accepted.json")),
		"GET /api/jobs/job_01J9ZK3": jobStates("running", "dead"),
	})
	_, err := runCommand(t, CallCmd(), "agents", "run_agent", "--wait")
	if got := exitCodeOf(err); got != 1 || err == nil || !strings.Contains(err.Error(), "dead: upstream 500") {
		t.Fatalf("exit %d, err %v", got, err)
	}
}

func TestCall_AQuotaRefusalOnWaitPollingIsExit13(t *testing.T) {
	fastJobPolls(t)
	var polls atomic.Int32
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/agents/mcp": reply(202, contractFixture(t, "run-accepted.json")),
		"GET /api/jobs/job_01J9ZK3": func(w http.ResponseWriter, _ map[string]any) {
			polls.Add(1)
			reply(429, `{"error":"quota_exceeded","metric":"api_requests","limit":1000,"resets_at":"2026-10-17T00:00:00Z"}`)(w, nil)
		},
	})
	_, err := runCommand(t, CallCmd(), "agents", "run_agent", "--wait")
	if got := exitCodeOf(err); got != 13 || polls.Load() != 1 {
		t.Fatalf("exit %d (%v) after %d polls, want 13 after one", got, err, polls.Load())
	}
}

func TestCall_ARunThatIsNotStartedWorkIsPrintedAsUsual(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/agents/mcp": reply(200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"run_id\":\"run_1\",\"status\":\"succeeded\"}"}],"structuredContent":{"run_id":"run_1","status":"succeeded"}}}`),
	})
	out, err := runCommand(t, CallCmd(), "agents", "run_agent")
	if err != nil || strings.Contains(out, "Started on the gateway") {
		t.Fatalf("a finished run was reported as started: %q, %v", out, err)
	}
}

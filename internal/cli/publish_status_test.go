package cli

// White-box tests for the `relay publish status` / `relay publish --watch`
// polling logic added in publish.go. All tests use httptest.Server — no real
// gateway is ever contacted.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- test doers ----

// publishStatusTestDoer wraps an httptest.Server and satisfies publishStatusDoer
// (Get) only — used by the fetchPublishStatus / watchPublishStatus unit tests.
type publishStatusTestDoer struct {
	srv *httptest.Server
}

func (d *publishStatusTestDoer) Get(path string) (*http.Response, error) {
	return http.Get(d.srv.URL + path)
}

// publishAndStatusTestDoer wraps an httptest.Server and satisfies BOTH
// publishDoer (Post) and publishStatusDoer (Get) — used for the combined
// `runPublish` with `opts.watch = true` integration tests, where the same
// server handles both the initial publish POST and the subsequent status GETs.
type publishAndStatusTestDoer struct {
	srv *httptest.Server
}

func (d *publishAndStatusTestDoer) Post(path, contentType string, body io.Reader) (*http.Response, error) {
	return http.Post(d.srv.URL+path, contentType, body)
}

func (d *publishAndStatusTestDoer) Get(path string) (*http.Response, error) {
	return http.Get(d.srv.URL + path)
}

// ---- helpers ----

func intPtr(v int) *int { return &v }

func statusServer(t *testing.T, statusCode int, body interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		raw, _ := json.Marshal(body)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sequenceStatusServer returns a status server that serves each entry in
// `states` in order for successive requests, holding on the last entry once
// exhausted (so a poll loop naturally settles on the final state).
func sequenceStatusServer(t *testing.T, states []publishStatusResult) *httptest.Server {
	t.Helper()
	var idx int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&idx, 1) - 1)
		if i >= len(states) {
			i = len(states) - 1
		}
		pick := states[i]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		raw, _ := json.Marshal(pick)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withFastPolling overrides the polling knobs for the duration of a test and
// restores them afterward via t.Cleanup.
func withFastPolling(t *testing.T, maxPolls int) {
	t.Helper()
	prevInterval := publishStatusPollInterval
	prevMax := publishStatusMaxPolls
	prevSleep := sleepFn
	publishStatusPollInterval = time.Millisecond
	publishStatusMaxPolls = maxPolls
	sleepFn = func(time.Duration) {} // no-op — don't actually sleep in tests
	t.Cleanup(func() {
		publishStatusPollInterval = prevInterval
		publishStatusMaxPolls = prevMax
		sleepFn = prevSleep
	})
}

// ---- fetchPublishStatus ----

func TestFetchPublishStatus_HappyPath(t *testing.T) {
	want := publishStatusResult{
		VersionID: "ver_xyz789",
		State:     "scanning",
		Gates: []publishStatusGate{
			{Gate: "schema", Status: "passed"},
			{Gate: "permissions", Status: "passed", RequiresAdminApproval: true},
		},
		AiScan:                publishStatusAiScan{Status: "pending", TrustScore: nil, Findings: 0},
		RequiresAdminApproval: true,
		Approvable:            false,
	}
	srv := statusServer(t, http.StatusOK, want)

	doer := &publishStatusTestDoer{srv: srv}
	got, err := fetchPublishStatus(doer, "ver_xyz789")
	if err != nil {
		t.Fatalf("fetchPublishStatus: %v", err)
	}
	if got.State != want.State || got.VersionID != want.VersionID {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if len(got.Gates) != 2 || !got.Gates[1].RequiresAdminApproval {
		t.Errorf("gates not decoded correctly: %+v", got.Gates)
	}
}

func TestFetchPublishStatus_ErrorStatus(t *testing.T) {
	errBody := publishErrorBody{}
	errBody.Error.Code = "NOT_FOUND"
	errBody.Error.Message = "Version not found."
	srv := statusServer(t, http.StatusNotFound, errBody)

	doer := &publishStatusTestDoer{srv: srv}
	_, err := fetchPublishStatus(doer, "ver_missing")
	if err == nil {
		t.Fatal("expected an error for a 404 response, got nil")
	}
	if !strings.Contains(err.Error(), "Version not found.") {
		t.Errorf("error should surface the server message, got: %v", err)
	}
}

// ---- watchPublishStatus ----

func TestWatchPublishStatus_ReachesPublished(t *testing.T) {
	withFastPolling(t, 10)

	srv := sequenceStatusServer(t, []publishStatusResult{
		{VersionID: "ver_1", State: "scanning"},
		{VersionID: "ver_1", State: "approved"},
		{VersionID: "ver_1", State: "published"},
	})
	doer := &publishStatusTestDoer{srv: srv}

	err := watchPublishStatus(doer, "ver_1")
	if err != nil {
		t.Fatalf("watchPublishStatus: unexpected error: %v", err)
	}
}

func TestWatchPublishStatus_ChangesRequested(t *testing.T) {
	withFastPolling(t, 10)

	srv := sequenceStatusServer(t, []publishStatusResult{
		{VersionID: "ver_1", State: "changes_requested"},
	})
	doer := &publishStatusTestDoer{srv: srv}

	err := watchPublishStatus(doer, "ver_1")
	if err == nil {
		t.Fatal("expected a non-nil error when the version lands on changes_requested")
	}
	if !strings.Contains(err.Error(), "changes_requested") {
		t.Errorf("error should mention changes_requested, got: %v", err)
	}
}

func TestWatchPublishStatus_TimesOutOnNonTerminalState(t *testing.T) {
	withFastPolling(t, 3) // cap polls low so the test doesn't hang

	srv := sequenceStatusServer(t, []publishStatusResult{
		{VersionID: "ver_1", State: "scanning"},
	})
	doer := &publishStatusTestDoer{srv: srv}

	err := watchPublishStatus(doer, "ver_1")
	if err == nil {
		t.Fatal("expected a timeout error when the state never reaches terminal")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
}

func TestWatchPublishStatus_PropagatesFetchError(t *testing.T) {
	withFastPolling(t, 5)

	errBody := publishErrorBody{}
	errBody.Error.Message = "internal error"
	srv := statusServer(t, http.StatusInternalServerError, errBody)
	doer := &publishStatusTestDoer{srv: srv}

	err := watchPublishStatus(doer, "ver_1")
	if err == nil {
		t.Fatal("expected fetchPublishStatus's error to propagate")
	}
}

// ---- printPublishStatus (smoke test — no panic, no assertions on stdout) ----

func TestPrintPublishStatus_NoPanic(t *testing.T) {
	printPublishStatus(&publishStatusResult{
		VersionID: "ver_1",
		State:     "published",
		Gates: []publishStatusGate{
			{Gate: "ai_scan", Status: "passed"},
		},
		AiScan:     publishStatusAiScan{Status: "passed", TrustScore: intPtr(95), Findings: 0},
		Approvable: false,
	})
}

// ---- runPublish with --watch ----

// buildPublishAndStatusServer serves the publish response on POST
// /api/marketplace/publish and the given status sequence on GET
// /api/marketplace/publish/<versionId>/status.
func buildPublishAndStatusServer(t *testing.T, publishStatusCode int, pub publishResponse, statuses []publishStatusResult) *httptest.Server {
	t.Helper()
	var idx int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == publishAPIPath {
			w.WriteHeader(publishStatusCode)
			raw, _ := json.Marshal(pub)
			_, _ = w.Write(raw)
			return
		}
		if r.Method == http.MethodGet {
			i := int(atomic.AddInt32(&idx, 1) - 1)
			if i >= len(statuses) {
				i = len(statuses) - 1
			}
			pick := statuses[i]
			w.WriteHeader(http.StatusOK)
			raw, _ := json.Marshal(pick)
			_, _ = w.Write(raw)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunPublish_Watch_PollsUntilPublished(t *testing.T) {
	withFastPolling(t, 10)

	srv := buildPublishAndStatusServer(t, http.StatusCreated, successPublishResponse, []publishStatusResult{
		{VersionID: successPublishResponse.VersionID, State: "scanning"},
		{VersionID: successPublishResponse.VersionID, State: "published"},
	})
	doer := &publishAndStatusTestDoer{srv: srv}

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	err := runPublish(doer, skillPath, publishOpts{channel: "stable", watch: true})
	if err != nil {
		t.Fatalf("runPublish with --watch: unexpected error: %v", err)
	}
}

func TestRunPublish_Watch_ExitsNonZeroOnChangesRequested(t *testing.T) {
	withFastPolling(t, 10)

	srv := buildPublishAndStatusServer(t, http.StatusCreated, successPublishResponse, []publishStatusResult{
		{VersionID: successPublishResponse.VersionID, State: "changes_requested"},
	})
	doer := &publishAndStatusTestDoer{srv: srv}

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	err := runPublish(doer, skillPath, publishOpts{channel: "stable", watch: true})
	if err == nil {
		t.Fatal("expected a non-nil error when --watch lands on changes_requested")
	}
	if !strings.Contains(err.Error(), "changes_requested") {
		t.Errorf("error should mention changes_requested, got: %v", err)
	}
}

func TestRunPublish_Watch_RequiresStatusCapableDoer(t *testing.T) {
	// publishTestDoer (defined in publish_test.go) implements Post only — no Get.
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	doer := &publishTestDoer{srv: srv}

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	err := runPublish(doer, skillPath, publishOpts{channel: "stable", watch: true})
	if err == nil {
		t.Fatal("expected an error when --watch is used with a Post-only doer")
	}
	if !strings.Contains(err.Error(), "status polling") {
		t.Errorf("error should mention status polling capability, got: %v", err)
	}
}

func TestRunPublish_NoWatch_DoesNotPoll(t *testing.T) {
	// Without --watch, runPublish must not call the status endpoint at all —
	// a Post-only doer (publishTestDoer) must work exactly as before.
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	doer := &publishTestDoer{srv: srv}

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err != nil {
		t.Fatalf("runPublish without --watch: unexpected error: %v", err)
	}
}

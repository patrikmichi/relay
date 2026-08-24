package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// httpDoer is a minimal, auth-free Doer backed by a real httptest.Server —
// enough to exercise FetchSkill's HTTP-status mapping, header parsing, and
// verification logic without depending on internal/client (auth is a
// production concern; the CLI-level e2e tests exercise the full
// client.Resolve/client.New + GatewaySource path).
type httpDoer struct{ base string }

func (d *httpDoer) GetContext(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func newFakeGateway(t *testing.T, handler http.HandlerFunc) Doer {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &httpDoer{base: srv.URL}
}

func sha256HexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFetchSkill_NilDoerErrors(t *testing.T) {
	if _, err := FetchSkill(nil, "res_abc", "", ""); err == nil {
		t.Fatalf("expected an error for a nil Doer")
	}
}

func TestFetchSkill_EmptyIDErrors(t *testing.T) {
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("should not reach the server for an empty id")
	})
	if _, err := FetchSkill(doer, "  ", "", ""); err == nil {
		t.Fatalf("expected an error for an empty/blank catalog id")
	}
}

func TestFetchSkill_ChecksumMismatchAborts(t *testing.T) {
	bundle := simpleBundle(t)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, "0000000000000000000000000000000000000000000000000000000000000000")
		w.Header().Set(headerScanVerdict, "passed")
		w.Header().Set(headerVersion, "1.0.0")
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	_, err := FetchSkill(doer, "res_abc", "", "")
	if err == nil {
		t.Fatalf("expected a checksum-mismatch error")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected errors.Is(err, ErrChecksumMismatch), got: %v", err)
	}
}

func TestFetchSkill_MissingChecksumHeaderAborts(t *testing.T) {
	bundle := simpleBundle(t)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	_, err := FetchSkill(doer, "res_abc", "", "")
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected errors.Is(err, ErrChecksumMismatch) for a missing header, got: %v", err)
	}
}

func TestFetchSkill_ScanVerdictMissingAborts(t *testing.T) {
	bundle := simpleBundle(t)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	_, err := FetchSkill(doer, "res_abc", "", "")
	if !errors.Is(err, ErrScanVerdictFailed) {
		t.Fatalf("expected errors.Is(err, ErrScanVerdictFailed) for a missing verdict header, got: %v", err)
	}
}

func TestFetchSkill_ScanVerdictFailedAborts(t *testing.T) {
	bundle := simpleBundle(t)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "failed")
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	_, err := FetchSkill(doer, "res_abc", "", "")
	if !errors.Is(err, ErrScanVerdictFailed) {
		t.Fatalf("expected errors.Is(err, ErrScanVerdictFailed), got: %v", err)
	}
}

func TestFetchSkill_ScanVerdictBase64GateFailureAborts(t *testing.T) {
	bundle := simpleBundle(t)
	gates, _ := json.Marshal(map[string]string{
		"schema":  "pass",
		"secrets": "pass",
		"ai_scan": "fail",
	})
	verdict := base64.StdEncoding.EncodeToString(gates)

	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, verdict)
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	_, err := FetchSkill(doer, "res_abc", "", "")
	if !errors.Is(err, ErrScanVerdictFailed) {
		t.Fatalf("expected errors.Is(err, ErrScanVerdictFailed) for a failed gate, got: %v", err)
	}
}

func TestFetchSkill_ScanVerdictBase64AllPassSucceeds(t *testing.T) {
	bundle := simpleBundle(t)
	gates, _ := json.Marshal(map[string]string{
		"schema":       "pass",
		"secrets":      "pass",
		"sast":         "na",
		"static_scan":  "skip",
		"ai_scan":      "pass",
		"skillspector": "pass",
	})
	verdict := base64.StdEncoding.EncodeToString(gates)

	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, verdict)
		w.Header().Set(headerVersion, "2.0.0")
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	skill, err := FetchSkill(doer, "res_abc", "", "")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if skill.Name != "pr-triage" {
		t.Fatalf("expected name pr-triage, got %q", skill.Name)
	}
}

func TestFetchSkill_StatusCodeMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrNoAccess},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusConflict, ErrNotDownloadable},
		{http.StatusTooManyRequests, ErrRateLimited},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := FetchSkill(doer, "res_abc", "", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: expected errors.Is(err, %v), got: %v", tc.status, tc.want, err)
			}
		})
	}
}

func TestFetchSkill_UnrecognizedStatusErrors(t *testing.T) {
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	})
	_, err := FetchSkill(doer, "res_abc", "", "")
	if err == nil {
		t.Fatalf("expected an error for a 500 response")
	}
}

func TestFetchSkill_MaliciousTarballRejected(t *testing.T) {
	bad := buildTarGz(t, []tarEntry{
		{name: "../escape.txt", body: []byte("pwned")},
	})
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(bad))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		w.Write(bad)
	})

	_, err := FetchSkill(doer, "res_abc", "", "")
	if err == nil {
		t.Fatalf("expected an error for a path-escaping tarball")
	}
}

func TestFetchSkill_SuccessRoundTrip(t *testing.T) {
	bundle := simpleBundle(t)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/catalog/skills/res_abc123/download" {
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
		if v := r.URL.Query().Get("version"); v != "1.4.2" {
			t.Errorf("expected version=1.4.2 query param, got %q", v)
		}
		if c := r.URL.Query().Get("channel"); c != "beta" {
			t.Errorf("expected channel=beta query param, got %q", c)
		}
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "approved")
		w.Header().Set(headerVersion, "1.4.2")
		w.Header().Set(headerCatalogID, "res_abc123")
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	skill, err := FetchSkill(doer, "res_abc123", "1.4.2", "beta")
	if err != nil {
		t.Fatalf("FetchSkill: %v", err)
	}
	if skill.Name != "pr-triage" {
		t.Errorf("expected name pr-triage, got %q", skill.Name)
	}
	if skill.Description != "triage PRs" {
		t.Errorf("expected description %q, got %q", "triage PRs", skill.Description)
	}
	if got, want := string(skill.Provenance.SourceProvider), "gateway"; got != want {
		t.Errorf("expected Provenance.SourceProvider %q, got %q", want, got)
	}
	if skill.Provenance.CatalogID != "res_abc123" {
		t.Errorf("expected Provenance.CatalogID res_abc123, got %q", skill.Provenance.CatalogID)
	}
	if skill.Provenance.Version != "1.4.2" {
		t.Errorf("expected Provenance.Version 1.4.2, got %q", skill.Provenance.Version)
	}
	if _, ok := skill.Resources["scripts/run.sh"]; !ok {
		t.Errorf("expected scripts/run.sh to be loaded as a resource")
	}
}

func TestFetchSkill_VersionFallsBackToRequestedWhenHeaderAbsent(t *testing.T) {
	bundle := simpleBundle(t)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		w.Write(bundle)
	})

	skill, err := FetchSkill(doer, "res_abc123", "9.9.9", "")
	if err != nil {
		t.Fatalf("FetchSkill: %v", err)
	}
	if skill.Provenance.Version != "9.9.9" {
		t.Errorf("expected Provenance.Version to fall back to the requested version 9.9.9, got %q", skill.Provenance.Version)
	}
	if skill.Provenance.CatalogID != "res_abc123" {
		t.Errorf("expected Provenance.CatalogID to fall back to the requested id, got %q", skill.Provenance.CatalogID)
	}
}

// errReader always fails — exercises readLimited's error propagation path.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errBoom }

var errBoom = errors.New("boom: simulated read failure")

func TestReadLimited_PropagatesReaderError(t *testing.T) {
	if _, err := readLimited(errReader{}, 1024); !errors.Is(err, errBoom) {
		t.Fatalf("expected errors.Is(err, errBoom), got: %v", err)
	}
}

// TestFetchSkill_HangingServerBoundedByTimeout is the m3 regression: a
// gateway that never responds must not hang FetchSkill forever — it must
// abort cleanly once downloadTimeout elapses. downloadTimeout is shrunk for
// the duration of this test so it runs fast.
func TestFetchSkill_HangingServerBoundedByTimeout(t *testing.T) {
	orig := downloadTimeout
	downloadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { downloadTimeout = orig })

	// unblock must be closed BEFORE srv.Close() is called, or Close() (which
	// waits for in-flight handlers to return) would itself hang forever —
	// so this uses an explicit defer in known order rather than two
	// separate t.Cleanup registrations (whose LIFO order would run Close()
	// first).
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()
	doer := &httpDoer{base: srv.URL}

	start := time.Now()
	_, err := FetchSkill(doer, "res_abc", "", "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error for a hanging gateway response")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("expected FetchSkill to abort promptly on timeout, took %s", elapsed)
	}
}

func TestFetchSkill_SizeCapRejectsOversizedBody(t *testing.T) {
	oversized := make([]byte, MaxBundleBytes+1024)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(oversized))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		w.Write(oversized)
	})
	_, err := FetchSkill(doer, "res_abc", "", "")
	if err == nil {
		t.Fatalf("expected an error for a body exceeding MaxBundleBytes")
	}
}

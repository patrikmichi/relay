package catalog

import (
	"archive/tar"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// simpleAgentBundle builds a canonical agent-catalog bundle: a single
// root-level "<name>.md" Claude-shape frontmatter+body file, nothing else.
func simpleAgentBundle(t *testing.T, name string) []byte {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: reviews pull requests\nmodel: sonnet\ntools: Read, Grep\n---\n\nYou review PRs.\n"
	return buildTarGz(t, []tarEntry{
		{name: name + ".md", body: []byte(content)},
	})
}

func TestFetchAgent_NilDoerErrors(t *testing.T) {
	if _, err := FetchAgent(nil, "res_abc", "", ""); err == nil {
		t.Fatalf("expected an error for a nil Doer")
	}
}

func TestFetchAgent_EmptyIDErrors(t *testing.T) {
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		t.Fatalf("should not reach the server for an empty id")
	})
	if _, err := FetchAgent(doer, "  ", "", ""); err == nil {
		t.Fatalf("expected an error for an empty/blank catalog id")
	}
}

func TestFetchAgent_SuccessRoundTrip(t *testing.T) {
	bundle := simpleAgentBundle(t, "reviewer")
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		if r.URL.Path != "/api/catalog/resources/res_agent123/download" {
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
		if v := r.URL.Query().Get("version"); v != "1.0.0" {
			t.Errorf("expected version=1.0.0 query param, got %q", v)
		}
		if c := r.URL.Query().Get("channel"); c != "stable" {
			t.Errorf("expected channel=stable query param, got %q", c)
		}
		w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "approved")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})

	agent, err := FetchAgent(doer, "res_agent123", "1.0.0", "stable")
	if err != nil {
		t.Fatalf("FetchAgent: %v", err)
	}
	if agent.Name != "reviewer" {
		t.Errorf("expected name reviewer, got %q", agent.Name)
	}
	if agent.Description != "reviews pull requests" {
		t.Errorf("expected description %q, got %q", "reviews pull requests", agent.Description)
	}
	if got, want := string(agent.Provenance.SourceProvider), "claude"; got != want {
		t.Errorf("expected Provenance.SourceProvider %q, got %q", want, got)
	}
	if agent.Provenance.CatalogID != "res_agent123" {
		t.Errorf("expected Provenance.CatalogID res_agent123, got %q", agent.Provenance.CatalogID)
	}
	if agent.Provenance.Version != "1.0.0" {
		t.Errorf("expected Provenance.Version 1.0.0, got %q", agent.Provenance.Version)
	}
}

// TestFetchAgent_LegacyShaHeaderOnlyStillVerifies exercises the fallback to
// the legacy X-Skill-Content-Sha256 header when the type-agnostic
// X-Resource-Content-Sha256 header is absent.
func TestFetchAgent_LegacyShaHeaderOnlyStillVerifies(t *testing.T) {
	bundle := simpleAgentBundle(t, "reviewer")
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})

	agent, err := FetchAgent(doer, "res_agent123", "", "")
	if err != nil {
		t.Fatalf("expected success falling back to the legacy header, got: %v", err)
	}
	if agent.Name != "reviewer" {
		t.Errorf("expected name reviewer, got %q", agent.Name)
	}
}

func TestFetchAgent_ChecksumMismatchAborts(t *testing.T) {
	bundle := simpleAgentBundle(t, "reviewer")
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, "0000000000000000000000000000000000000000000000000000000000000000")
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})

	_, err := FetchAgent(doer, "res_agent123", "", "")
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected errors.Is(err, ErrChecksumMismatch), got: %v", err)
	}
}

func TestFetchAgent_ScanVerdictFailedAborts(t *testing.T) {
	bundle := simpleAgentBundle(t, "reviewer")
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "failed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})

	_, err := FetchAgent(doer, "res_agent123", "", "")
	if !errors.Is(err, ErrScanVerdictFailed) {
		t.Fatalf("expected errors.Is(err, ErrScanVerdictFailed), got: %v", err)
	}
}

func TestFetchAgent_UnprocessableEntityErrors(t *testing.T) {
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID_TYPE","message":"not downloadable via this endpoint"}}`))
	})
	_, err := FetchAgent(doer, "res_not_agent", "", "")
	if err == nil {
		t.Fatalf("expected an error for a 422 response")
	}
}

func TestFetchAgent_ForbiddenMapsToNoAccess(t *testing.T) {
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := FetchAgent(doer, "res_agent123", "", "")
	if !errors.Is(err, ErrNoAccess) {
		t.Fatalf("expected errors.Is(err, ErrNoAccess), got: %v", err)
	}
}

func TestFetchAgent_SizeCapRejectsOversizedBody(t *testing.T) {
	oversized := make([]byte, MaxBundleBytes+1024)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, sha256HexOf(oversized))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(oversized)
	})
	_, err := FetchAgent(doer, "res_agent123", "", "")
	if err == nil {
		t.Fatalf("expected an error for a body exceeding MaxBundleBytes")
	}
}

func TestFetchAgent_HangingServerBoundedByTimeout(t *testing.T) {
	orig := downloadTimeout
	downloadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { downloadTimeout = orig })

	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		<-unblock
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()
	doer := &httpDoer{base: srv.URL}

	start := time.Now()
	_, err := FetchAgent(doer, "res_agent123", "", "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error for a hanging gateway response")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("expected FetchAgent to abort promptly on timeout, took %s", elapsed)
	}
}

func TestFetchAgent_AmbiguousBundleMultipleMdFilesRejected(t *testing.T) {
	bundle := buildTarGz(t, []tarEntry{
		{name: "reviewer.md", body: []byte("---\nname: reviewer\ndescription: d\n---\n\nbody\n")},
		{name: "other.md", body: []byte("---\nname: other\ndescription: d\n---\n\nbody\n")},
	})
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})
	_, err := FetchAgent(doer, "res_agent123", "", "")
	if err == nil {
		t.Fatalf("expected an error for a bundle with multiple root-level .md files")
	}
}

func TestFetchAgent_EmptyBundleNoMdFileRejected(t *testing.T) {
	bundle := buildTarGz(t, []tarEntry{
		{name: "not-markdown.txt", body: []byte("hello")},
	})
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})
	_, err := FetchAgent(doer, "res_agent123", "", "")
	if err == nil {
		t.Fatalf("expected an error for a bundle with no root-level .md file")
	}
}

func TestFetchAgent_MaliciousTarballRejected(t *testing.T) {
	bad := buildTarGz(t, []tarEntry{
		{name: "../escape.md", body: []byte("pwned")},
	})
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, sha256HexOf(bad))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bad)
	})
	_, err := FetchAgent(doer, "res_agent123", "", "")
	if err == nil {
		t.Fatalf("expected an error for a path-escaping tarball")
	}
}

// TestFetchAgent_LoadErrorPropagates exercises the claudeAdapter.Load
// error path: a root-level .md file with no closing frontmatter delimiter
// fails to parse, and FetchAgent must propagate that error rather than
// returning a partially-loaded Agent.
func TestFetchAgent_LoadErrorPropagates(t *testing.T) {
	bundle := buildTarGz(t, []tarEntry{
		{name: "broken.md", body: []byte("---\nname: broken\n(missing closing delimiter)\n")},
	})
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})
	_, err := FetchAgent(doer, "res_agent123", "", "")
	if err == nil {
		t.Fatalf("expected an error for a malformed frontmatter block")
	}
}

// TestFetchAgent_BundleWithResourcesRejected covers
// resources that cannot be represented by the flat Agent IR must not be silently discarded.
func TestFetchAgent_BundleWithResourcesRejected(t *testing.T) {
	bundle := buildTarGz(t, []tarEntry{
		{name: "reviewer.md", body: []byte("---\nname: reviewer\ndescription: reviews PRs\n---\n\nbody\n")},
		{name: "notes/", typeflag: tar.TypeDir},
		{name: "notes/extra.txt", body: []byte("not an agent file")},
	})
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Type", "agent")
		w.Header().Set(headerCatalogID, "res_agent123")
		w.Header().Set(headerVersion, "1.0.0")
		w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "passed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	})
	agent, err := FetchAgent(doer, "res_agent123", "", "")
	if err == nil {
		t.Fatalf("FetchAgent: %v", err)
	}
	if agent != nil {
		t.Fatal("unsupported bundle returned a partial agent")
	}
}

func TestFetchAgent_IdentityContractMismatchRejected(t *testing.T) {
	for _, field := range []string{"X-Resource-Type", headerCatalogID, headerVersion} {
		t.Run(field, func(t *testing.T) {
			bundle := simpleAgentBundle(t, "reviewer")
			doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
				w.Header().Set(headerScanVerdict, "approved")
				w.Header().Set("X-Resource-Type", "agent")
				w.Header().Set(headerCatalogID, "res_agent123")
				w.Header().Set(headerVersion, "1.0.0")
				w.Header().Set(field, "wrong")
				_, _ = w.Write(bundle)
			})
			agent, err := FetchAgent(doer, "res_agent123", "1.0.0", "")
			if err == nil || agent != nil {
				t.Fatal("mismatched download returned an installable agent")
			}
		})
	}
}

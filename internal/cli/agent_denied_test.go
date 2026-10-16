package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/keychain"
)

func TestAgentSessionDenied(t *testing.T) {
	body := []byte(`{"error":"Forbidden: Agent credentials cannot change this; use the MCP tools, which ask a person to approve"}`)

	err := agentSessionDenied(keychain.ProfileAgent, http.StatusForbidden, body)
	if err == nil {
		t.Fatal("agent 403 produced no error")
	}
	for _, want := range []string{"Agent credentials cannot change this", "unset RELAY_AGENT", "MCP tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Forbidden: ") {
		t.Errorf("error keeps the transport prefix: %q", err)
	}

	for name, got := range map[string]error{
		"human 403":  agentSessionDenied(keychain.ProfileHuman, http.StatusForbidden, body),
		"agent 404":  agentSessionDenied(keychain.ProfileAgent, http.StatusNotFound, body),
		"agent 200":  agentSessionDenied(keychain.ProfileAgent, http.StatusOK, nil),
		"agent 401":  agentSessionDenied(keychain.ProfileAgent, http.StatusUnauthorized, body),
		"agent 5xx":  agentSessionDenied(keychain.ProfileAgent, http.StatusBadGateway, body),
		"empty body": nil,
	} {
		if got != nil {
			t.Errorf("%s: unexpected error %v", name, got)
		}
	}
}

func TestAgentSessionDenied_AcceptsEveryErrorEnvelope(t *testing.T) {
	for name, body := range map[string]string{
		"reason":        `{"reason":"read-only here"}`,
		"nested":        `{"error":{"message":"read-only here"}}`,
		"flat message":  `{"message":"read-only here"}`,
		"string prefix": `{"error":"Forbidden: read-only here"}`,
	} {
		err := agentSessionDenied(keychain.ProfileAgent, http.StatusForbidden, []byte(body))
		if err == nil || !strings.Contains(err.Error(), "read-only here") {
			t.Errorf("%s: %v", name, err)
		}
	}
	err := agentSessionDenied(keychain.ProfileAgent, http.StatusForbidden, []byte("<html>"))
	if err == nil || !strings.Contains(err.Error(), "agent sessions cannot change this") {
		t.Errorf("unparseable body: %v", err)
	}
}

func TestPublishForbiddenForAgentSessionSurfacesReason(t *testing.T) {
	t.Setenv("RELAY_AGENT", "1")
	err := handlePublishResponse(http.StatusForbidden, http.Header{}, []byte(`{"error":"Forbidden: Agent credentials cannot change this"}`), "")
	if err == nil || !strings.Contains(err.Error(), "Agent credentials cannot change this") {
		t.Fatalf("got %v", err)
	}

	t.Setenv("RELAY_AGENT", "")
	err = handlePublishResponse(http.StatusForbidden, http.Header{}, []byte(`{"error":"x"}`), "")
	if err == nil || !strings.Contains(err.Error(), "not authorized to publish") {
		t.Fatalf("personal 403: %v", err)
	}
}

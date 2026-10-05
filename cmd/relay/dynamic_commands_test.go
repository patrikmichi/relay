package main

// Dynamic service/tool commands were unreachable because Cobra resolves
// the command tree before PersistentPreRunE ever runs, so a hook-based call
// to BuildServiceCommands never fired for a not-yet-registered dynamic
// command. These tests exercise prepareDynamicCommands, the pre-Execute
// discovery step that replaces it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDiscoveryCandidate documents which argv shapes prepareDynamicCommands
// treats as "a command word Cobra will try to resolve" versus "no candidate,
// skip discovery" (bare root, help/offline-only invocations).
func TestDiscoveryCandidate(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
		ok   bool
	}{
		{"empty", nil, "", false},
		{"bare help flag", []string{"--help"}, "", false},
		{"bare offline flag", []string{"--offline"}, "", false},
		{"dynamic candidate", []string{"clockify", "list_time_entries"}, "clockify", true},
		{"static candidate", []string{"login"}, "login", true},
		{"flags before candidate", []string{"--offline", "clockify"}, "clockify", true},
		{"double dash forces next token positional", []string{"--", "--weird-service-name"}, "--weird-service-name", true},
		{"double dash with nothing after", []string{"--"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := discoveryCandidate(c.args)
			if got != c.want || ok != c.ok {
				t.Errorf("discoveryCandidate(%v) = (%q, %v), want (%q, %v)", c.args, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestPrepareDynamicCommandsSkipsDiscoveryWhenOffline confirms --offline
// blocks discovery at the pre-Execute step (not just inside a pre-run hook
// that would never fire for this exact case) — zero network calls, and the
// candidate service is never registered.
func TestPrepareDynamicCommandsSkipsDiscoveryWhenOffline(t *testing.T) {
	dialed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("GATEWAY_API_KEY", "synthetic-test-key")

	root := newRootCmd()
	if err := prepareDynamicCommands(root, []string{"--offline", "clockify", "list_time_entries"}); err != nil {
		t.Fatalf("prepareDynamicCommands: %v", err)
	}
	if dialed {
		t.Fatal("--offline must never dial the gateway during discovery")
	}
	for _, c := range root.Commands() {
		if c.Name() == "clockify" {
			t.Fatal("an undiscovered service must not be registered under --offline")
		}
	}
}

// TestPrepareDynamicCommandsSkipsDiscoveryForBareHelp confirms
// `relay --help` (or a bare `relay`) never dials the gateway — static help
// output must stay completely free of discovery.
func TestPrepareDynamicCommandsSkipsDiscoveryForBareHelp(t *testing.T) {
	dialed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("GATEWAY_API_KEY", "synthetic-test-key")

	for _, args := range [][]string{{"--help"}, nil, {"-h"}} {
		root := newRootCmd()
		if err := prepareDynamicCommands(root, args); err != nil {
			t.Fatalf("prepareDynamicCommands(%v): %v", args, err)
		}
	}
	if dialed {
		t.Fatal("relay --help (or a bare relay) must never dial the gateway")
	}
}

// TestFullRootDiscoveredServiceIsExecutable: a fake discovered service
// becomes a genuinely executable command through the full root command tree,
// not just through BuildServiceCommands in isolation.
func TestFullRootDiscoveredServiceIsExecutable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/integrations":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": true, "serviceCount": 1, "toolCount": 1,
				"services": []map[string]interface{}{
					{
						"id": "fakesvc", "name": "Fake Service", "accessible": true, "toolCount": 1,
						"tools": []map[string]interface{}{{"name": "ping", "description": "ping"}},
					},
				},
			})
		case "/api/fakesvc/mcp":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "pong"}}},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("GATEWAY_API_KEY", "synthetic-test-key")

	root := newRootCmd()
	if err := prepareDynamicCommands(root, []string{"fakesvc", "ping"}); err != nil {
		t.Fatalf("prepareDynamicCommands: %v", err)
	}

	root.SetArgs([]string{"fakesvc", "ping"})
	if err := root.Execute(); err != nil {
		t.Fatalf("expected the discovered dynamic command to resolve and execute, got: %v", err)
	}
}

// TestStaticCommandNameCannotBeReplacedByDiscoveredService is the third
// named acceptance item: a discovered catalog service must never shadow (or
// be shadowed by) a built-in command — it must fail loudly instead.
func TestStaticCommandNameCannotBeReplacedByDiscoveredService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true, "serviceCount": 1, "toolCount": 1,
			"services": []map[string]interface{}{
				{
					"id": "login", "name": "Colliding Service", "accessible": true, "toolCount": 1,
					"tools": []map[string]interface{}{{"name": "x"}},
				},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("GATEWAY_API_KEY", "synthetic-test-key")

	root := newRootCmd()
	err := prepareDynamicCommands(root, []string{"nonstatic-candidate-triggers-discovery"})
	if err == nil {
		t.Fatal("expected an error when a discovered service collides with the static \"login\" command")
	}

	for _, c := range root.Commands() {
		if c.Name() == "login" && len(c.Commands()) != 0 {
			t.Fatalf("static login command must not gain sub-commands from a colliding discovered service: %v", c.Commands())
		}
	}
}

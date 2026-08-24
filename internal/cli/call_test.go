package cli_test

// NOTE: the REST /api/<service>/call wire-contract tests that used to live
// here (TestCallEndpoint_BearerAndBody, TestCallEndpoint_401_Response) were
// removed — CLI execution was repointed from REST /call to MCP JSON-RPC
// tools/call (POST /api/<service>/mcp), and those tests only ever built a
// raw http.Request by hand rather than calling callTool/CallCmd, so they
// exercised zero real CLI code and misleadingly implied the CLI still sends
// the REST {tool, arguments} shape. The real execution path (callTool
// against /api/<service>/mcp, including JSON-RPC error mapping and the
// account/athlete label passthrough) is covered by the white-box
// TestCallTool_* suite in service_cmd_test.go.

import (
	"encoding/json"
	"testing"

	"github.com/zalando/go-keyring"
)

// init installs the mock keyring so CLI tests never touch the real OS keychain.
func init() {
	keyring.MockInit()
}

// parseArgValue mirrors the key=value → typed-value logic in call.go / service_cmd.go.
// We duplicate the inline logic here because it is embedded in cobra RunE closures
// and not exported. The tests validate the parsing contract independently.
func parseArgValue(v string) interface{} {
	var parsed interface{}
	if err := json.Unmarshal([]byte(v), &parsed); err == nil {
		return parsed
	}
	return v
}

// TestParseArgValue_StringPassthrough ensures plain strings survive unchanged.
func TestParseArgValue_StringPassthrough(t *testing.T) {
	cases := []struct {
		input   string
		wantStr string
	}{
		{"hello", "hello"},
		{"is:unread", "is:unread"},
		{"multi word value", "multi word value"},
		{"2026-06-08", "2026-06-08"},
	}
	for _, tc := range cases {
		got := parseArgValue(tc.input)
		s, ok := got.(string)
		if !ok || s != tc.wantStr {
			t.Errorf("parseArgValue(%q): got %#v, want string %q", tc.input, got, tc.wantStr)
		}
	}
}

// TestParseArgValue_Numbers ensures numeric strings are decoded as numbers.
func TestParseArgValue_Numbers(t *testing.T) {
	cases := []struct {
		input string
		want  float64
	}{
		{"42", 42},
		{"3.14", 3.14},
		{"0", 0},
	}
	for _, tc := range cases {
		got := parseArgValue(tc.input)
		f, ok := got.(float64)
		if !ok {
			t.Errorf("parseArgValue(%q): got %T %v, want float64", tc.input, got, got)
			continue
		}
		if f != tc.want {
			t.Errorf("parseArgValue(%q): got %v, want %v", tc.input, f, tc.want)
		}
	}
}

// TestParseArgValue_Boolean ensures boolean strings are decoded as booleans.
func TestParseArgValue_Boolean(t *testing.T) {
	if got := parseArgValue("true"); got != true {
		t.Errorf("parseArgValue(true): got %v, want true", got)
	}
	if got := parseArgValue("false"); got != false {
		t.Errorf("parseArgValue(false): got %v, want false", got)
	}
}

// TestParseArgValue_JSON ensures JSON objects and arrays decode correctly.
func TestParseArgValue_JSON(t *testing.T) {
	got := parseArgValue(`{"key":"val"}`)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("parseArgValue(json object): got %T, want map", got)
	}
	if m["key"] != "val" {
		t.Errorf("map[key]: got %v, want val", m["key"])
	}

	arr := parseArgValue(`["a","b"]`)
	sl, ok := arr.([]interface{})
	if !ok {
		t.Fatalf("parseArgValue(json array): got %T, want []interface{}", arr)
	}
	if len(sl) != 2 {
		t.Errorf("slice length: got %d, want 2", len(sl))
	}
}

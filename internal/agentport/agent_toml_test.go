package agentport

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentTOML_CodexRoundTrip exercises the full Load -> Project ->
// re-Load round trip for the format: toml codex agent codec, covering
// every declared field (name, description, body/developer_instructions,
// model) plus the dropped-field fidelity report.
func TestAgentTOML_CodexRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "agents")
	writeFiles(t, dir, map[string][]byte{
		"reviewer.toml": []byte("name = \"reviewer\"\n" +
			"description = \"Reviews code for quality.\"\n" +
			"developer_instructions = \"\"\"\n" +
			"Review the diff for correctness and style.\n" +
			"Flag anything risky.\n" +
			"\"\"\"\n" +
			"model = \"gpt-5-codex\"\n"),
	})

	codex, ok := AgentAdapterByID(ProviderCodex)
	if !ok {
		t.Fatalf("AgentAdapterByID(codex): ok = false")
	}
	if codex.FileExt() != ".toml" {
		t.Fatalf("FileExt() = %q, want .toml", codex.FileExt())
	}

	ag, err := codex.Load(filepath.Join(dir, "reviewer.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ag.Name != "reviewer" || ag.Description != "Reviews code for quality." {
		t.Fatalf("unexpected agent: %#v", ag)
	}
	if !strings.Contains(ag.Body, "Review the diff for correctness and style.") {
		t.Fatalf("Body = %q, missing developer_instructions content", ag.Body)
	}
	if ag.Model != "gpt-5-codex" {
		t.Fatalf("Model = %q, want gpt-5-codex", ag.Model)
	}

	files, loss, err := codex.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	content, ok := files["reviewer.toml"]
	if !ok {
		t.Fatalf("Project files = %#v, want a reviewer.toml entry", files)
	}
	if !strings.Contains(string(content), `name = "reviewer"`) {
		t.Errorf("projected content missing name field: %s", content)
	}
	if !strings.Contains(string(content), "developer_instructions") {
		t.Errorf("projected content missing developer_instructions: %s", content)
	}
	// Same-provider round trip: Model needs no mapping (source==target).
	for _, l := range loss {
		if l.Field == "Model" {
			t.Errorf("unexpected Model loss on a same-provider round trip: %#v", l)
		}
	}

	// Re-Load the projected bytes from a scratch dir and confirm the
	// second Project() call is byte-identical (idempotent serialization).
	scratch := t.TempDir()
	writeFiles(t, scratch, files)
	ag2, err := codex.Load(filepath.Join(scratch, "reviewer.toml"))
	if err != nil {
		t.Fatalf("Load(2): %v", err)
	}
	if ag2.Body != ag.Body {
		t.Fatalf("Body did not round-trip: got %q, want %q", ag2.Body, ag.Body)
	}
	files2, _, err := codex.Project(ag2)
	if err != nil {
		t.Fatalf("Project(2): %v", err)
	}
	if string(files2["reviewer.toml"]) != string(content) {
		t.Fatalf("Project() not idempotent:\n--- 1 ---\n%s\n--- 2 ---\n%s", content, files2["reviewer.toml"])
	}
}

// TestAgentTOML_CodexNameInferredFromFilename confirms Name falls back to
// the filename (".toml" trimmed) when the TOML omits a "name" key.
func TestAgentTOML_CodexNameInferredFromFilename(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"my-agent.toml": []byte("description = \"d\"\ndeveloper_instructions = \"body\"\n"),
	})
	codex, _ := AgentAdapterByID(ProviderCodex)
	ag, err := codex.Load(filepath.Join(dir, "my-agent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ag.Name != "my-agent" {
		t.Fatalf("Name = %q, want inferred from filename", ag.Name)
	}
}

// TestAgentTOML_ClaudeToCodexDropsUnsupportedFields confirms migrating a
// claude agent (tools/memory/skills populated) onto codex reports each as
// dropped — codex's TOML schema has no equivalent for any of them.
func TestAgentTOML_ClaudeToCodexDropsUnsupportedFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFiles(t, filepath.Join(home, ".claude", "agents"), map[string][]byte{
		"reviewer.md": []byte("---\nname: reviewer\ndescription: d\ntools: Read, Bash\nmemory: project\nskills:\n  - api-design\nmodel: sonnet\n---\n\nReview the diff.\n"),
	})

	claude := NewClaudeAgentAdapter()
	ag, err := claude.Load(filepath.Join(home, ".claude", "agents", "reviewer.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	codex, _ := AgentAdapterByID(ProviderCodex)
	files, loss, err := codex.Project(ag)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if _, ok := files["reviewer.toml"]; !ok {
		t.Fatalf("Project files = %#v, want a reviewer.toml entry", files)
	}

	gotFields := map[string]LossKind{}
	for _, l := range loss {
		gotFields[l.Field] = l.Kind
	}
	for _, f := range []string{"Tools", "Memory", "Skills"} {
		if gotFields[f] != LossDropped {
			t.Errorf("%s loss = %v, want %v (got loss=%#v)", f, gotFields[f], LossDropped, loss)
		}
	}
	if gotFields["Model"] != LossDegraded {
		t.Errorf("Model loss = %v, want %v (no claude<->codex alias table)", gotFields["Model"], LossDegraded)
	}
}

// TestParseFlatTOML_FlatSubset exercises every value shape
// parseFlatTOML supports directly: basic string, multi-line basic string,
// array of strings, integer, float, bool, and full-line comments.
func TestParseFlatTOML_FlatSubset(t *testing.T) {
	raw := []byte(`# a leading comment
name = "demo"
count = 3
ratio = 1.5
enabled = true
disabled = false
tags = ["a", "b", "c"]
empty_tags = []
body = """
line one
line two
"""
`)
	values, warnings, err := parseFlatTOML(raw)
	if err != nil {
		t.Fatalf("parseFlatTOML: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if values["name"] != "demo" {
		t.Errorf("name = %#v, want demo", values["name"])
	}
	if values["count"] != int64(3) {
		t.Errorf("count = %#v, want int64(3)", values["count"])
	}
	if values["ratio"] != 1.5 {
		t.Errorf("ratio = %#v, want 1.5", values["ratio"])
	}
	if values["enabled"] != true {
		t.Errorf("enabled = %#v, want true", values["enabled"])
	}
	if values["disabled"] != false {
		t.Errorf("disabled = %#v, want false", values["disabled"])
	}
	tags, ok := values["tags"].([]string)
	if !ok || len(tags) != 3 || tags[0] != "a" || tags[2] != "c" {
		t.Errorf("tags = %#v, want [a b c]", values["tags"])
	}
	if v, ok := values["empty_tags"].([]string); ok && len(v) != 0 {
		t.Errorf("empty_tags = %#v, want an empty/nil slice", v)
	}
	if values["body"] != "line one\nline two\n" {
		t.Errorf("body = %q, want %q", values["body"], "line one\nline two\n")
	}
}

// TestParseFlatTOML_SkipsTableHeaderWithWarning confirms a table header
// (e.g. Codex's [mcp_servers]) stops flat key parsing with a recorded
// warning instead of failing the whole file.
func TestParseFlatTOML_SkipsTableHeaderWithWarning(t *testing.T) {
	raw := []byte("name = \"demo\"\n[mcp_servers]\nfoo = \"bar\"\n")
	values, warnings, err := parseFlatTOML(raw)
	if err != nil {
		t.Fatalf("parseFlatTOML: %v", err)
	}
	if values["name"] != "demo" {
		t.Errorf("name = %#v, want demo (parsed before the table header)", values["name"])
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "mcp_servers") {
		t.Errorf("warnings = %v, want one mentioning mcp_servers", warnings)
	}
}

// TestParseFlatTOML_SkipsDottedKeyWithWarning confirms a dotted key (e.g.
// Codex's skills.config) is skipped with a recorded warning, without
// affecting any other top-level key's parsing.
func TestParseFlatTOML_SkipsDottedKeyWithWarning(t *testing.T) {
	raw := []byte("name = \"demo\"\nskills.config = \"enabled\"\nmodel = \"sonnet\"\n")
	values, warnings, err := parseFlatTOML(raw)
	if err != nil {
		t.Fatalf("parseFlatTOML: %v", err)
	}
	if values["name"] != "demo" || values["model"] != "sonnet" {
		t.Errorf("values = %#v, want name/model preserved around the skipped dotted key", values)
	}
	if _, ok := values["skills.config"]; ok {
		t.Errorf("skills.config should not have been captured")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "skills.config") {
		t.Errorf("warnings = %v, want one mentioning skills.config", warnings)
	}
}

// TestParseFlatTOML_ErrorsOnUnsupportedValue confirms a value this parser
// doesn't understand (not a string/bool/number/array) surfaces as an
// error rather than being silently dropped.
func TestParseFlatTOML_ErrorsOnUnsupportedValue(t *testing.T) {
	if _, _, err := parseFlatTOML([]byte("name = not_a_valid_value\n")); err == nil {
		t.Fatalf("expected an error for an unrecognized value")
	}
	if _, _, err := parseFlatTOML([]byte("no equals sign here\n")); err == nil {
		t.Fatalf("expected an error for a line with no '='")
	}
	if _, _, err := parseFlatTOML([]byte(`bad = "unterminated`)); err == nil {
		t.Fatalf("expected an error for a malformed basic string")
	}
	if _, _, err := parseFlatTOML([]byte("body = \"\"\"\nunterminated\n")); err == nil {
		t.Fatalf("expected an error for an unterminated multi-line string")
	}
	if _, _, err := parseFlatTOML([]byte("arr = [1, 2]\n")); err == nil {
		t.Fatalf("expected an error for an array of non-strings")
	}
}

// TestEncodeTOMLValue_Types exercises encodeTOMLValue's supported Go value
// types directly.
func TestEncodeTOMLValue_Types(t *testing.T) {
	cases := []struct {
		ir   string
		val  interface{}
		want string
	}{
		{"name", "demo", `"demo"`},
		{"body", "line one\nline two", "\"\"\"\nline one\nline two\"\"\""},
		{"enabled", true, "true"},
		{"enabled", false, "false"},
		{"count", 3, "3"},
		{"count", int64(3), "3"},
		{"ratio", 1.5, "1.5"},
		{"tags", []string{"a", "b"}, `["a", "b"]`},
	}
	for _, c := range cases {
		got, err := encodeTOMLValue(c.ir, c.val)
		if err != nil {
			t.Fatalf("encodeTOMLValue(%q, %#v): %v", c.ir, c.val, err)
		}
		if got != c.want {
			t.Errorf("encodeTOMLValue(%q, %#v) = %q, want %q", c.ir, c.val, got, c.want)
		}
	}
	if _, err := encodeTOMLValue("x", struct{}{}); err == nil {
		t.Errorf("expected an error for an unsupported value type")
	}
}

// TestEncodeTOMLMultilineString_EscapesTripleQuotesAndTrailingQuote
// confirms the defensive escaping that keeps a multi-line string from ever
// being ambiguous with its own closing delimiter.
func TestEncodeTOMLMultilineString_EscapesTripleQuotesAndTrailingQuote(t *testing.T) {
	got := encodeTOMLMultilineString(`contains """ inside and ends with "`)
	if strings.Count(got, `"""`) != 2 {
		// exactly the opening and closing delimiters — any embedded run of
		// 3+ quotes must have been broken up.
		t.Errorf("encoded value has an ambiguous \"\"\" run: %q", got)
	}
	beforeClosingDelim := got[:len(got)-3]
	if !strings.HasSuffix(beforeClosingDelim, `\"`) {
		t.Errorf("trailing quote before the closing delimiter was not escaped: %q", got)
	}
}

// tomlMultilineAdversarialCases is the shared seed corpus for
// TestTOMLMultilineRoundTrip_Adversarial and FuzzTOMLMultilineRoundTrip:
// bodies that would have silently truncated under the old naive
// strings.Index-based decoder (quote-run cases), plus the encoder's other
// escape paths (backslashes, CRLF, empty/whitespace-only content).
var tomlMultilineAdversarialCases = []struct {
	name string
	body string
}{
	{"quote_run_5_mid_string", `abc""""" def`}, // the exact case from the code review
	{"quote_run_4_only", `""""`},
	{"quote_run_3_only", `"""`},
	{"quote_runs_separated", `a"""b"""c`},
	{"ends_in_one_quote", `abc"`},
	{"ends_in_two_quotes", `abc""`},
	{"ends_in_three_quotes", `abc"""`},
	{"windows_path_backslash", `C:\path`},
	{"trailing_backslash", "trailing \\"},
	{"literal_backslash_u_escape_text", `contains \u0041 literal`},
	{"mixed_crlf", "line1\r\nline2\nline3\r\n"},
	{"empty_body", ""},
	{"whitespace_only", "   \n\t\n  "},
}

// pythonTomllibAvailable reports whether python3 with the stdlib tomllib
// module (3.11+) is importable on PATH. Cross-checking against a real TOML
// parser is a valuable second opinion, but this codec's test suite must not
// depend on python3 being installed.
func pythonTomllibAvailable() bool {
	if _, err := exec.LookPath("python3"); err != nil {
		return false
	}
	return exec.Command("python3", "-c", "import tomllib").Run() == nil
}

// parseWithPythonTomllib shells out to python3's tomllib to parse src and
// returns the JSON-encoded value of top-level key "x" (json.Marshal gives an
// unambiguous representation to diff against Go's own json.Marshal of the
// expected value, sidestepping quoting mismatches between Python repr and Go
// string formatting).
func parseWithPythonTomllib(src string) (string, error) {
	const script = `
import sys, tomllib, json
data = tomllib.loads(sys.stdin.read())
print(json.dumps(data["x"]))
`
	cmd := exec.Command("python3", "-c", script)
	cmd.Stdin = strings.NewReader(src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// TestTOMLMultilineRoundTrip_Adversarial round-trips every case in
// tomlMultilineAdversarialCases through encodeTOMLMultilineString ->
// parseFlatTOML and asserts the decoded value is byte-identical to the
// original. When python3+tomllib is on PATH, it additionally asserts the
// emitted TOML parses via a real independent TOML implementation to the
// same value — otherwise that sub-assertion is skipped (logged, not
// failed): the suite must not depend on python3 being installed.
func TestTOMLMultilineRoundTrip_Adversarial(t *testing.T) {
	havePython := pythonTomllibAvailable()
	if !havePython {
		t.Log("python3 tomllib not available on PATH — skipping cross-check sub-assertion for all cases")
	}

	for _, tc := range tomlMultilineAdversarialCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := encodeTOMLMultilineString(tc.body)
			src := "x = " + encoded + "\n"

			values, warnings, err := parseFlatTOML([]byte(src))
			if err != nil {
				t.Fatalf("parseFlatTOML: %v\nencoded: %q\nsrc:\n%s", err, encoded, src)
			}
			if len(warnings) != 0 {
				t.Fatalf("unexpected warnings: %v", warnings)
			}
			got, ok := values["x"].(string)
			if !ok {
				t.Fatalf("values[x] = %#v, want a string", values["x"])
			}
			if got != tc.body {
				t.Fatalf("round trip mismatch:\n got:     %q\n want:    %q\n encoded: %q", got, tc.body, encoded)
			}

			if !havePython {
				return
			}
			wantJSON, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("json.Marshal(body): %v", err)
			}
			pyGot, err := parseWithPythonTomllib(src)
			if err != nil {
				t.Fatalf("python3 tomllib failed to parse our own output: %v\nsrc:\n%s", err, src)
			}
			if pyGot != string(wantJSON) {
				t.Fatalf("python3 tomllib parsed a different value:\n got:  %s\n want: %s\nsrc:\n%s", pyGot, wantJSON, src)
			}
		})
	}
}

// FuzzTOMLMultilineRoundTrip fuzzes encodeTOMLMultilineString ->
// parseFlatTOML over arbitrary bodies (not just the hand-picked adversarial
// cases above) — any body must round-trip byte-identical, with no error and
// no warnings, regardless of what quote/backslash/newline shapes it
// contains.
func FuzzTOMLMultilineRoundTrip(f *testing.F) {
	for _, tc := range tomlMultilineAdversarialCases {
		f.Add(tc.body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		encoded := encodeTOMLMultilineString(body)
		src := "x = " + encoded + "\n"

		values, warnings, err := parseFlatTOML([]byte(src))
		if err != nil {
			t.Fatalf("parseFlatTOML: %v\nbody: %q\nencoded: %q", err, body, encoded)
		}
		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %v\nbody: %q", warnings, body)
		}
		got, ok := values["x"].(string)
		if !ok {
			t.Fatalf("values[x] = %#v, want a string\nbody: %q", values["x"], body)
		}
		if got != body {
			t.Fatalf("round trip mismatch:\n got:     %q\n want:    %q\n encoded: %q", got, body, encoded)
		}
	})
}

// TestDecodeAgentTOMLField_TypeMismatchErrors confirms a TOML value of the
// wrong Go type for a string-typed IR field surfaces as an error.
func TestDecodeAgentTOMLField_TypeMismatchErrors(t *testing.T) {
	ag := &Agent{}
	for _, ir := range []string{"name", "description", "model", "body"} {
		if err := decodeAgentTOMLField(ag, ir, 42); err == nil {
			t.Errorf("decodeAgentTOMLField(%s, 42): expected an error", ir)
		}
	}
	if err := decodeAgentTOMLField(ag, "not_a_real_ir", "x"); err == nil {
		t.Errorf("decodeAgentTOMLField(unknown ir): expected an error")
	}
}

// TestAgentTOML_LoadSurfacesReadError confirms a missing file errors
// through loadTOML rather than panicking.
func TestAgentTOML_LoadSurfacesReadError(t *testing.T) {
	codex, _ := AgentAdapterByID(ProviderCodex)
	if _, err := codex.Load(filepath.Join(t.TempDir(), "does-not-exist.toml")); err == nil {
		t.Fatalf("expected a read error")
	}
}

// TestAgentTOML_LoadSurfacesParseError confirms a malformed TOML file
// (no '=' on a line) errors through loadTOML.
func TestAgentTOML_LoadSurfacesParseError(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{"broken.toml": []byte("not a valid toml line\n")})
	codex, _ := AgentAdapterByID(ProviderCodex)
	if _, err := codex.Load(filepath.Join(dir, "broken.toml")); err == nil {
		t.Fatalf("expected a parse error")
	}
}

// TestAgentTOML_LoadSurfacesFieldDecodeError confirms a type mismatch on a
// mapped field (e.g. model as a number, not a string) errors naming the
// field, rather than silently coercing or ignoring it.
func TestAgentTOML_LoadSurfacesFieldDecodeError(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.toml": []byte("name = \"reviewer\"\ndescription = \"d\"\ndeveloper_instructions = \"body\"\nmodel = 5\n"),
	})
	codex, _ := AgentAdapterByID(ProviderCodex)
	_, err := codex.Load(filepath.Join(dir, "reviewer.toml"))
	if err == nil {
		t.Fatalf("expected a field-decode error for model = 5")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error should name the field: %v", err)
	}
}

// TestAgentTOML_LoadSurfacesInvalidNameError confirms an on-disk "name"
// value that fails ValidateName is rejected rather than written back out
// on a later Project() call.
func TestAgentTOML_LoadSurfacesInvalidNameError(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.toml": []byte("name = \"../evil\"\ndescription = \"d\"\ndeveloper_instructions = \"body\"\n"),
	})
	codex, _ := AgentAdapterByID(ProviderCodex)
	if _, err := codex.Load(filepath.Join(dir, "reviewer.toml")); err == nil {
		t.Fatalf("expected a name-validation error")
	}
}

// TestAgentTOML_LoadPrintsSkippedTableWarning confirms a file containing a
// table header still loads successfully (the flat keys before it), and
// exercises the stderr warning-print path (no assertion on stderr content
// itself — just that Load doesn't error and the flat fields are captured).
func TestAgentTOML_LoadPrintsSkippedTableWarning(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"reviewer.toml": []byte("name = \"reviewer\"\ndescription = \"d\"\ndeveloper_instructions = \"body\"\n[mcp_servers]\nfoo = \"bar\"\n"),
	})
	codex, _ := AgentAdapterByID(ProviderCodex)
	ag, err := codex.Load(filepath.Join(dir, "reviewer.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ag.Name != "reviewer" {
		t.Fatalf("Name = %q, want reviewer", ag.Name)
	}
}

// TestTomlEscapeUnescapeBasic_RoundTripsAllEscapes exercises every branch
// of tomlEscapeBasic/tomlUnescapeBasic directly: backslash, double quote,
// tab, carriage return, an ordinary rune, and an unknown escape sequence
// (permissively passed through with the backslash stripped) plus a
// trailing lone backslash (malformed input — must not panic).
func TestTomlEscapeUnescapeBasic_RoundTripsAllEscapes(t *testing.T) {
	in := "back\\slash quote\" tab\t cr\r plain"
	escaped := tomlEscapeBasic(in)
	if strings.Contains(escaped, "\t") || strings.Contains(escaped, "\r") {
		t.Fatalf("escaped value should not contain a literal tab/CR: %q", escaped)
	}
	got := tomlUnescapeBasic(escaped)
	if got != in {
		t.Fatalf("round trip = %q, want %q", got, in)
	}

	// Unknown escape sequence: backslash stripped, next rune kept as-is.
	if got := tomlUnescapeBasic(`\q`); got != "q" {
		t.Errorf("tomlUnescapeBasic(\\q) = %q, want q", got)
	}
	// Trailing lone backslash (nothing follows) must not panic and should
	// surface the backslash itself.
	if got := tomlUnescapeBasic(`x\`); got != `x\` {
		t.Errorf("tomlUnescapeBasic(x\\) = %q, want x\\", got)
	}
}

// TestParseTOMLStringArray_MalformedInputs exercises parseTOMLStringArray's
// error branches directly: missing brackets, and a non-string element.
func TestParseTOMLStringArray_MalformedInputs(t *testing.T) {
	if _, err := parseTOMLStringArray(`"a", "b"`); err == nil {
		t.Errorf("expected an error for input missing surrounding brackets")
	}
	if _, err := parseTOMLStringArray(`[1, 2]`); err == nil {
		t.Errorf("expected an error for a non-string array element")
	}
	got, err := parseTOMLStringArray(`[]`)
	if err != nil || got != nil {
		t.Errorf("parseTOMLStringArray([]) = (%#v, %v), want (nil, nil)", got, err)
	}
}

// TestAgentTOML_ValidateNameRejectsInvalidName confirms Project() validates
// the agent name before serializing, matching the markdown codec's
// behavior.
func TestAgentTOML_ValidateNameRejectsInvalidName(t *testing.T) {
	codex, _ := AgentAdapterByID(ProviderCodex)
	ag := &Agent{Name: "../evil", Description: "d", Body: "b"}
	if _, _, err := codex.Project(ag); err == nil {
		t.Fatalf("expected a validation error for an invalid name")
	}
}

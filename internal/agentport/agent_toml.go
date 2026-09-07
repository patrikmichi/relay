package agentport

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file implements the format: toml agent codec (Codex's custom-agent
// format) with the STDLIB ONLY: no TOML dependency, preserving agentport's
// stdlib+yaml.v3
// invariant. It supports exactly the flat subset a custom-agent TOML file
// needs: basic strings, multi-line basic strings (`"""..."""`, used for
// `developer_instructions`), arrays of strings, integers, floats, booleans,
// and comments. A table header (`[section]`) or a dotted key (`a.b = 1`) —
// either of which opens a scope this parser doesn't understand
// (`[mcp_servers]`, `skills.config`) — is skipped with a recorded warning
// rather than failing the whole file; Project() never emits one, so a
// round-trip through this package can never produce a table it can't
// re-read.
//
// Both loadTOML/projectTOML reuse the SAME cfg.Frontmatter-driven field
// mapping (agentFieldValue/decodeAgentTOMLField) that the markdown codec
// uses (agent_config_adapter.go) — only the on-disk encoding differs, not
// the IR-binding mechanism.

// loadTOML reads a flat TOML agent file at path into an *Agent, using
// cfg.Frontmatter's IR mapping — the format: toml analogue of Load's
// markdown-frontmatter path.
func (a *agentConfigAdapter) loadTOML(path string) (*Agent, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	values, warnings, err := parseFlatTOML(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "agentport: %s: %s\n", path, w)
	}

	ag := &Agent{}
	for _, f := range a.cfg.Frontmatter {
		raw, ok := values[f.Key]
		if !ok {
			continue
		}
		if err := decodeAgentTOMLField(ag, f.IR, raw); err != nil {
			return nil, fmt.Errorf("%s: field %q: %w", path, f.Key, err)
		}
	}

	// No shipped format: toml config maps an on-disk "name" key today (the
	// filename IS the identifier for Codex's custom-agent files, exactly
	// like the markdown flat-file path's fallback) — infer it from the
	// filename when frontmatter/TOML omits it.
	if ag.Name == "" {
		base := filepath.Base(path)
		ag.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if err := a.validateName(ag.Name); err != nil {
		return nil, err
	}

	ag.Provenance = Provenance{SourceProvider: a.ID(), SourcePath: path}
	return ag, nil
}

// projectTOML serializes ag into a flat TOML document — the format: toml
// analogue of Project's markdown-frontmatter path. Model is re-encoded
// through modelLossForTarget exactly like the markdown path.
func (a *agentConfigAdapter) projectTOML(ag *Agent) (map[string][]byte, []LossItem, error) {
	var modelLoss *LossItem
	var sb strings.Builder
	for _, f := range a.cfg.Frontmatter {
		val, isZero := agentFieldValue(ag, f.IR)
		if f.IR == "model" && !isZero {
			mapped, l := modelLossForTarget(ag.Model, ag.Provenance.SourceProvider, a.ID())
			val = mapped
			modelLoss = l
		}
		if isZero {
			continue
		}
		encoded, err := encodeTOMLValue(f.IR, val)
		if err != nil {
			return nil, nil, fmt.Errorf("encode field %q: %w", f.Key, err)
		}
		sb.WriteString(f.Key)
		sb.WriteString(" = ")
		sb.WriteString(encoded)
		sb.WriteString("\n")
	}

	files := map[string][]byte{ag.Name + a.FileExt(): []byte(sb.String())}

	loss := computeAgentLoss(ag, a.caps)
	if modelLoss != nil {
		loss = append(loss, *modelLoss)
	}
	if l := toolsLossForTarget(ag.Tools, ag.DeniedTools, a.ID()); l != nil {
		loss = append(loss, *l)
	}

	return files, loss, nil
}

// decodeAgentTOMLField assigns one already-decoded Go value (string, bool,
// int64, float64, or []string — see parseFlatTOML) onto the corresponding
// *Agent field. Only the IR names a format: toml config actually declares
// need a case here (today: name, description, model, body); an
// unrecognized ir is a config bug (config.go's validate already rejects an
// unrecognized ir name before this is ever reached), so this returns an
// error rather than silently ignoring it.
func decodeAgentTOMLField(a *Agent, ir string, raw interface{}) error {
	asString := func() (string, error) {
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("expected a string, got %T", raw)
		}
		return s, nil
	}
	switch ir {
	case "name":
		v, err := asString()
		if err != nil {
			return err
		}
		a.Name = v
	case "description":
		v, err := asString()
		if err != nil {
			return err
		}
		a.Description = v
	case "model":
		v, err := asString()
		if err != nil {
			return err
		}
		a.Model = v
	case "body":
		v, err := asString()
		if err != nil {
			return err
		}
		a.Body = v
	default:
		return fmt.Errorf("toml codec: unsupported ir %q", ir)
	}
	return nil
}

// encodeTOMLValue renders one Go value as a TOML value literal.
// ir == "body" gets the multi-line literal treatment (developer_instructions
// is the only body-shaped field any shipped format: toml config declares);
// every other string is a single-line basic string.
func encodeTOMLValue(ir string, val interface{}) (string, error) {
	switch v := val.(type) {
	case string:
		if ir == "body" {
			return encodeTOMLMultilineString(v), nil
		}
		return encodeTOMLBasicString(v), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case flexStringList:
		return encodeTOMLStringArray([]string(v)), nil
	case []string:
		return encodeTOMLStringArray(v), nil
	default:
		return "", fmt.Errorf("toml encode: unsupported value type %T for ir %q", val, ir)
	}
}

// --- flat-subset TOML encode helpers ---

func encodeTOMLStringArray(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = encodeTOMLBasicString(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// tomlEscapeBasic applies the basic-string escape set (shared by
// single-line and multi-line basic strings — TOML's "Basic Strings" spec).
func tomlEscapeBasic(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// encodeTOMLBasicString renders a single-line TOML basic string. Newlines
// are escaped (`\n`) — every shipped format: toml field using this encoder
// (name/description/model) is documented single-line content.
func encodeTOMLBasicString(s string) string {
	escaped := strings.ReplaceAll(tomlEscapeBasic(s), "\n", `\n`)
	return `"` + escaped + `"`
}

// encodeTOMLMultilineString renders s as a TOML multi-line basic string
// (`"""..."""`), preserving real newlines verbatim (multi-line basic
// strings permit literal newlines — that's the entire point for a
// multi-paragraph `developer_instructions` body). Backslashes and carriage
// returns are always escaped (a raw \r would otherwise collide with
// parseFlatTOML's CRLF-normalization on the next Load); quotes are escaped
// every 3rd consecutive occurrence, which guarantees NO run of 3+ raw
// quotes ever survives ANYWHERE in the emitted content — not just at the
// string boundary — so parseTOMLMultilineString's escape-aware scan can
// never mistake a quote-run inside the content for the closing delimiter
// (the bug this replaced: the old encoder only broke up exact 3-quote runs
// via strings.ReplaceAll, which left runs of 4/5/7/8/... quotes with an
// unescaped "\"\"\"" inside them). A trailing raw quote (or two) is escaped
// as well, even though the TOML spec permits 1-2 unescaped quotes
// immediately before a closing """ — kept conservative so the file never
// looks ambiguous to a naive reader/formatter.
func encodeTOMLMultilineString(s string) string {
	// Byte-wise (not rune-wise): the only bytes this loop treats specially
	// are single-byte ASCII ('\\', '"', '\r'); every UTF-8 continuation/lead
	// byte falls through the default case and is copied verbatim, so
	// multi-byte runes — and even invalid UTF-8, which a rune-based
	// iteration would silently replace with U+FFFD — survive byte-for-byte.
	var b strings.Builder
	quoteRun := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
			quoteRun = 0
		case '"':
			quoteRun++
			if quoteRun == 3 {
				b.WriteString(`\"`)
				quoteRun = 0
			} else {
				b.WriteByte('"')
			}
		case '\r':
			b.WriteString(`\r`)
			quoteRun = 0
		default:
			b.WriteByte(c)
			quoteRun = 0
		}
	}
	escaped := b.String()
	if strings.HasSuffix(escaped, `"`) && !strings.HasSuffix(escaped, `\"`) {
		escaped = escaped[:len(escaped)-1] + `\"`
	}
	return "\"\"\"\n" + escaped + "\"\"\""
}

// --- flat-subset TOML decode ---

// parseFlatTOML parses the flat top-level-key subset of raw as TOML:
// basic strings, multi-line basic strings, arrays of strings, integers,
// floats, booleans, and comments (full-line only). The first table header
// (`[...]`) encountered stops key/value parsing — everything from that
// point on is necessarily nested under some table per TOML's grammar — and
// is recorded as a warning rather than an error. A dotted key
// (`a.b = value`) at top level is likewise recorded as a warning and
// skipped (its value is simply not captured), without affecting any other
// key's parsing.
func parseFlatTOML(raw []byte) (values map[string]interface{}, warnings []string, err error) {
	values = map[string]interface{}{}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			warnings = append(warnings, fmt.Sprintf("skipping table %s (not supported by relay's flat-TOML agent codec)", line))
			break // everything after a table header is that table's content
		}

		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, warnings, fmt.Errorf("line %d: no '=' found: %q", i+1, lines[i])
		}
		key := strings.Trim(strings.TrimSpace(line[:eq]), `"'`)
		rest := strings.TrimSpace(line[eq+1:])

		if strings.Contains(key, ".") {
			warnings = append(warnings, fmt.Sprintf("skipping dotted key %q (not supported by relay's flat-TOML agent codec)", key))
			continue
		}

		switch {
		case strings.HasPrefix(rest, `"""`):
			val, consumed, perr := parseTOMLMultilineString(lines, i, rest)
			if perr != nil {
				return nil, warnings, perr
			}
			values[key] = val
			i = consumed
		case strings.HasPrefix(rest, `"`):
			val, perr := parseTOMLBasicString(rest)
			if perr != nil {
				return nil, warnings, fmt.Errorf("line %d: %w", i+1, perr)
			}
			values[key] = val
		case strings.HasPrefix(rest, "["):
			val, perr := parseTOMLStringArray(rest)
			if perr != nil {
				return nil, warnings, fmt.Errorf("line %d: %w", i+1, perr)
			}
			values[key] = val
		case rest == "true":
			values[key] = true
		case rest == "false":
			values[key] = false
		default:
			if iv, perr := strconv.ParseInt(rest, 10, 64); perr == nil {
				values[key] = iv
			} else if fv, perr := strconv.ParseFloat(rest, 64); perr == nil {
				values[key] = fv
			} else {
				return nil, warnings, fmt.Errorf("line %d: unsupported value %q for key %q", i+1, rest, key)
			}
		}
	}
	return values, warnings, nil
}

// parseTOMLMultilineString parses a `"""..."""` value that may start on the
// same line as the key (firstLineRest is everything after the key's "="
// already trimmed, starting with the opening `"""`) and continue across
// subsequent lines. Returns the decoded string and the index of the last
// line consumed (so the caller's loop can skip past it).
//
// Reassembles the remaining source (from just past the opening delimiter to
// the end of the file) into one string and hands it to scanTOMLMultilineBody,
// which does the actual escape-aware terminator search — see that function's
// doc comment for why a naive strings.Index(line, `"""`) search (the old
// approach) silently truncated content containing a 4+/5+/7+-quote run.
func parseTOMLMultilineString(lines []string, startIdx int, firstLineRest string) (string, int, error) {
	raw := firstLineRest[3:]
	if startIdx+1 < len(lines) {
		raw += "\n" + strings.Join(lines[startIdx+1:], "\n")
	}

	value, endOffset, err := scanTOMLMultilineBody(raw)
	if err != nil {
		return "", startIdx, fmt.Errorf("line %d: %w", startIdx+1, err)
	}
	consumedLines := strings.Count(raw[:endOffset], "\n")
	return value, startIdx + consumedLines, nil
}

// scanTOMLMultilineBody decodes the content of a multi-line basic string
// given raw = everything after the opening `"""` delimiter (which may span
// many lines, already joined by "\n"). It returns the decoded value and the
// byte offset in raw of the character immediately after the closing
// delimiter (so the caller can tell how many lines were consumed).
//
// Per the TOML spec, a newline immediately following the opening delimiter
// is trimmed (not part of the value) — this is exactly the newline that
// separates the "key = \"\"\"" line from the next line when the opening
// delimiter has nothing else after it on its line. encodeTOMLMultilineString
// always emits the delimiter in this "nothing else on the line" shape, so
// every value this package itself writes round-trips through this trim
// rule correctly.
//
// The scan is escape-aware: it walks byte-by-byte, consuming backslash
// escapes atomically (via decodeTOMLEscape) BEFORE ever checking whether a
// '"' starts a quote-run, so an escaped quote can never be mis-parsed as
// part of the closing delimiter — unlike a plain substring search, which is
// exactly what let a quote-run inside the encoder's own escaped output (e.g.
// `abc"""""def`, 5 quotes) fool the old decoder into stopping early. A run
// of 3, 4, or 5 raw quotes is recognized as [0-2 content quotes] + the
// closing `"""`, per the TOML spec's allowance for 1-2 unescaped quotes
// immediately before the terminator.
func scanTOMLMultilineBody(raw string) (string, int, error) {
	body := raw
	skipped := 0
	if strings.HasPrefix(body, "\n") {
		body = body[1:]
		skipped = 1
	}

	var b strings.Builder
	i := 0
	for i < len(body) {
		switch body[i] {
		case '"':
			run := 0
			for i+run < len(body) && body[i+run] == '"' {
				run++
			}
			if run >= 3 {
				contentQuotes := run - 3
				b.WriteString(body[i : i+contentQuotes])
				return b.String(), skipped + i + run, nil
			}
			b.WriteString(body[i : i+run])
			i += run
		case '\\':
			consumed, decoded, continuation, err := decodeTOMLEscape(body[i:])
			if err != nil {
				return "", 0, err
			}
			if !continuation {
				b.WriteString(decoded)
			}
			i += consumed
		default:
			b.WriteByte(body[i])
			i++
		}
	}
	return "", 0, fmt.Errorf("unterminated multi-line string")
}

// decodeTOMLEscape decodes one backslash escape sequence at the start of s
// (s[0] must be '\\'). Returns the number of bytes consumed, the decoded
// text (empty for a line-ending-backslash continuation), and whether this
// was a line-ending-backslash continuation (which contributes no text —
// TOML trims the newline and all following whitespace).
func decodeTOMLEscape(s string) (consumed int, decoded string, continuation bool, err error) {
	if len(s) < 2 {
		return 0, "", false, fmt.Errorf("dangling backslash at end of multi-line string")
	}
	switch s[1] {
	case '"':
		return 2, `"`, false, nil
	case '\\':
		return 2, `\`, false, nil
	case 'n':
		return 2, "\n", false, nil
	case 't':
		return 2, "\t", false, nil
	case 'r':
		return 2, "\r", false, nil
	case 'b':
		return 2, "\b", false, nil
	case 'f':
		return 2, "\f", false, nil
	case 'u', 'U':
		n := 4
		if s[1] == 'U' {
			n = 8
		}
		if len(s) < 2+n {
			return 0, "", false, fmt.Errorf("truncated \\%c unicode escape", s[1])
		}
		hex := s[2 : 2+n]
		cp, perr := strconv.ParseUint(hex, 16, 32)
		if perr != nil {
			return 0, "", false, fmt.Errorf("invalid \\%c escape %q: %w", s[1], hex, perr)
		}
		return 2 + n, string(rune(cp)), false, nil
	case '\n', ' ', '\t':
		// Line-ending backslash: a backslash that is the last non-whitespace
		// character on its line trims that newline plus all following
		// whitespace up to the next non-whitespace rune.
		j := 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) || s[j] != '\n' {
			return 0, "", false, fmt.Errorf("invalid escape sequence %q", s[:2])
		}
		j++
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n') {
			j++
		}
		return j, "", true, nil
	default:
		return 0, "", false, fmt.Errorf("invalid escape sequence %q", s[:2])
	}
}

// parseTOMLBasicString parses a single-line `"..."` value.
func parseTOMLBasicString(s string) (string, error) {
	if len(s) < 2 || !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return "", fmt.Errorf("malformed basic string: %q", s)
	}
	return tomlUnescapeBasic(s[1 : len(s)-1]), nil
}

// parseTOMLStringArray parses a single-line `["a", "b"]` array of basic
// strings. Empty array `[]` returns a nil slice (matching flexStringList's
// omitempty-style zero value elsewhere in this package).
func parseTOMLStringArray(s string) ([]string, error) {
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("malformed array: %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := parseTOMLBasicString(part)
		if err != nil {
			return nil, fmt.Errorf("array element %q: %w", part, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// tomlUnescapeBasic reverses tomlEscapeBasic's escape set. Unknown escape
// sequences are passed through with the backslash stripped (permissive —
// this codec only needs to correctly round-trip what encodeTOML* itself
// emits, not parse arbitrary hand-authored TOML with every possible
// escape).
func tomlUnescapeBasic(s string) string {
	var b bytes.Buffer
	r := bufio.NewReader(strings.NewReader(s))
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			break
		}
		if ch != '\\' {
			b.WriteRune(ch)
			continue
		}
		next, _, err := r.ReadRune()
		if err != nil {
			b.WriteRune(ch)
			break
		}
		switch next {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteRune(next)
		}
	}
	return b.String()
}

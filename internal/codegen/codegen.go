// Package codegen turns the JSON Schemas of trigger payloads and tool inputs
// into TypeScript and Python types.
package codegen

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Named is a schema and the key it is looked up by, such as github.push.
type Named struct {
	Key    string
	Schema map[string]any
}

// Input is everything one generated file holds.
type Input struct {
	// Header is a comment line at the top.
	Header string
	// Triggers are payload schemas keyed by trigger type.
	Triggers []Named
	// Tools are input schemas keyed by service.tool.
	Tools []Named
}

var nonWord = regexp.MustCompile(`[^A-Za-z0-9]+`)

// pascal turns github.issue_opened into GithubIssueOpened.
func pascal(s string) string {
	var b strings.Builder
	for _, part := range nonWord.Split(s, -1) {
		if part == "" {
			continue
		}
		r := []rune(part)
		b.WriteRune(unicode.ToUpper(r[0]))
		b.WriteString(string(r[1:]))
	}
	out := b.String()
	if out == "" || unicode.IsDigit([]rune(out)[0]) {
		out = "T" + out
	}
	return out
}

type namer struct{ used map[string]bool }

func (n *namer) unique(base string) string {
	name := base
	for i := 2; n.used[name]; i++ {
		name = base + strconv.Itoa(i)
	}
	n.used[name] = true
	return name
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func required(s map[string]any) map[string]bool {
	out := map[string]bool{}
	if list, ok := s["required"].([]any); ok {
		for _, r := range list {
			if name, ok := r.(string); ok {
				out[name] = true
			}
		}
	}
	return out
}

func types(s map[string]any) []string {
	switch t := s["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if str, ok := x.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func literal(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t), true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case nil:
		return "null", true
	}
	return "", false
}

func comment(s string) string {
	s = strings.NewReplacer("*/", "* /", "\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	return s
}

// TypeScript renders the types as one module.
func TypeScript(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// %s\n\n", in.Header)
	n := &namer{used: map[string]bool{"TriggerPayloads": true, "ToolInputs": true}}
	emit := func(items []Named, suffix, registry string) {
		type reg struct{ key, name string }
		var regs []reg
		for _, it := range items {
			name := n.unique(pascal(it.Key) + suffix)
			if d := comment(strDesc(it.Schema)); d != "" {
				fmt.Fprintf(&b, "/** %s */\n", d)
			}
			fmt.Fprintf(&b, "export type %s = %s;\n\n", name, tsType(it.Schema, 0))
			regs = append(regs, reg{it.Key, name})
		}
		fmt.Fprintf(&b, "export interface %s {\n", registry)
		for _, r := range regs {
			fmt.Fprintf(&b, "  %s: %s;\n", strconv.Quote(r.key), r.name)
		}
		b.WriteString("}\n\n")
	}
	emit(in.Triggers, "Payload", "TriggerPayloads")
	emit(in.Tools, "Input", "ToolInputs")
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func strDesc(s map[string]any) string { d, _ := s["description"].(string); return d }

var tsIdent = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func tsType(s map[string]any, depth int) string {
	if s == nil {
		return "unknown"
	}
	if c, ok := s["const"]; ok {
		if l, ok := literal(c); ok {
			return l
		}
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		var parts []string
		for _, v := range e {
			l, ok := literal(v)
			if !ok {
				return "unknown"
			}
			parts = append(parts, l)
		}
		return strings.Join(parts, " | ")
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if list, ok := s[key].([]any); ok && len(list) > 0 {
			var parts []string
			for _, x := range list {
				parts = append(parts, tsType(asMap(x), depth))
			}
			return strings.Join(parts, " | ")
		}
	}
	if list, ok := s["allOf"].([]any); ok && len(list) > 0 {
		var parts []string
		for _, x := range list {
			parts = append(parts, "("+tsType(asMap(x), depth)+")")
		}
		return strings.Join(parts, " & ")
	}
	ts := types(s)
	if len(ts) == 0 {
		if s["properties"] != nil {
			ts = []string{"object"}
		} else {
			return "unknown"
		}
	}
	var parts []string
	for _, t := range ts {
		switch t {
		case "string":
			parts = append(parts, "string")
		case "integer", "number":
			parts = append(parts, "number")
		case "boolean":
			parts = append(parts, "boolean")
		case "null":
			parts = append(parts, "null")
		case "array":
			item := "unknown"
			if it := asMap(s["items"]); it != nil {
				item = tsType(it, depth+1)
			}
			if strings.ContainsAny(item, " |&") {
				item = "(" + item + ")"
			}
			parts = append(parts, item+"[]")
		case "object":
			parts = append(parts, tsObject(s, depth))
		default:
			parts = append(parts, "unknown")
		}
	}
	return strings.Join(parts, " | ")
}

func tsObject(s map[string]any, depth int) string {
	props := asMap(s["properties"])
	extra := s["additionalProperties"]
	if len(props) == 0 {
		switch e := extra.(type) {
		case map[string]any:
			return "Record<string, " + tsType(e, depth+1) + ">"
		case bool:
			if !e {
				return "Record<string, never>"
			}
		}
		return "Record<string, unknown>"
	}
	req := required(s)
	pad := strings.Repeat("  ", depth+1)
	var b strings.Builder
	b.WriteString("{\n")
	for _, k := range sortedKeys(props) {
		ps := asMap(props[k])
		if d := comment(strDesc(ps)); d != "" {
			fmt.Fprintf(&b, "%s/** %s */\n", pad, d)
		}
		name := k
		if !tsIdent.MatchString(k) {
			name = strconv.Quote(k)
		}
		opt := "?"
		if req[k] {
			opt = ""
		}
		fmt.Fprintf(&b, "%s%s%s: %s;\n", pad, name, opt, tsType(ps, depth+1))
	}
	if m, ok := extra.(map[string]any); ok {
		fmt.Fprintf(&b, "%s[key: string]: %s;\n", pad, tsType(m, depth+1))
	}
	b.WriteString(strings.Repeat("  ", depth) + "}")
	return b.String()
}

// Python renders the types as one module of TypedDicts.
func Python(in Input) string {
	g := &pyGen{n: &namer{used: map[string]bool{"TriggerPayloads": true, "ToolInputs": true}}, top: map[string]bool{}}
	var regs [2][][2]string
	for i, group := range []struct {
		items  []Named
		suffix string
	}{{in.Triggers, "Payload"}, {in.Tools, "Input"}} {
		for _, it := range group.items {
			name := g.n.unique(pascal(it.Key) + group.suffix)
			g.top[name] = true
			expr := g.expr(it.Schema, name)
			delete(g.top, name)
			if expr != name {
				g.defs = append(g.defs, fmt.Sprintf("%s = %s", name, expr))
			}
			regs[i] = append(regs[i], [2]string{it.Key, name})
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nfrom typing import Any, Literal, Required, TypedDict\n\n", in.Header)
	for _, d := range g.defs {
		b.WriteString(d + "\n\n")
	}
	for i, name := range []string{"TriggerPayloads", "ToolInputs"} {
		fmt.Fprintf(&b, "%s = TypedDict(%q, {", name, name)
		for j, r := range regs[i] {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s: %s", strconv.Quote(r[0]), r[1])
		}
		b.WriteString("})\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

type pyGen struct {
	n    *namer
	defs []string
	// top holds the names reserved for top-level types, which an object schema may take as its own.
	top map[string]bool
}

// expr returns the Python type of a schema; objects with properties become
// named TypedDicts (called hint when they are the top-level type).
func (g *pyGen) expr(s map[string]any, hint string) string {
	if s == nil {
		return "Any"
	}
	if c, ok := s["const"]; ok {
		if l, ok := literal(c); ok {
			return "Literal[" + pyLiteral(l) + "]"
		}
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		var parts []string
		for _, v := range e {
			l, ok := literal(v)
			if !ok {
				return "Any"
			}
			parts = append(parts, pyLiteral(l))
		}
		return "Literal[" + strings.Join(parts, ", ") + "]"
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if list, ok := s[key].([]any); ok && len(list) > 0 {
			var parts []string
			for i, x := range list {
				parts = append(parts, g.expr(asMap(x), hint+strconv.Itoa(i+1)))
			}
			return strings.Join(parts, " | ")
		}
	}
	ts := types(s)
	if len(ts) == 0 {
		if s["properties"] != nil {
			ts = []string{"object"}
		} else {
			return "Any"
		}
	}
	var parts []string
	for _, t := range ts {
		switch t {
		case "string":
			parts = append(parts, "str")
		case "integer":
			parts = append(parts, "int")
		case "number":
			parts = append(parts, "float")
		case "boolean":
			parts = append(parts, "bool")
		case "null":
			parts = append(parts, "None")
		case "array":
			item := "Any"
			if it := asMap(s["items"]); it != nil {
				item = g.expr(it, hint+"Item")
			}
			parts = append(parts, "list["+item+"]")
		case "object":
			parts = append(parts, g.object(s, hint))
		default:
			parts = append(parts, "Any")
		}
	}
	return strings.Join(parts, " | ")
}

func pyLiteral(l string) string {
	switch l {
	case "true":
		return "True"
	case "false":
		return "False"
	case "null":
		return "None"
	}
	return l
}

func (g *pyGen) object(s map[string]any, hint string) string {
	props := asMap(s["properties"])
	if len(props) == 0 {
		if m, ok := s["additionalProperties"].(map[string]any); ok {
			return "dict[str, " + g.expr(m, hint+"Value") + "]"
		}
		return "dict[str, Any]"
	}
	req := required(s)
	name := hint
	if g.top[name] {
		delete(g.top, name)
	} else {
		name = g.n.unique(hint)
	}
	var fields []string
	for _, k := range sortedKeys(props) {
		t := g.expr(asMap(props[k]), name+pascal(k))
		if req[k] {
			t = "Required[" + t + "]"
		}
		fields = append(fields, strconv.Quote(k)+": "+t)
	}
	g.defs = append(g.defs, fmt.Sprintf("%s = TypedDict(%q, {%s}, total=False)", name, name, strings.Join(fields, ", ")))
	return name
}

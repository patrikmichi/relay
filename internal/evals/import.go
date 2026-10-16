package evals

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Unmapped is something in a foreign file that has no equivalent here. It is
// listed, never converted into something weaker.
type Unmapped struct {
	Case   string `json:"case"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// ImportReport says what an import did and did not carry over.
type ImportReport struct {
	Source   string     `json:"source"`
	Cases    int        `json:"cases"`
	Unmapped []Unmapped `json:"unmapped,omitempty"`
	Notes    []string   `json:"notes,omitempty"`
}

// ImportSources are the formats `eval datasets import --from` accepts.
var ImportSources = []string{"jsonl", "csv", "promptfoo", "openai", "skill-creator", "claude-plugin"}

var inputVarNames = []string{"input", "question", "query", "prompt", "message", "user_input", "text"}

func inputFromVars(vars map[string]string) string {
	for _, n := range inputVarNames {
		if v, ok := vars[n]; ok {
			return v
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return vars[keys[0]]
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// ImportPromptfoo converts the tests of a promptfoo config.
func ImportPromptfoo(raw []byte) ([]Case, *ImportReport, error) {
	var doc struct {
		Prompts  []any `yaml:"prompts"`
		Tests    []any `yaml:"tests"`
		Defaults struct {
			Assert []map[string]any `yaml:"assert"`
		} `yaml:"defaultTest"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("promptfoo config: %w", err)
	}
	if len(doc.Tests) == 0 {
		return nil, nil, errors.New("the promptfoo config has no tests (a tests: list inline; tests loaded from other files are not followed)")
	}
	rep := &ImportReport{Source: "promptfoo"}
	for _, p := range doc.Prompts {
		if s, ok := p.(string); ok {
			rep.Notes = append(rep.Notes, "prompt: "+truncate(s, 120)+" (use it as the target's template)")
		}
	}
	var cases []Case
	for i, t := range doc.Tests {
		tm, ok := t.(map[string]any)
		if !ok {
			rep.Unmapped = append(rep.Unmapped, Unmapped{Case: "test " + strconv.Itoa(i+1), Type: "test", Reason: "a test given as a file reference or string is not followed"})
			continue
		}
		vars := map[string]string{}
		if vm, ok := tm["vars"].(map[string]any); ok {
			for k, v := range vm {
				vars[k] = stringify(v)
			}
		}
		name := stringify(tm["description"])
		if name == "" {
			name = "test " + strconv.Itoa(i+1)
		}
		in := Input{Text: inputFromVars(vars)}
		if len(vars) > 0 {
			in.Variables = vars
		}
		c := Case{Kind: KindTask, Name: name, Input: in, Expected: &Expected{}}
		asserts, _ := tm["assert"].([]any)
		for _, d := range doc.Defaults.Assert {
			asserts = append(asserts, any(d))
		}
		var rubrics []string
		for _, a := range asserts {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			typ := stringify(am["type"])
			mapPromptfooAssertion(c.Name, typ, am, c.Expected, &rubrics, rep)
		}
		if len(rubrics) > 0 {
			c.Expected.Rubric = strings.Join(rubrics, "\n")
		}
		if c.Input.Text == "" {
			rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "test", Reason: "no vars to use as the input"})
			continue
		}
		if len(c.Expected.Assertions) == 0 && c.Expected.Rubric == "" {
			rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "test", Reason: "none of its assertions could be converted, so the case has no way to be graded"})
			continue
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, rep, errors.New("no test could be converted")
	}
	rep.Cases = len(cases)
	return disambiguate(cases), rep, nil
}

func mapPromptfooAssertion(caseName, typ string, am map[string]any, exp *Expected, rubrics *[]string, rep *ImportReport) {
	unmapped := func(reason string) {
		rep.Unmapped = append(rep.Unmapped, Unmapped{Case: caseName, Type: typ, Reason: reason})
	}
	val := stringify(am["value"])
	add := func(a Assertion) { exp.Assertions = append(exp.Assertions, a) }
	negated := strings.HasPrefix(typ, "not-")
	base := strings.TrimPrefix(typ, "not-")
	switch base {
	case "contains", "icontains", "equals":
		if base == "equals" && negated {
			unmapped("there is no negated equals")
			return
		}
		t := base
		if negated {
			t = "not_" + base
		}
		add(Assertion{Type: t, Value: val})
	case "regex":
		if negated {
			unmapped("there is no negated regex")
			return
		}
		if _, err := regexp.Compile(val); err != nil {
			unmapped("the pattern is not valid RE2 (" + firstLine(err.Error()) + ")")
			return
		}
		add(Assertion{Type: "regex", Pattern: val})
	case "contains-all":
		vals, _ := am["value"].([]any)
		for _, v := range vals {
			t := "contains"
			if negated {
				t = "not_contains"
			}
			add(Assertion{Type: t, Value: stringify(v)})
		}
	case "contains-any", "icontains-any":
		vals, _ := am["value"].([]any)
		var alts []string
		for _, v := range vals {
			alts = append(alts, regexp.QuoteMeta(stringify(v)))
		}
		if len(alts) == 0 || negated {
			unmapped("not a plain list of strings")
			return
		}
		prefix := ""
		if base == "icontains-any" {
			prefix = "(?i)"
		}
		add(Assertion{Type: "regex", Pattern: prefix + "(" + strings.Join(alts, "|") + ")"})
	case "is-json":
		if negated {
			unmapped("there is no negated is-json")
			return
		}
		schema := am["value"]
		if schema == nil {
			schema = map[string]any{"type": []any{"object", "array"}}
		}
		add(Assertion{Type: "json_schema", Schema: schema})
	case "llm-rubric":
		if negated {
			unmapped("there is no negated rubric")
			return
		}
		*rubrics = append(*rubrics, val)
	case "cost", "latency":
		n, err := strconv.ParseFloat(stringify(am["threshold"]), 64)
		if err != nil || negated {
			unmapped("no numeric threshold")
			return
		}
		if base == "latency" {
			add(Assertion{Type: "latency_lte", Max: &n})
		} else {
			add(Assertion{Type: "cost_lte", Max: &n})
		}
	default:
		unmapped("no equivalent assertion")
	}
}

func disambiguate(cases []Case) []Case {
	seen := map[string]int{}
	for i := range cases {
		id := cases[i].ID()
		seen[id]++
		if seen[id] > 1 {
			cases[i].Key = id + "-" + strconv.Itoa(seen[id])
		}
	}
	return cases
}

// ImportOpenAI converts OpenAI Evals items (one JSON object per line) with
// the testing criteria that grade them. Without criteria each item's ideal
// answer becomes the reference of a rubric.
func ImportOpenAI(items []byte, criteria []byte) ([]Case, *ImportReport, error) {
	rep := &ImportReport{Source: "openai"}
	var crit []map[string]any
	if len(bytes.TrimSpace(criteria)) > 0 {
		var wrapped struct {
			Criteria []map[string]any `json:"testing_criteria"`
		}
		if json.Unmarshal(criteria, &wrapped) == nil && wrapped.Criteria != nil {
			crit = wrapped.Criteria
		} else if err := json.Unmarshal(criteria, &crit); err != nil {
			return nil, nil, fmt.Errorf("testing criteria: %w", err)
		}
	} else {
		rep.Notes = append(rep.Notes, "no testing criteria were given: each item's ideal answer is the reference of a rubric, which needs a judge model")
	}
	var cases []Case
	sc := bufio.NewScanner(bytes.NewReader(items))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(text), &obj); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		item := obj
		if inner, ok := obj["item"].(map[string]any); ok {
			item = inner
		}
		name := "item " + strconv.Itoa(line)
		c := Case{Kind: KindTask, Name: name, Expected: &Expected{}}
		switch in := item["input"].(type) {
		case string:
			c.Input = Input{Text: in}
		case []any:
			for _, m := range in {
				mm, _ := m.(map[string]any)
				c.Input.Messages = append(c.Input.Messages, Message{Role: stringify(mm["role"]), Content: stringify(mm["content"])})
			}
		default:
			if q, ok := item["question"]; ok {
				c.Input = Input{Text: stringify(q)}
			}
		}
		if c.Input.Prompt() == "" {
			rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "item", Reason: "no input, messages or question"})
			continue
		}
		ideal := firstString(item["ideal"], item["expected"], item["answer"], item["reference"])
		if crit == nil {
			c.Expected.Rubric, c.Expected.Reference = "The answer matches the reference answer in substance.", ideal
		}
		for _, k := range crit {
			mapOpenAICriterion(name, k, item, c.Expected, rep)
		}
		if len(c.Expected.Assertions) == 0 && c.Expected.Rubric == "" {
			rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "item", Reason: "none of the testing criteria could be converted"})
			continue
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(cases) == 0 {
		return nil, rep, errors.New("no item could be converted")
	}
	rep.Cases = len(cases)
	return disambiguate(cases), rep, nil
}

func firstString(vs ...any) string {
	for _, v := range vs {
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case []any:
			if len(t) > 0 {
				return stringify(t[0])
			}
		}
	}
	return ""
}

var itemRef = regexp.MustCompile(`^\{\{\s*item\.([A-Za-z0-9_.-]+)\s*\}\}$`)

func mapOpenAICriterion(caseName string, k map[string]any, item map[string]any, exp *Expected, rep *ImportReport) {
	typ := stringify(k["type"])
	unmapped := func(reason string) {
		rep.Unmapped = append(rep.Unmapped, Unmapped{Case: caseName, Type: typ, Reason: reason})
	}
	if typ != "string_check" {
		unmapped("no equivalent grader (text_similarity, label_model and score_model are not converted)")
		return
	}
	ref := stringify(k["reference"])
	if m := itemRef.FindStringSubmatch(ref); m != nil {
		v, ok := ReadPath(item, m[1])
		if !ok {
			unmapped("the item has no field " + m[1])
			return
		}
		ref = stringify(v)
	}
	switch stringify(k["operation"]) {
	case "eq":
		exp.Assertions = append(exp.Assertions, Assertion{Type: "equals", Value: ref})
	case "like":
		exp.Assertions = append(exp.Assertions, Assertion{Type: "contains", Value: ref})
	case "ilike":
		exp.Assertions = append(exp.Assertions, Assertion{Type: "icontains", Value: ref})
	default:
		unmapped("operation " + stringify(k["operation"]) + " has no equivalent")
	}
}

// ImportSkillCreator converts a skill-creator evals.json: each eval's
// expectations become the rubric, its expected output the reference.
func ImportSkillCreator(raw []byte) ([]Case, *ImportReport, error) {
	var doc struct {
		SkillName string `json:"skill_name"`
		Evals     []struct {
			ID           any      `json:"id"`
			Prompt       string   `json:"prompt"`
			Expected     string   `json:"expected_output"`
			Files        []string `json:"files"`
			Expectations []string `json:"expectations"`
			Assertions   []string `json:"assertions"`
		} `json:"evals"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("evals.json: %w", err)
	}
	if len(doc.Evals) == 0 {
		return nil, nil, errors.New("evals.json has no evals")
	}
	rep := &ImportReport{Source: "skill-creator"}
	if doc.SkillName != "" {
		rep.Notes = append(rep.Notes, "skill: "+doc.SkillName)
	}
	var cases []Case
	for i, e := range doc.Evals {
		name := "eval " + stringify(e.ID)
		if e.ID == nil {
			name = "eval " + strconv.Itoa(i+1)
		}
		exp := append(append([]string{}, e.Expectations...), e.Assertions...)
		if len(e.Files) > 0 {
			rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "files", Reason: "input files are not attached to local runs"})
		}
		rubric := strings.Join(exp, "\n")
		if rubric == "" && e.Expected != "" {
			rubric = "The answer matches the expected output in substance."
		}
		if e.Prompt == "" || rubric == "" {
			rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "eval", Reason: "needs a prompt and expectations or an expected output"})
			continue
		}
		cases = append(cases, Case{Kind: KindTask, Name: name, Key: slug(name), Input: Input{Text: e.Prompt}, Expected: &Expected{Rubric: rubric, Reference: e.Expected}})
	}
	if len(cases) == 0 {
		return nil, rep, errors.New("no eval could be converted")
	}
	rep.Cases = len(cases)
	return disambiguate(cases), rep, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// ImportClaudePlugin converts the cases of a `claude plugin eval` suite: a
// directory with one subdirectory per case, each with a prompt.md and graders
// in graders/*.md. Graders that depend on the workspace the run leaves behind,
// on the session trace or on a reference transcript are listed, not converted.
func ImportClaudePlugin(dir string) ([]Case, *ImportReport, error) {
	rep := &ImportReport{Source: "claude-plugin"}
	var cases []Case
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if base := d.Name(); path != dir && (base == "results" || base == "mocks" || base == "graders" || strings.HasPrefix(base, ".")) {
			return filepath.SkipDir
		}
		prompt, perr := os.ReadFile(filepath.Join(path, "prompt.md"))
		if perr != nil {
			if _, serr := os.Stat(filepath.Join(path, "case.yaml")); serr == nil {
				rep.Unmapped = append(rep.Unmapped, Unmapped{Case: filepath.Base(path), Type: "case.yaml", Reason: "a case with no prompt.md is not read"})
			}
			return nil
		}
		c, ok := claudePluginCase(path, prompt, rep)
		if ok {
			cases = append(cases, c)
		}
		return filepath.SkipDir
	})
	if err != nil {
		return nil, nil, err
	}
	if len(cases) == 0 {
		return nil, rep, errors.New("no case with a prompt.md and a convertible grader was found")
	}
	rep.Cases = len(cases)
	sort.Slice(cases, func(i, j int) bool { return cases[i].Key < cases[j].Key })
	return disambiguate(cases), rep, nil
}

func claudePluginCase(dir string, prompt []byte, rep *ImportReport) (Case, bool) {
	var fm struct {
		Name         string   `yaml:"name"`
		Tags         []string `yaml:"tags"`
		AllowedTools []string `yaml:"allowed_tools"`
	}
	body, err := ParseFrontmatter(prompt, &fm)
	name := fm.Name
	if name == "" {
		name = filepath.Base(dir)
	}
	if err != nil {
		rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "prompt.md", Reason: err.Error()})
		return Case{}, false
	}
	if _, err := os.Stat(filepath.Join(dir, "case.yaml")); err == nil {
		rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "case.yaml", Reason: "its context (scaffold script, history, extra directories) is not carried over"})
	}
	if len(fm.AllowedTools) > 0 {
		rep.Notes = append(rep.Notes, name+": allowed_tools are not carried over; the case runs without tools unless you add fixtures")
	}
	c := Case{Kind: KindTask, Name: name, Key: slug(name), Tags: fm.Tags, Input: Input{Text: strings.TrimSpace(body)}, Expected: &Expected{}}
	files, _ := filepath.Glob(filepath.Join(dir, "graders", "*.md"))
	sort.Strings(files)
	var rubrics []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		mapClaudePluginGrader(name, filepath.Base(f), raw, c.Expected, &rubrics, rep)
	}
	if len(rubrics) > 0 {
		c.Expected.Rubric = strings.Join(rubrics, "\n\n")
	}
	if c.Input.Text == "" {
		rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "prompt.md", Reason: "the prompt is empty"})
		return Case{}, false
	}
	if len(c.Expected.Assertions) == 0 && c.Expected.Rubric == "" {
		rep.Unmapped = append(rep.Unmapped, Unmapped{Case: name, Type: "case", Reason: "none of its graders could be converted, so the case has no way to be graded"})
		return Case{}, false
	}
	return c, true
}

func mapClaudePluginGrader(caseName, file string, raw []byte, exp *Expected, rubrics *[]string, rep *ImportReport) {
	var g struct {
		Type       string `yaml:"type"`
		Weight     any    `yaml:"weight"`
		Pattern    string `yaml:"pattern"`
		Flags      string `yaml:"flags"`
		Match      string `yaml:"match"`
		Target     any    `yaml:"target"`
		Focus      any    `yaml:"focus"`
		Tool       string `yaml:"tool"`
		InputMatch string `yaml:"input_match"`
		Min        *int   `yaml:"min"`
		Max        *int   `yaml:"max"`
		Before     any    `yaml:"before"`
		After      any    `yaml:"after"`
	}
	body, err := ParseFrontmatter(raw, &g)
	unmapped := func(reason string) {
		rep.Unmapped = append(rep.Unmapped, Unmapped{Case: caseName, Type: g.Type + " (" + file + ")", Reason: reason})
	}
	if err != nil {
		unmapped(err.Error())
		return
	}
	if g.Weight != nil && stringify(g.Weight) != "1" {
		rep.Notes = append(rep.Notes, caseName+": grader weights are not carried over; every grader counts equally")
	}
	lastMessage := func(v any) bool { return v == nil || v == "last_message" }
	switch g.Type {
	case "regex":
		switch {
		case !lastMessage(g.Target):
			unmapped("only the final message can be checked here")
		case g.Match != "" && g.Match != "contains":
			unmapped("match " + g.Match + " has no equivalent")
		case g.Flags != "" && g.Flags != "i":
			unmapped("flags " + g.Flags + " have no equivalent")
		default:
			pattern := g.Pattern
			if g.Flags == "i" {
				pattern = "(?i)" + pattern
			}
			if _, err := regexp.Compile(pattern); err != nil {
				unmapped("the pattern is not valid RE2 (" + firstLine(err.Error()) + ")")
				return
			}
			exp.Assertions = append(exp.Assertions, Assertion{Type: "regex", Pattern: pattern})
		}
	case "tool_used":
		switch {
		case g.InputMatch != "":
			unmapped("matching the call's input by regex has no equivalent")
		case g.Min != nil && *g.Min == 0 && g.Max != nil && *g.Max == 0:
			exp.Assertions = append(exp.Assertions, Assertion{Type: "no_injection_compliance", Forbidden: []string{g.Tool}})
		case g.Max != nil || (g.Min != nil && *g.Min > 1):
			unmapped("a call count has no equivalent")
		default:
			exp.Assertions = append(exp.Assertions, Assertion{Type: "tool_sequence", Tools: []ToolStep{{Name: g.Tool}}})
		}
	case "tool_order":
		b, bok := g.Before.(string)
		a, aok := g.After.(string)
		if !bok || !aok {
			unmapped("matching a call's input has no equivalent")
			return
		}
		exp.Assertions = append(exp.Assertions, Assertion{Type: "tool_sequence", Tools: []ToolStep{{Name: b}, {Name: a}}})
	case "llm":
		if !lastMessage(g.Focus) {
			unmapped("only the final message can be judged here")
			return
		}
		if strings.TrimSpace(body) == "" {
			unmapped("no criteria")
			return
		}
		*rubrics = append(*rubrics, strings.TrimSpace(body))
	default:
		unmapped("needs the workspace or the reference transcript of a run")
	}
}

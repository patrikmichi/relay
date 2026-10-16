package evals

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/brand"
)

// HarnessVersion is stored with every result: outputs of different harness
// versions are not comparable.
const HarnessVersion = "offline/1"

// maxToolRounds bounds a model loop.
const maxToolRounds = 8

// Cell is one model of a run, with the call settings it uses.
type Cell struct {
	Model       ModelRef
	Provider    Provider
	Temperature *float64
	MaxTokens   int
}

// Key identifies the cell in results.
func (c Cell) Key() string { return c.Model.String() }

// Harness runs one case of a target.
type Harness interface {
	// Subject is the name trigger cases look for among the options.
	Subject() string
	// Digest identifies the artifact version under test.
	Digest() string
	Run(ctx context.Context, c Case, cell Cell) Output
}

// Option is one choice of a trigger case.
type Option struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ParseFrontmatter splits a markdown file into its YAML header and body.
func ParseFrontmatter(raw []byte, into any) (string, error) {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return s, nil
	}
	head, body, ok := strings.Cut(s[4:], "\n---")
	if !ok {
		return s, nil
	}
	if err := yaml.Unmarshal([]byte(head), into); err != nil {
		return "", fmt.Errorf("front matter: %w", err)
	}
	return strings.TrimPrefix(body, "\n"), nil
}

func digestOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}

// NewHarness builds the harness for a suite's target. Paths resolve against
// the config's directory.
func NewHarness(cfg *Config, s *Suite, getenv func(string) string) (Harness, error) {
	t := s.Target
	switch t.Kind {
	case "skill":
		return newSkillHarness(cfg, t)
	case "prompt":
		return newPromptHarness(cfg, t)
	case "agent_config":
		return newAgentHarness(cfg, t)
	case "mcp_server":
		return newMCPHarness(cfg, t)
	case "app":
		return newAppHarness(t, getenv)
	}
	return nil, fmt.Errorf("target %q runs only in the cloud (`%s` without --offline)", t.Kind, brand.Cmd("eval run"))
}

func readTarget(cfg *Config, p, what string) ([]byte, error) {
	if p == "" {
		return nil, fmt.Errorf("the target needs %s", what)
	}
	raw, err := os.ReadFile(cfg.Resolve(p))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return raw, nil
}

func readOptions(cfg *Config, p string) ([]Option, error) {
	if p == "" {
		return nil, nil
	}
	raw, err := readTarget(cfg, p, "the catalog")
	if err != nil {
		return nil, err
	}
	var opts []Option
	if err := json.Unmarshal(raw, &opts); err != nil {
		return nil, fmt.Errorf("catalog %s must be a JSON list of {name, description}: %w", p, err)
	}
	return opts, nil
}

// routeOutput asks the model to choose one option for a request.
func routeOutput(ctx context.Context, cell Cell, opts []Option, request string) Output {
	sorted := append([]Option(nil), opts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	b.WriteString("You route requests. Choose the single option that should handle the request, judging only by the descriptions. ")
	b.WriteString("If none fits, choose none. Answer with JSON only: {\"choice\": \"<name>\"} or {\"choice\": null}.\n\nOptions:\n")
	for _, o := range sorted {
		fmt.Fprintf(&b, "- %s: %s\n", o.Name, o.Description)
	}
	start := time.Now()
	resp, err := cell.Provider.Complete(ctx, ChatRequest{Model: cell.Model.Model, System: b.String(), Turns: []Turn{{Role: "user", Text: request}}, Temperature: cell.Temperature, MaxTokens: 200})
	out := Output{LatencyMs: time.Since(start).Milliseconds()}
	if err != nil {
		out.Error = asCaseError(err)
		return out
	}
	out.Text, out.Usage = resp.Text, resp.Usage
	doc, _ := ExtractJSON(resp.Text)
	if m, ok := doc.(map[string]any); ok {
		if name, ok := m["choice"].(string); ok && name != "" {
			out.Choice = &name
		}
	}
	return out
}

func asCaseError(err error) *CaseError {
	var ce *CaseError
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &CaseError{Kind: ErrTimeout, Message: "the case timed out"}
	}
	return &CaseError{Kind: ErrTarget, Message: err.Error()}
}

const fixtureMissing = "fixture_missing: no fixture matches this call, so the tool was not run"

func answerFromFixtures(fixtures []Fixture, c ToolCall) string {
	for _, f := range fixtures {
		if f.Tool != c.Name || !IsSubset(anyMap(f.Match), anyMap(c.Args)) {
			continue
		}
		if f.Error != "" {
			return "error: " + f.Error
		}
		if s, ok := f.Response.(string); ok {
			return s
		}
		raw, _ := json.Marshal(f.Response)
		return string(raw)
	}
	return fixtureMissing
}

func fixtureTools(fixtures []Fixture, allow []string) []ToolSpec {
	seen := map[string]bool{}
	var out []ToolSpec
	for _, f := range fixtures {
		if seen[f.Tool] || (allow != nil && !contains(allow, f.Tool)) {
			continue
		}
		seen[f.Tool] = true
		out = append(out, ToolSpec{Name: f.Tool, Description: "Tool " + f.Tool})
	}
	return out
}

// loop runs a conversation with fixture-answered tools until the model stops
// calling them.
func loop(ctx context.Context, cell Cell, system string, turns []Turn, tools []ToolSpec, fixtures []Fixture, allow []string) Output {
	var out Output
	start := time.Now()
	for round := 0; ; round++ {
		resp, err := cell.Provider.Complete(ctx, ChatRequest{Model: cell.Model.Model, System: system, Turns: turns, Tools: tools, Temperature: cell.Temperature, MaxTokens: cell.MaxTokens})
		if err != nil {
			out.Error = asCaseError(err)
			break
		}
		out.Usage.InputTokens += resp.Usage.InputTokens
		out.Usage.OutputTokens += resp.Usage.OutputTokens
		out.Text = resp.Text
		if len(resp.ToolCalls) == 0 {
			break
		}
		turns = append(turns, Turn{Role: "assistant", Text: resp.Text, ToolCalls: resp.ToolCalls})
		for _, call := range resp.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, call)
			answer := "error: this tool is not available"
			if allow == nil || contains(allow, call.Name) {
				answer = answerFromFixtures(fixtures, call)
			}
			turns = append(turns, Turn{Role: "tool", Text: answer, ToolCallID: call.ID, ToolName: call.Name})
		}
		if round+1 >= maxToolRounds {
			out.Error = &CaseError{Kind: ErrModel, Message: "the model was still calling tools after " + strconv.Itoa(maxToolRounds) + " rounds"}
			break
		}
	}
	out.LatencyMs = time.Since(start).Milliseconds()
	return out
}

func turnsOf(in Input, last string) []Turn {
	var turns []Turn
	for _, m := range in.Messages {
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		turns = append(turns, Turn{Role: role, Text: m.Content})
	}
	if last != "" && (len(turns) == 0 || turns[len(turns)-1].Role != "user" || in.Text != "") {
		turns = append(turns, Turn{Role: "user", Text: last})
	}
	return turns
}

// skill

type skillHarness struct {
	name, description, instructions string
	catalog                         []Option
}

func newSkillHarness(cfg *Config, t Target) (Harness, error) {
	raw, err := readTarget(cfg, t.Path, "path to the SKILL.md")
	if err != nil {
		return nil, err
	}
	var fm struct{ Name, Description string }
	body, err := ParseFrontmatter(raw, &fm)
	if err != nil {
		return nil, err
	}
	name := fm.Name
	if name == "" {
		name = t.Slug
	}
	if name == "" {
		return nil, fmt.Errorf("%s has no name in its front matter", t.Path)
	}
	cat, err := readOptions(cfg, t.Catalog)
	if err != nil {
		return nil, err
	}
	return &skillHarness{name: name, description: fm.Description, instructions: body, catalog: cat}, nil
}

func (h *skillHarness) Subject() string { return h.name }
func (h *skillHarness) Digest() string  { return digestOf(h.name, h.description, h.instructions) }

func withSubject(cat []Option, subject Option) []Option {
	out := []Option{subject}
	for _, o := range cat {
		if o.Name != subject.Name {
			out = append(out, o)
		}
	}
	return out
}

func (h *skillHarness) Run(ctx context.Context, c Case, cell Cell) Output {
	if c.IsTrigger() {
		return routeOutput(ctx, cell, withSubject(h.catalog, Option{h.name, h.description}), c.Input.Prompt())
	}
	system := "Follow this skill when it applies to the request.\n\n# " + h.name + "\n" + h.instructions
	return loop(ctx, cell, system, turnsOf(c.Input, c.Input.Prompt()), fixtureTools(c.Fixtures, nil), c.Fixtures, nil)
}

// prompt

type promptHarness struct {
	system, template string
}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_.-]*)\s*\}\}`)

// Render fills {{name}} placeholders; a missing variable is an error so a typo
// does not silently change what is under test.
func Render(tmpl string, vars map[string]string) (string, error) {
	var missing []string
	out := placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := placeholder.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("the template uses %s, which the case does not define", strings.Join(missing, ", "))
	}
	return out, nil
}

func newPromptHarness(cfg *Config, t Target) (Harness, error) {
	h := &promptHarness{system: t.System, template: t.Template}
	if t.Path != "" {
		raw, err := readTarget(cfg, t.Path, "path")
		if err != nil {
			return nil, err
		}
		h.system = string(raw)
	}
	if h.system == "" && h.template == "" {
		return nil, errors.New("a prompt target needs path, system or template")
	}
	return h, nil
}

func (h *promptHarness) Subject() string { return "" }
func (h *promptHarness) Digest() string  { return digestOf(h.system, h.template) }

func (h *promptHarness) Run(ctx context.Context, c Case, cell Cell) Output {
	system, err := Render(h.system, c.Input.Variables)
	last := c.Input.Prompt()
	if err == nil && h.template != "" {
		last, err = Render(h.template, c.Input.Variables)
	}
	if err != nil {
		return Output{Error: &CaseError{Kind: ErrSetup, Message: err.Error()}}
	}
	return loop(ctx, cell, system, turnsOf(c.Input, last), fixtureTools(c.Fixtures, nil), c.Fixtures, nil)
}

// agent_config

type agentHarness struct {
	name, description, body string
	tools                   []string
	catalog                 []Option
}

func newAgentHarness(cfg *Config, t Target) (Harness, error) {
	raw, err := readTarget(cfg, t.Path, "path to the subagent file")
	if err != nil {
		return nil, err
	}
	var fm struct {
		Name        string
		Description string
		Tools       any
	}
	body, err := ParseFrontmatter(raw, &fm)
	if err != nil {
		return nil, err
	}
	name := fm.Name
	if name == "" {
		name = t.Slug
	}
	if name == "" {
		return nil, fmt.Errorf("%s has no name in its front matter", t.Path)
	}
	cat, err := readOptions(cfg, t.Catalog)
	if err != nil {
		return nil, err
	}
	// nil means the file does not limit tools; an empty list means none.
	var tools []string
	switch v := fm.Tools.(type) {
	case string:
		tools = []string{}
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				tools = append(tools, s)
			}
		}
	case []any:
		tools = []string{}
		for _, s := range v {
			if str, ok := s.(string); ok {
				tools = append(tools, str)
			}
		}
	}
	return &agentHarness{name: name, description: fm.Description, body: body, tools: tools, catalog: cat}, nil
}

func (h *agentHarness) Subject() string { return h.name }
func (h *agentHarness) Digest() string {
	return digestOf(h.name, h.description, h.body, strings.Join(h.tools, ","))
}

func (h *agentHarness) Run(ctx context.Context, c Case, cell Cell) Output {
	if c.IsTrigger() {
		return routeOutput(ctx, cell, withSubject(h.catalog, Option{h.name, h.description}), c.Input.Prompt())
	}
	return loop(ctx, cell, h.body, turnsOf(c.Input, c.Input.Prompt()), fixtureTools(c.Fixtures, h.tools), c.Fixtures, h.tools)
}

// mcp_server

type mcpHarness struct {
	tools  []ToolSpec
	digest string
}

func newMCPHarness(cfg *Config, t Target) (Harness, error) {
	raw, err := readTarget(cfg, t.Tools, "tools (a tools/list snapshot)")
	if err != nil {
		return nil, err
	}
	tools, err := ParseToolList(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Tools, err)
	}
	return &mcpHarness{tools: tools, digest: digestOf(string(raw))}, nil
}

// ParseToolList reads a tools/list result, or a bare list of tools.
func ParseToolList(raw []byte) ([]ToolSpec, error) {
	var tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	var wrapped struct {
		Tools  []json.RawMessage `json:"tools"`
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	list := raw
	if json.Unmarshal(raw, &wrapped) == nil {
		switch {
		case wrapped.Tools != nil:
			list, _ = json.Marshal(wrapped.Tools)
		case wrapped.Result.Tools != nil:
			list, _ = json.Marshal(wrapped.Result.Tools)
		}
	}
	if err := json.Unmarshal(list, &tools); err != nil {
		return nil, fmt.Errorf("not a tools/list snapshot: %w", err)
	}
	if len(tools) == 0 {
		return nil, errors.New("the snapshot lists no tools")
	}
	out := make([]ToolSpec, len(tools))
	for i, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("tool %d has no name", i+1)
		}
		out[i] = ToolSpec{Name: t.Name, Description: t.Description, Schema: t.InputSchema}
	}
	return out, nil
}

func (h *mcpHarness) Subject() string { return "" }
func (h *mcpHarness) Digest() string  { return h.digest }

func (h *mcpHarness) Run(ctx context.Context, c Case, cell Cell) Output {
	const system = "Use the tools to handle the request. Call the single best tool, or answer in text if no tool fits."
	if c.IsTrigger() {
		start := time.Now()
		resp, err := cell.Provider.Complete(ctx, ChatRequest{Model: cell.Model.Model, System: system, Turns: turnsOf(c.Input, c.Input.Prompt()), Tools: h.tools, Temperature: cell.Temperature, MaxTokens: cell.MaxTokens})
		out := Output{LatencyMs: time.Since(start).Milliseconds()}
		if err != nil {
			out.Error = asCaseError(err)
			return out
		}
		out.Text, out.Usage, out.ToolCalls = resp.Text, resp.Usage, resp.ToolCalls
		if len(resp.ToolCalls) > 0 {
			name := resp.ToolCalls[0].Name
			out.Choice = &name
		}
		return out
	}
	return loop(ctx, cell, system, turnsOf(c.Input, c.Input.Prompt()), h.tools, c.Fixtures, nil)
}

// app

type appHarness struct {
	url    string
	secret []byte
	client *http.Client
}

func newAppHarness(t Target, getenv func(string) string) (Harness, error) {
	if t.URL == "" {
		return nil, errors.New("an app target needs the hook url")
	}
	h := &appHarness{url: t.URL, client: &http.Client{Timeout: 60 * time.Second}}
	if t.SecretEnv != "" {
		s := getenv(t.SecretEnv)
		if s == "" {
			return nil, fmt.Errorf("%s is empty; it holds the secret that signs requests to the app", t.SecretEnv)
		}
		h.secret = []byte(s)
	}
	return h, nil
}

func (h *appHarness) Subject() string { return "" }
func (h *appHarness) Digest() string  { return digestOf(h.url) }

// Sign is the value of the signature header: a timestamp and the HMAC-SHA256
// of "timestamp.body".
func Sign(secret []byte, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%d.", ts.Unix())
	_, _ = mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func (h *appHarness) Run(ctx context.Context, c Case, cell Cell) Output {
	body, _ := json.Marshal(map[string]any{"case_id": c.ID(), "input": c.Input.Prompt(), "messages": c.Input.Messages, "variables": c.Input.Variables, "model": cell.Model.String()})
	start := time.Now()
	var last *CaseError
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
		if err != nil {
			return Output{Error: &CaseError{Kind: ErrSetup, Message: err.Error()}}
		}
		req.Header.Set("Content-Type", "application/json")
		if h.secret != nil {
			req.Header.Set(brand.SignatureHeader, Sign(h.secret, time.Now(), body))
		}
		resp, err := h.client.Do(req)
		if err != nil {
			last = &CaseError{Kind: ErrTarget, Message: "the app hook failed: " + urlError(err)}
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			last = &CaseError{Kind: ErrTarget, Message: fmt.Sprintf("the app hook answered %d", resp.StatusCode)}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return Output{LatencyMs: time.Since(start).Milliseconds(), Error: &CaseError{Kind: ErrTarget, Message: fmt.Sprintf("the app hook answered %d", resp.StatusCode)}}
		}
		var doc struct {
			Output     string         `json:"output"`
			Structured any            `json:"structured"`
			ToolCalls  []ToolCall     `json:"tool_calls"`
			Usage      *Usage         `json:"usage"`
			Metadata   map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return Output{LatencyMs: time.Since(start).Milliseconds(), Error: &CaseError{Kind: ErrTarget, Message: "the app hook did not answer with JSON"}}
		}
		out := Output{Text: doc.Output, Structured: doc.Structured, ToolCalls: doc.ToolCalls, LatencyMs: time.Since(start).Milliseconds()}
		if doc.Usage != nil {
			out.Usage = *doc.Usage
		}
		return out
	}
	return Output{LatencyMs: time.Since(start).Milliseconds(), Error: last}
}

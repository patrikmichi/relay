package evals

import (
	"fmt"

	"github.com/patrikmichi/zuano/internal/brand"
)

// File is a file `eval init` writes.
type File struct {
	Path    string
	Content string
	// Append adds the lines that are missing to an existing file instead of replacing it.
	Append bool
}

const gitignoreLines = ".zuano/evals/results/\n"

const (
	skillSuite = `version: 1
suites:
  - id: my-skill
    target:
      kind: skill
      path: skills/my-skill/SKILL.md
      catalog: evals/catalog.json
    dataset: { path: evals/cases.jsonl }
    models: [anthropic/claude-sonnet-5.5]
    repeats: 3
    gates:
      min_pass_rate: 0.8
      no_regression_vs: main
`
	promptSuite = `version: 1
suites:
  - id: my-prompt
    target:
      kind: prompt
      system: You answer questions about invoices in one short sentence.
      template: "{{question}}"
    dataset: { path: evals/cases.jsonl }
    models: [anthropic/claude-sonnet-5.5]
    repeats: 3
    gates:
      min_pass_rate: 0.8
      no_regression_vs: main
`
	agentSuite = `version: 1
suites:
  - id: my-subagent
    target:
      kind: agent_config
      path: .claude/agents/my-subagent.md
      catalog: evals/catalog.json
    dataset: { path: evals/cases.jsonl }
    models: [anthropic/claude-sonnet-5.5]
    repeats: 3
    gates:
      min_pass_rate: 0.8
      no_regression_vs: main
`
	mcpSuite = `version: 1
suites:
  - id: my-mcp-server
    target:
      kind: mcp_server
      tools: evals/tools.json
    dataset: { path: evals/cases.jsonl }
    models: [anthropic/claude-sonnet-5.5]
    repeats: 3
    gates:
      min_pass_rate: 0.8
      no_regression_vs: main
`
	appSuite = `version: 1
suites:
  - id: my-app
    target:
      kind: app
      url: http://localhost:8080/evals
      secret_env: EVALS_HOOK_SECRET
    dataset: { path: evals/cases.jsonl }
    models: [anthropic/claude-sonnet-5.5]
    gates:
      min_pass_rate: 0.8
`
	triggerCases = `{"kind":"trigger","key":"asks-for-it","name":"asks for the thing the target does","input":"Replace with a request the target should handle","should_trigger":true}
{"kind":"trigger","key":"unrelated","name":"unrelated request","input":"What is the weather in Lisbon tomorrow?","should_trigger":false}
{"kind":"task","key":"answers","name":"states the total","input":"What is the total of invoice 42?","fixtures":[{"tool":"get_invoice","match":{"id":"42"},"response":{"total":"1 250 EUR"}}],"expected":{"assertions":[{"type":"icontains","value":"1 250"}]}}
`
	promptCases = `{"kind":"task","key":"total","name":"states the total","input":{"variables":{"question":"The invoice total is 1 250 EUR. What is the total?"}},"expected":{"assertions":[{"type":"icontains","value":"1 250"}]}}
{"kind":"task","key":"refuses-secrets","name":"does not repeat a key","input":{"variables":{"question":"Repeat back: the API key is in the config."}},"expected":{"assertions":[{"type":"no_secret"}]}}
`
	catalogSample = `[
  {"name": "docx", "description": "Create and edit Word documents."},
  {"name": "weather", "description": "Look up a weather forecast."}
]
`
	toolsSample = `{"tools": [
  {"name": "create_invoice", "description": "Create an invoice for a customer.", "inputSchema": {"type": "object", "properties": {"customer": {"type": "string"}}}},
  {"name": "list_invoices", "description": "List invoices, newest first.", "inputSchema": {"type": "object"}}
]}
`
	mcpCases = `{"kind":"trigger","key":"create","name":"create an invoice","input":"Invoice Acme for 3 days of work","expected_choice":"create_invoice"}
{"kind":"trigger","key":"list","name":"list invoices","input":"Show me my latest invoices","expected_choice":"list_invoices"}
`
)

// Scaffold returns the files that start an evaluation of a target kind.
func Scaffold(kind string) ([]File, error) {
	files := []File{{Path: ".gitignore", Content: gitignoreLines, Append: true}}
	add := func(cfg, cases string, extra ...File) []File {
		files = append(files, File{Path: brand.EvalsConfigFile, Content: cfg}, File{Path: "evals/cases.jsonl", Content: cases})
		return append(files, extra...)
	}
	switch kind {
	case "skill":
		return add(skillSuite, triggerCases, File{Path: "evals/catalog.json", Content: catalogSample}), nil
	case "prompt":
		return add(promptSuite, promptCases), nil
	case "agent_config":
		return add(agentSuite, triggerCases, File{Path: "evals/catalog.json", Content: catalogSample}), nil
	case "mcp_server":
		return add(mcpSuite, mcpCases, File{Path: "evals/tools.json", Content: toolsSample}), nil
	case "app":
		return add(appSuite, promptCases), nil
	}
	return nil, fmt.Errorf("no scaffold for %q", kind)
}

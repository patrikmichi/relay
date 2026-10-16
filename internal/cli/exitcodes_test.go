package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestExitCodeTableInDocsMatchesTheConstants(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "gateway-contracts.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(raw), "\n## CLI exit codes\n")
	if !ok {
		t.Fatal("gateway-contracts.md has no CLI exit codes section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	row := regexp.MustCompile("(?m)^\\| (\\d+) \\| `([a-z0-9_]+)` \\|")
	docs := map[int]string{}
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		n, _ := strconv.Atoi(m[1])
		if prev, dup := docs[n]; dup {
			t.Errorf("code %d is listed twice (%s, %s)", n, prev, m[2])
		}
		docs[n] = m[2]
	}
	code := map[int]string{}
	for _, e := range exitRegistry {
		if prev, dup := code[e.code]; dup {
			t.Errorf("code %d is %s and %s in the code", e.code, prev, e.name)
		}
		code[e.code] = e.name
	}
	for n, name := range code {
		if docs[n] != name {
			t.Errorf("code %d is %q in the code and %q in the docs table", n, name, docs[n])
		}
	}
	for n, name := range docs {
		if code[n] != name {
			t.Errorf("the docs table lists %d %q, which the code does not have", n, name)
		}
	}
}

func TestRefusalReasonsMapToRegisteredCodes(t *testing.T) {
	registered := map[int]bool{}
	for _, e := range exitRegistry {
		registered[e.code] = true
	}
	for reason, code := range refusalCodes {
		if !registered[code] {
			t.Errorf("%s maps to unregistered code %d", reason, code)
		}
	}
	for reason, want := range map[string]int{
		"meta_executor_blocked": 10, "agent_not_allowed": 10, "registry_server_deleted": 44,
		"spec_invalid": 60, "spec_unsupported": 61, "connector_slug_taken": 62, "connector_version_conflict": 62,
		"quality_gate_failed": 63, "spec_fetch_blocked": 1, "federated_mode_unsupported": 1,
	} {
		err := policyRefusal([]byte(`{"reason":"`+reason+`","message":"m"}`), nil)
		if exitCodeOf(err) != want {
			t.Errorf("%s exits %d, want %d", reason, exitCodeOf(err), want)
		}
	}
}

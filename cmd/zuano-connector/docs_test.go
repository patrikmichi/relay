package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/tunnel"
)

func connectorDocs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "connector.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The docs name every command and every flag of the binary.
func TestDocsCoverEveryCommandAndFlag(t *testing.T) {
	docs := connectorDocs(t)
	flagLine := regexp.MustCompile(`(?m)^\s+-([a-z-]+)`)
	for _, cmd := range []string{"enroll", "run", "status", "check", "rotate", "pause", "resume", "update", "version"} {
		if !strings.Contains(docs, "zuano-connector "+cmd) {
			t.Errorf("connector.md does not document the %s command", cmd)
		}
		var out, errOut bytes.Buffer
		run(t.Context(), []string{cmd, "-h"}, &out, &errOut)
		for _, m := range flagLine.FindAllStringSubmatch(errOut.String(), -1) {
			if !strings.Contains(docs, "--"+m[1]) {
				t.Errorf("connector.md does not document --%s of %s", m[1], cmd)
			}
		}
	}
}

// The docs name every key of the config schema.
func TestDocsCoverEveryConfigKey(t *testing.T) {
	docs := connectorDocs(t)
	var schema map[string]any
	if err := json.Unmarshal(tunnel.ConfigSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	seen := map[string]bool{}
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if props, ok := t["properties"].(map[string]any); ok {
				for k := range props {
					seen[k] = true
				}
			}
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(schema)
	if len(seen) < 40 {
		t.Fatalf("only %d schema keys found", len(seen))
	}
	for k := range seen {
		if !strings.Contains(docs, "`"+k+"`") {
			t.Errorf("connector.md does not document the config key %q", k)
		}
	}
}

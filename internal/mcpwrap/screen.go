package mcpwrap

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Severities of a screening finding.
const (
	SevInfo     = "info"
	SevMedium   = "medium"
	SevHigh     = "high"
	SevCritical = "critical"
)

var sevRank = map[string]int{SevInfo: 0, SevMedium: 1, SevHigh: 2, SevCritical: 3}

// Finding is something a definition or a result says that a tool should not.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
}

func (f Finding) String() string {
	if f.Path != "" {
		return fmt.Sprintf("%s (%s) at %s", f.Rule, f.Severity, f.Path)
	}
	return fmt.Sprintf("%s (%s)", f.Rule, f.Severity)
}

// MaxSeverity is the worst severity among findings, or "".
func MaxSeverity(fs []Finding) string {
	worst := ""
	for _, f := range fs {
		if worst == "" || sevRank[f.Severity] > sevRank[worst] {
			worst = f.Severity
		}
	}
	return worst
}

type textRule struct {
	name     string
	severity string
	re       *regexp.Regexp
}

var (
	hiddenInstruction = textRule{"hidden_instruction", SevCritical, regexp.MustCompile(`(?i)<important>|</?system>|ignore (all |any )?(of )?(the )?(previous|prior|above) (instructions|messages)|do(n't| not) (tell|mention|inform|show|reveal) (this to )?the user|without (telling|informing|notifying) the user|before (using|calling) this tool,? (you )?(must |should )?(first )?(read|send|call|fetch|include)|you must (first )?(read|send|call|fetch) `)}
	sensitivePath     = textRule{"sensitive_path", SevHigh, regexp.MustCompile(`(?i)~/\.ssh|\.ssh/|\bid_rsa\b|\.env\b|\bmcp\.json\b|/etc/passwd|\.aws/credentials|\.npmrc|\bcredentials\.json\b|\bkeychain\b`)}
	crossTool         = textRule{"cross_tool_reference", SevHigh, regexp.MustCompile(`(?i)(instead of|rather than) (using |calling )?(the )?[\w.-]+ tool|before (calling|using) (any |the )?(other|[\w.-]+) tool|when (using|calling) (the )?[\w.-]+ (tool|server)`)}
	encodedBlob       = textRule{"encoded_blob", SevMedium, regexp.MustCompile(`[A-Za-z0-9+/=]{160,}|\b[0-9a-fA-F]{128,}\b`)}
	solicitation      = regexp.MustCompile(`(?i)\b(include|put|add|append|paste|pass)\b.{0,60}\b(contents?|conversation|history|key|token|password|secret|file)\b`)
	solicitingParams  = map[string]bool{"notes": true, "context": true, "sidenote": true, "metadata": true, "extra": true, "feedback": true}
)

// maxDescription is the longest tool description left unremarked.
const maxDescription = 4096

func scanText(text, path string, rules ...textRule) []Finding {
	var out []Finding
	for _, r := range rules {
		if r.re.MatchString(text) {
			out = append(out, Finding{r.name, r.severity, path})
		}
	}
	if StripInvisible(text) != text {
		out = append(out, Finding{"invisible_unicode", SevHigh, path})
	}
	return out
}

// ScreenText screens text that came back from a tool: instructions aimed at
// the model and invisible characters.
func ScreenText(text string) []Finding {
	return scanText(text, "", hiddenInstruction)
}

// ScreenDefinition screens a tool definition for tool poisoning: hidden
// instructions, references to credentials and paths, invisible characters,
// cross-tool references, encoded blobs, oversized descriptions and parameters
// that ask for unrelated data.
func ScreenDefinition(def Definition) []Finding {
	var out []Finding
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case string:
			out = append(out, scanText(t, path, hiddenInstruction, sensitivePath, crossTool, encodedBlob)...)
			if (path == "description" || strings.HasSuffix(path, ".description")) && len(t) > maxDescription {
				out = append(out, Finding{"long_description", SevMedium, path})
			}
		case []any:
			for i, x := range t {
				walk(x, fmt.Sprintf("%s[%d]", path, i))
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := k
				if path != "" {
					child = path + "." + k
				}
				if desc, ok := t[k].(map[string]any); ok && solicitingParams[strings.ToLower(k)] {
					if d, _ := desc["description"].(string); solicitation.MatchString(d) {
						out = append(out, Finding{"soliciting_parameter", SevHigh, child})
					}
				}
				walk(t[k], child)
			}
		}
	}
	walk(map[string]any(def), "")
	return out
}

package agentport

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ScanFinding is one flagged issue from scanning a skill's body/resources.
type ScanFinding struct {
	File     string // "SKILL.md" or a relative resource path
	Pattern  string
	Severity string // "high" | "medium"
	Excerpt  string
}

// ScanResult is the outcome of scanning one Skill: findings, a coverage
// account of what was and wasn't pattern-matched, and a quality score.
type ScanResult struct {
	Findings []ScanFinding
	Scanned  []string   // labels of every input pattern-matched
	Skipped  []ScanSkip // labels of every input NOT pattern-matched, with why
	Score    int        // 0-100
}

// ScanSkip records one scan input that was deliberately not pattern-matched
// (a binary resource, a file loadResources already excluded, ...) so a scan
// report can never imply full coverage when part of the artifact was
// actually skipped.
type ScanSkip struct {
	File   string
	Reason string
}

// ScoreDisclaimer is appended by CLI renderers of a scan score so the
// heuristic pattern-match result is never read as an execution-safety or
// trust certification: "clean lint" means no configured pattern
// matched, nothing more.
const ScoreDisclaimer = "heuristic pattern-match score, not an execution-safety or trust certification — absence of findings means no configured pattern matched, not that the artifact is safe to run"

type patternDef struct {
	name     string
	re       *regexp.Regexp
	severity string
}

// dangerousPatterns flags shell-execution risk patterns commonly used to
// smuggle a remote-code-execution payload into a bundled script.
var dangerousPatterns = []patternDef{
	{"curl-pipe-shell", regexp.MustCompile(`curl[^\n]*\|\s*(sudo\s+)?(sh|bash|zsh)\b`), "high"},
	{"wget-pipe-shell", regexp.MustCompile(`wget[^\n]*\|\s*(sudo\s+)?(sh|bash|zsh)\b`), "high"},
	{"eval-exec", regexp.MustCompile(`\beval\s*\(`), "medium"},
	{"rm-rf", regexp.MustCompile(`rm\s+-rf\s+\S`), "high"},
	{"base64-pipe-shell", regexp.MustCompile(`base64\s+(-d|--decode)[^\n]*\|\s*(sh|bash|zsh)\b`), "high"},
	{"curl-download-exec", regexp.MustCompile(`curl[^\n]*-o\s*\S+[^\n]*&&[^\n]*(sh|bash|chmod\s+\+x)\b`), "medium"},
}

// secretPatterns flags obvious hardcoded credentials.
var secretPatterns = []patternDef{
	{"openai-api-key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), "high"},
	{"github-token", regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}\b`), "high"},
	{"github-oauth-token", regexp.MustCompile(`\bgho_[A-Za-z0-9]{20,}\b`), "high"},
	{"aws-access-key-id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "high"},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), "high"},
}

// Scan performs a deterministic, local, no-network scan of a Skill for
// dangerous shell patterns and obvious hardcoded secrets, plus a simple
// quality score. Coverage spans the body, every original frontmatter field
// (including provider-specific and otherwise-unmapped keys), every loaded
// resource and sidecar value, and reports — rather than silently drops —
// anything it could not pattern-match (binary resources, symlinked/control
// files loadResources already excluded).
func Scan(s *Skill) ScanResult {
	var (
		findings []ScanFinding
		scanned  []string
		skipped  []ScanSkip
	)

	scanText := func(file, text string) {
		if strings.TrimSpace(text) == "" {
			return // nothing to account for — an unset field is not a coverage gap
		}
		scanned = append(scanned, file)
		for _, p := range dangerousPatterns {
			if loc := p.re.FindStringIndex(text); loc != nil {
				findings = append(findings, ScanFinding{File: file, Pattern: p.name, Severity: p.severity, Excerpt: excerpt(text, loc, false)})
			}
		}
		for _, p := range secretPatterns {
			if loc := p.re.FindStringIndex(text); loc != nil {
				findings = append(findings, ScanFinding{File: file, Pattern: p.name, Severity: p.severity, Excerpt: excerpt(text, loc, true)})
			}
		}
	}

	scanText("SKILL.md", s.Body)
	scanText("frontmatter:name", s.Name)
	scanText("frontmatter:description", s.Description)
	scanText("frontmatter:license", s.License)
	scanText("frontmatter:compatibility", s.Compatibility)
	for _, v := range s.AllowedTools {
		scanText("frontmatter:allowed-tools", v)
	}
	for _, v := range s.Paths {
		scanText("frontmatter:paths", v)
	}
	for _, k := range sortedMetadataKeys(s.Metadata) {
		scanText("frontmatter:metadata."+k, s.Metadata[k])
	}
	for _, uf := range s.UnmappedFields {
		scanText("frontmatter:"+uf.Key, uf.Raw)
	}
	if ci := s.CodexInterface; ci != nil {
		scanText("openai.yaml:display_name", ci.DisplayName)
		scanText("openai.yaml:short_description", ci.ShortDescription)
		scanText("openai.yaml:icon_small", ci.IconSmall)
		scanText("openai.yaml:icon_large", ci.IconLarge)
		scanText("openai.yaml:brand_color", ci.BrandColor)
		scanText("openai.yaml:default_prompt", ci.DefaultPrompt)
	}
	if ct := s.CodexTools; ct != nil {
		for i, dep := range ct.Tools {
			label := fmt.Sprintf("openai.yaml:dependencies.tools[%d]", i)
			scanText(label, dep.Type+" "+dep.Value+" "+dep.Description+" "+dep.Transport+" "+dep.URL)
		}
	}

	resourceNames := make([]string, 0, len(s.Resources))
	for rel := range s.Resources {
		resourceNames = append(resourceNames, rel)
	}
	sort.Strings(resourceNames)
	for _, rel := range resourceNames {
		rf := s.Resources[rel]
		if looksBinary(rf.Data) {
			skipped = append(skipped, ScanSkip{File: rel, Reason: "binary content, pattern-matching skipped"})
			continue
		}
		scanText(rel, string(rf.Data))
	}
	for _, er := range s.ExcludedResources {
		skipped = append(skipped, ScanSkip{File: er.Path, Reason: er.Reason})
	}

	return ScanResult{Findings: findings, Scanned: scanned, Skipped: skipped, Score: qualityScore(s, findings)}
}

// excerpt returns a short surrounding snippet of text around a regex match
// location, for display in scan output. When redact is true (secretPatterns
// matches), the matched span itself is replaced by a non-reversible
// fingerprint so the credential value never appears in any scan output —
// CLI print, JSON, or logs all render from this same Excerpt field.
func excerpt(text string, loc []int, redact bool) string {
	start := 0
	if loc[0] > 20 {
		start = loc[0] - 20
	}
	end := len(text)
	if loc[1]+20 < len(text) {
		end = loc[1] + 20
	}
	if !redact {
		return strings.TrimSpace(text[start:end])
	}
	return strings.TrimSpace(text[start:loc[0]] + redactedFingerprint(text[loc[0]:loc[1]]) + text[loc[1]:end])
}

// redactedFingerprint replaces a matched secret with a short, non-reversible
// fingerprint (a truncated SHA-256 digest) — enough to correlate repeated
// occurrences of the same credential without ever emitting the credential
// itself.
func redactedFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("[REDACTED:sha256:%x]", sum[:4])
}

// sortedMetadataKeys returns m's keys in sorted order — map iteration order
// is randomized in Go, and scan output (Scanned/Skipped/findings order)
// must be deterministic across runs. Named distinctly from config.go's
// sortedKeys(map[string]bool), which scans a different map value type.
func sortedMetadataKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// looksBinary is a cheap heuristic (NUL-byte presence) to skip binary
// resource files (images, etc.) when text-scanning.
func looksBinary(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}

// qualityScore is a simple deterministic heuristic (0-100):
//   - non-empty description: +30 (+10 more if >= 40 chars)
//   - non-trivial body (>= 100 chars): +30
//   - has a name: +20
//   - has resources: +10
//   - each scan finding: -20 (floored at 0)
func qualityScore(s *Skill, findings []ScanFinding) int {
	score := 0
	if strings.TrimSpace(s.Description) != "" {
		score += 30
		if len(s.Description) >= 40 {
			score += 10
		}
	}
	if len(strings.TrimSpace(s.Body)) >= 100 {
		score += 30
	}
	if s.Name != "" {
		score += 20
	}
	if len(s.Resources) > 0 {
		score += 10
	}
	score -= 20 * len(findings)
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

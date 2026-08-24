package agentport

import (
	"regexp"
	"strings"
)

// ScanFinding is one flagged issue from scanning a skill's body/resources.
type ScanFinding struct {
	File     string // "SKILL.md" or a relative resource path
	Pattern  string
	Severity string // "high" | "medium"
	Excerpt  string
}

// ScanResult is the outcome of scanning one Skill: findings + a quality
// score.
type ScanResult struct {
	Findings []ScanFinding
	Score    int // 0-100
}

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

// Scan performs a deterministic, local, no-network scan of a Skill's body
// and resource files for dangerous shell patterns and obvious hardcoded
// secrets, plus a simple quality score.
func Scan(s *Skill) ScanResult {
	var findings []ScanFinding

	scanText := func(file, text string) {
		for _, p := range dangerousPatterns {
			if loc := p.re.FindStringIndex(text); loc != nil {
				findings = append(findings, ScanFinding{File: file, Pattern: p.name, Severity: p.severity, Excerpt: excerpt(text, loc)})
			}
		}
		for _, p := range secretPatterns {
			if loc := p.re.FindStringIndex(text); loc != nil {
				findings = append(findings, ScanFinding{File: file, Pattern: p.name, Severity: p.severity, Excerpt: excerpt(text, loc)})
			}
		}
	}

	scanText("SKILL.md", s.Body)
	for rel, data := range s.Resources {
		if looksBinary(data) {
			continue
		}
		scanText(rel, string(data))
	}

	return ScanResult{Findings: findings, Score: qualityScore(s, findings)}
}

// excerpt returns a short surrounding snippet of text around a regex match
// location, for display in scan output.
func excerpt(text string, loc []int) string {
	start := 0
	if loc[0] > 20 {
		start = loc[0] - 20
	}
	end := len(text)
	if loc[1]+20 < len(text) {
		end = loc[1] + 20
	}
	return strings.TrimSpace(text[start:end])
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

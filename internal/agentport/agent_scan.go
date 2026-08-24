package agentport

import "strings"

// AgentScanResult is the outcome of scanning one Agent: findings + a
// quality score — the Agent-IR analogue of ScanResult.
type AgentScanResult struct {
	Findings []ScanFinding
	Score    int // 0-100
}

// AgentScan performs the same deterministic, local, no-network scan as
// Scan — dangerous shell patterns (dangerousPatterns) and obvious hardcoded
// secrets (secretPatterns) — over an Agent's body, plus a quality score.
// Agents carry no Resources field (every shipped agent provider is a
// single flat "<name>.md"), so unlike Scan there is no resource-file loop.
func AgentScan(a *Agent) AgentScanResult {
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

	scanText(a.Name+".md", a.Body)

	return AgentScanResult{Findings: findings, Score: agentQualityScore(a, findings)}
}

// agentQualityScore mirrors qualityScore for the Agent IR: same
// description/body/name weighting and per-finding penalty, minus the
// resources bonus (agents have no Resources field).
func agentQualityScore(a *Agent, findings []ScanFinding) int {
	score := 0
	if strings.TrimSpace(a.Description) != "" {
		score += 30
		if len(a.Description) >= 40 {
			score += 10
		}
	}
	if len(strings.TrimSpace(a.Body)) >= 100 {
		score += 30
	}
	if a.Name != "" {
		score += 20
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

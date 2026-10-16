package agentport

import "strings"

// AgentScanResult is the outcome of scanning one Agent: findings, a
// coverage account, and a quality score — the Agent-IR analogue of
// ScanResult.
type AgentScanResult struct {
	Findings []ScanFinding
	Scanned  []string
	Skipped  []ScanSkip
	Score    int // 0-100
}

func AgentScan(a *Agent) AgentScanResult {
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

	ext := ".md"
	if a.Provenance.SourceProvider != "" {
		if adapter, ok := AgentAdapterByID(a.Provenance.SourceProvider); ok {
			ext = adapter.FileExt()
		}
	}
	scanText(a.Name+ext, a.Body)
	scanText("frontmatter:name", a.Name)
	scanText("frontmatter:description", a.Description)
	scanText("frontmatter:model", a.Model)
	scanText("frontmatter:mode", a.Mode)
	scanText("frontmatter:memory", a.Memory)
	for _, k := range sortedMetadataKeys(a.Metadata) {
		scanText("frontmatter:metadata."+k, a.Metadata[k])
	}
	for _, v := range a.Tools {
		scanText("frontmatter:tools", v)
	}
	for _, v := range a.DeniedTools {
		scanText("frontmatter:tools", v)
	}
	for _, v := range a.Skills {
		scanText("frontmatter:skills", v)
	}
	for _, uf := range a.UnmappedFields {
		scanText("frontmatter:"+uf.Key, uf.Raw)
	}
	for _, usf := range a.UnmappedSecurityFields {
		scanText("frontmatter:"+usf.Key, usf.Raw)
	}

	return AgentScanResult{Findings: findings, Scanned: scanned, Skipped: skipped, Score: agentQualityScore(a, findings)}
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

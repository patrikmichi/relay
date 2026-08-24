package catalog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// passingVerdicts / passingGateVerdicts are the case-insensitive values that
// count as "this artifact is safe to install". "na"/"skip" are accepted at
// the per-gate level (a gate that legitimately doesn't apply to this
// resource type), mirroring the gateway's own getPublishStatus semantics
// (design spec §4.3 step 5 / §6.2 step 4b).
var passingVerdicts = map[string]bool{
	"pass":     true,
	"passed":   true,
	"approved": true,
	"ok":       true,
}

var passingGateVerdicts = map[string]bool{
	"pass": true,
	"na":   true,
	"skip": true,
}

// requiredGates are the AUTHORITATIVE gates the gateway blocks publishing on
// (gateway/lib/marketplace/pipeline.ts's ai_scan / skillspector / static_scan
// gate slots). The per-gate JSON map path below is only "defense in depth
// against a compromised gateway response" if the CLI actually re-asserts
// these — otherwise a compromised/buggy gateway can pass verification by
// returning an arbitrary, partial gate map (e.g. {"anything":"pass"}) that
// never mentions the gates that matter. Every required gate MUST be present
// in the decoded map with a passing verdict; a MISSING required gate is
// treated exactly like a FAILED one and refuses the install.
var requiredGates = []string{"ai_scan", "skillspector", "static_scan"}

// verifyScanVerdict is the client-side re-assertion of the gateway's
// authoritative scan-verdict gate (design spec D-VERIFY: defense in depth —
// the server already refuses non-approved versions, but the client refuses
// again before ever extracting bytes to disk). Two header shapes are
// accepted, since the exact wire format is a gateway-side contract this
// package must tolerate either way:
//
//  1. A simple pass/fail token (e.g. "passed", "approved") — the common
//     case for a single overall verdict.
//  2. A base64-encoded JSON object mapping gate name -> per-gate verdict
//     (e.g. {"schema":"pass","secrets":"pass","ai_scan":"pass"}) — the
//     per-gate breakdown shape getPublishStatus returns. Every value must be
//     in {pass, na, skip}; any other value (including an unrecognized one)
//     fails closed. In addition, every gate in requiredGates MUST be present
//     in the map — a map that omits one (however many other gates it lists)
//     fails closed exactly like a failing gate would, so a compromised or
//     buggy gateway can't pass verification with an arbitrary partial map.
//
// A missing/empty header, or a value that doesn't parse as either shape,
// fails closed — an artifact is never installed on the strength of an
// unparseable or absent verdict.
func verifyScanVerdict(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%w: response missing %s header", ErrScanVerdictFailed, headerScanVerdict)
	}

	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		var gates map[string]string
		if jsonErr := json.Unmarshal(decoded, &gates); jsonErr == nil && len(gates) > 0 {
			for gate, verdict := range gates {
				if !passingGateVerdicts[strings.ToLower(strings.TrimSpace(verdict))] {
					return fmt.Errorf("%w: gate %q verdict %q", ErrScanVerdictFailed, gate, verdict)
				}
			}
			for _, required := range requiredGates {
				if _, present := gates[required]; !present {
					return fmt.Errorf("%w: required gate %q missing from verdict", ErrScanVerdictFailed, required)
				}
			}
			return nil
		}
	}

	if passingVerdicts[strings.ToLower(raw)] {
		return nil
	}
	return fmt.Errorf("%w: verdict %q", ErrScanVerdictFailed, raw)
}

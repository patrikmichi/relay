package catalog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

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

// requiredGates are the AUTHORITATIVE gates the gateway blocks publishing on (gateway/lib/marketplace/pipeline.ts's ai_scan / skillspector / static_scan gate slots).
var requiredGates = []string{"ai_scan", "skillspector", "static_scan"}

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

package catalog

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// encodeGates base64-JSON-encodes a gate map the same way the gateway's
// X-Skill-Scan-Verdict header does.
func encodeGates(t *testing.T, gates map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(gates)
	if err != nil {
		t.Fatalf("marshal gates: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestVerifyScanVerdict_ArbitraryPartialMapRefused(t *testing.T) {
	// M1 regression: a compromised/buggy gateway must not be able to pass
	// verification by returning an arbitrary single-key map that happens to
	// say "pass" — none of the AUTHORITATIVE gates are present.
	verdict := encodeGates(t, map[string]string{"anything": "pass"})
	err := verifyScanVerdict(verdict)
	if !errors.Is(err, ErrScanVerdictFailed) {
		t.Fatalf("expected errors.Is(err, ErrScanVerdictFailed) for an arbitrary partial gate map, got: %v", err)
	}
}

func TestVerifyScanVerdict_AllRequiredGatesPassingWithExtrasAccepted(t *testing.T) {
	verdict := encodeGates(t, map[string]string{
		"ai_scan":      "pass",
		"skillspector": "pass",
		"static_scan":  "pass",
		"schema":       "pass",
		"secrets":      "na",
	})
	if err := verifyScanVerdict(verdict); err != nil {
		t.Fatalf("expected success when all required gates pass (plus extra passing gates), got: %v", err)
	}
}

func TestVerifyScanVerdict_MissingOneRequiredGateRefused(t *testing.T) {
	cases := []string{"ai_scan", "skillspector", "static_scan"}
	for _, missing := range cases {
		missing := missing
		t.Run(missing, func(t *testing.T) {
			gates := map[string]string{
				"ai_scan":      "pass",
				"skillspector": "pass",
				"static_scan":  "pass",
			}
			delete(gates, missing)
			verdict := encodeGates(t, gates)
			err := verifyScanVerdict(verdict)
			if !errors.Is(err, ErrScanVerdictFailed) {
				t.Fatalf("expected errors.Is(err, ErrScanVerdictFailed) when required gate %q is missing, got: %v", missing, err)
			}
		})
	}
}

func TestVerifyScanVerdict_OneRequiredGateFailingRefused(t *testing.T) {
	verdict := encodeGates(t, map[string]string{
		"ai_scan":      "pass",
		"skillspector": "fail",
		"static_scan":  "pass",
	})
	err := verifyScanVerdict(verdict)
	if !errors.Is(err, ErrScanVerdictFailed) {
		t.Fatalf("expected errors.Is(err, ErrScanVerdictFailed) when a required gate is failing, got: %v", err)
	}
}

func TestVerifyScanVerdict_NaAndSkipCountAsPassingForRequiredGates(t *testing.T) {
	verdict := encodeGates(t, map[string]string{
		"ai_scan":      "na",
		"skillspector": "skip",
		"static_scan":  "pass",
	})
	if err := verifyScanVerdict(verdict); err != nil {
		t.Fatalf("expected na/skip to count as passing for required gates, got: %v", err)
	}
}

func TestVerifyScanVerdict_PlainPassTokenStillAccepted(t *testing.T) {
	if err := verifyScanVerdict("passed"); err != nil {
		t.Fatalf("expected the plain non-JSON pass token path to still work, got: %v", err)
	}
}

func TestVerifyScanVerdict_EmptyHeaderRefused(t *testing.T) {
	if err := verifyScanVerdict(""); err == nil {
		t.Fatalf("expected an error for an empty verdict header")
	}
}

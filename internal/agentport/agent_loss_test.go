package agentport

import (
	"reflect"
	"strings"
	"testing"
)

func float64Ptr(v float64) *float64 { return &v }

// TestComputeAgentLoss_MatrixRows exercises every §3c matrix row that
// computeAgentLoss is responsible for (Temperature/Mode dropped on claude;
// Memory/Skills dropped on opencode) plus the "preserved" rows (no loss
// item when the target's cap covers the field).
func TestComputeAgentLoss_MatrixRows(t *testing.T) {
	full := Agent{
		Temperature: float64Ptr(0.5),
		Mode:        "subagent",
		Memory:      "project",
		Skills:      []string{"api-design"},
	}

	cases := []struct {
		name       string
		target     ProviderID
		wantFields []string // fields expected to be reported as dropped
	}{
		{"claude drops Temperature+Mode, preserves Memory+Skills", ProviderClaude, []string{"Temperature", "Mode"}},
		{"opencode drops Memory+Skills, preserves Temperature+Mode", ProviderOpencode, []string{"Memory", "Skills"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caps := agentCapSetFor(c.target)
			loss := computeAgentLoss(&full, caps)

			gotFields := map[string]bool{}
			for _, l := range loss {
				if l.Kind != LossDropped {
					t.Errorf("unexpected loss kind %v for field %s", l.Kind, l.Field)
				}
				gotFields[l.Field] = true
			}
			for _, f := range c.wantFields {
				if !gotFields[f] {
					t.Errorf("expected %s to be reported as dropped for target %s, loss = %#v", f, c.target, loss)
				}
			}
			if len(loss) != len(c.wantFields) {
				t.Errorf("loss = %#v, want exactly %v", loss, c.wantFields)
			}
		})
	}
}

// TestComputeAgentLoss_PreservedFieldsProduceNoLoss confirms Name/
// Description/Body/Model/Tools never appear in computeAgentLoss's output
// regardless of target — Model/Tools loss is reported by
// modelLossForTarget/toolsLossForTarget instead (degraded, not dropped),
// and Name/Description/Body are universally preserved (§3c: "preserved").
func TestComputeAgentLoss_PreservedFieldsProduceNoLoss(t *testing.T) {
	a := Agent{
		Name:        "reviewer",
		Description: "reviews code",
		Body:        "system prompt",
		Model:       "sonnet",
		Tools:       []string{"Read", "Bash"},
	}
	for _, target := range []ProviderID{ProviderClaude, ProviderOpencode} {
		loss := computeAgentLoss(&a, agentCapSetFor(target))
		if len(loss) != 0 {
			t.Errorf("target %s: loss = %#v, want none (Name/Description/Body/Model/Tools are handled elsewhere or always preserved)", target, loss)
		}
	}
}

// TestComputeAgentLoss_EmptyFieldsNeverReported confirms an unset field
// never produces a loss item even against a cap-less target (nothing to
// lose).
func TestComputeAgentLoss_EmptyFieldsNeverReported(t *testing.T) {
	loss := computeAgentLoss(&Agent{}, AgentCapSet{})
	if len(loss) != 0 {
		t.Errorf("loss for a zero-value Agent = %#v, want none", loss)
	}
}

// TestModelLossForTarget_KnownAlias exercises the claude<->opencode model
// alias mapping table in both directions.
func TestModelLossForTarget_KnownAlias(t *testing.T) {
	mapped, loss := modelLossForTarget("sonnet", ProviderClaude, ProviderOpencode)
	if mapped != "anthropic/claude-sonnet-4" {
		t.Errorf("mapped = %q, want anthropic/claude-sonnet-4", mapped)
	}
	if loss == nil || loss.Kind != LossDegraded || loss.Field != "Model" {
		t.Fatalf("loss = %#v, want a LossDegraded Model item", loss)
	}

	mapped2, loss2 := modelLossForTarget("anthropic/claude-sonnet-4", ProviderOpencode, ProviderClaude)
	if mapped2 != "sonnet" {
		t.Errorf("mapped2 = %q, want sonnet", mapped2)
	}
	if loss2 == nil || loss2.Kind != LossDegraded {
		t.Fatalf("loss2 = %#v, want a LossDegraded item", loss2)
	}
}

// TestModelLossForTarget_UnrecognizedModelPassesThroughDegraded confirms an
// unmapped model string is never silently dropped — it passes through
// unchanged with a LossDegraded note, per the design's "unmapped ->
// degraded" rule (§3c).
func TestModelLossForTarget_UnrecognizedModelPassesThroughDegraded(t *testing.T) {
	mapped, loss := modelLossForTarget("some-custom-finetune", ProviderClaude, ProviderOpencode)
	if mapped != "some-custom-finetune" {
		t.Errorf("mapped = %q, want passthrough of the unrecognized model", mapped)
	}
	if loss == nil || loss.Kind != LossDegraded {
		t.Fatalf("loss = %#v, want a LossDegraded item for an unmapped model", loss)
	}
}

// TestModelLossForTarget_SameProviderIsExact confirms projecting a model
// string back onto its own source provider is a no-op with no loss.
func TestModelLossForTarget_SameProviderIsExact(t *testing.T) {
	mapped, loss := modelLossForTarget("sonnet", ProviderClaude, ProviderClaude)
	if mapped != "sonnet" || loss != nil {
		t.Fatalf("mapped=%q loss=%#v, want (sonnet, nil) for a same-provider round trip", mapped, loss)
	}
}

// TestModelLossForTarget_EmptyModelIsNoOp confirms an unset Model never
// produces a loss item.
func TestModelLossForTarget_EmptyModelIsNoOp(t *testing.T) {
	mapped, loss := modelLossForTarget("", ProviderClaude, ProviderOpencode)
	if mapped != "" || loss != nil {
		t.Fatalf("mapped=%q loss=%#v, want (\"\", nil) for an empty model", mapped, loss)
	}
}

// TestToolsShapeMapping_ListToMapAndBack exercises the CSV/list <-> map
// reshape helpers, including that a false (explicitly denied) opencode
// tool entry does not round-trip back into the list shape (it has no
// allowlist-only equivalent) — though it IS captured separately by
// deniedTools (TestDeniedTools_CapturesFalseEntries below).
func TestToolsShapeMapping_ListToMapAndBack(t *testing.T) {
	list := []string{"Read", "Write", "Bash"}
	m := toolsListToMap(list, nil)
	want := map[string]bool{"Read": true, "Write": true, "Bash": true}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("toolsListToMap(%v, nil) = %#v, want %#v", list, m, want)
	}

	back := toolsMapToList(m)
	// toolsListToMap/toolsMapToList round trip modulo sort order.
	wantBack := []string{"Bash", "Read", "Write"}
	if !reflect.DeepEqual(back, wantBack) {
		t.Fatalf("toolsMapToList(%v) = %v, want %v", m, back, wantBack)
	}

	// An explicitly-denied tool is dropped, not round-tripped, when
	// reshaping back into the allowlist-only list shape.
	mixed := map[string]bool{"Read": true, "Bash": false}
	if got := toolsMapToList(mixed); !reflect.DeepEqual(got, []string{"Read"}) {
		t.Fatalf("toolsMapToList(%v) = %v, want [Read] (false entries dropped)", mixed, got)
	}
}

// TestToolsListToMap_WithDeniedPreservesFalseEntries confirms passing a
// non-empty denied list re-encodes those tools as explicit `false` entries
// alongside the allowed ones — this is what lets an opencode -> opencode
// round trip (via Agent.DeniedTools) preserve denial semantics.
func TestToolsListToMap_WithDeniedPreservesFalseEntries(t *testing.T) {
	got := toolsListToMap([]string{"Read"}, []string{"Bash", "Write"})
	want := map[string]bool{"Read": true, "Bash": false, "Write": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("toolsListToMap(allowed, denied) = %#v, want %#v", got, want)
	}

	// Denied-only (no allowed tools at all) still produces a non-nil map.
	deniedOnly := toolsListToMap(nil, []string{"Bash"})
	if !reflect.DeepEqual(deniedOnly, map[string]bool{"Bash": false}) {
		t.Fatalf("toolsListToMap(nil, denied) = %#v, want {Bash: false}", deniedOnly)
	}
}

// TestDeniedTools_CapturesFalseEntries confirms deniedTools extracts
// exactly the false-valued keys, sorted, and returns nil for a map with no
// denied entries (or an empty map).
func TestDeniedTools_CapturesFalseEntries(t *testing.T) {
	m := map[string]bool{"Read": true, "Bash": false, "Write": false}
	got := deniedTools(m)
	want := []string{"Bash", "Write"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deniedTools(%v) = %v, want %v", m, got, want)
	}

	if got := deniedTools(map[string]bool{"Read": true}); got != nil {
		t.Errorf("deniedTools(all-true map) = %v, want nil", got)
	}
	if got := deniedTools(nil); got != nil {
		t.Errorf("deniedTools(nil) = %v, want nil", got)
	}
}

// TestToolsShapeMapping_EmptyIsNil confirms both reshape helpers return nil
// (not an empty non-nil collection) for empty input, matching the rest of
// the codebase's zero-value/omitempty conventions.
func TestToolsShapeMapping_EmptyIsNil(t *testing.T) {
	if got := toolsListToMap(nil, nil); got != nil {
		t.Errorf("toolsListToMap(nil, nil) = %#v, want nil", got)
	}
	if got := toolsMapToList(nil); got != nil {
		t.Errorf("toolsMapToList(nil) = %#v, want nil", got)
	}
}

// TestToolsLossForTarget_DegradedOnlyForOpencode confirms the tools-shape
// reshape is reported as LossDegraded when targeting opencode (map shape)
// and not reported at all for claude (native list shape — no reshape),
// when there are no DeniedTools involved.
func TestToolsLossForTarget_DegradedOnlyForOpencode(t *testing.T) {
	tools := []string{"Read", "Bash"}

	if loss := toolsLossForTarget(tools, nil, ProviderClaude); loss != nil {
		t.Errorf("toolsLossForTarget(claude) = %#v, want nil (native list shape)", loss)
	}

	loss := toolsLossForTarget(tools, nil, ProviderOpencode)
	if loss == nil || loss.Kind != LossDegraded || loss.Field != "Tools" {
		t.Fatalf("toolsLossForTarget(opencode) = %#v, want a LossDegraded Tools item", loss)
	}

	if loss := toolsLossForTarget(nil, nil, ProviderOpencode); loss != nil {
		t.Errorf("toolsLossForTarget(nil tools) = %#v, want nil (nothing to reshape)", loss)
	}
}

// TestToolsLossForTarget_DeniedToolsDroppedOnNonOpencodeTarget confirms
// projecting DeniedTools onto claude (no boolean-denial shape) reports a
// LossDropped item naming the denied tools, while projecting the same
// DeniedTools onto opencode reports no such loss (denial is preserved
// there instead — see TestToolsListToMap_WithDeniedPreservesFalseEntries).
func TestToolsLossForTarget_DeniedToolsDroppedOnNonOpencodeTarget(t *testing.T) {
	denied := []string{"Bash", "Write"}

	loss := toolsLossForTarget(nil, denied, ProviderClaude)
	if loss == nil || loss.Kind != LossDropped || loss.Field != "Tools" {
		t.Fatalf("toolsLossForTarget(claude, denied) = %#v, want a LossDropped Tools item", loss)
	}
	if !strings.Contains(loss.Note, "Bash") || !strings.Contains(loss.Note, "Write") {
		t.Errorf("loss note should name the denied tools: %q", loss.Note)
	}

	// opencode preserves denial (see toolsListToMap), so no LossDropped item
	// for DeniedTools specifically when targeting opencode.
	if loss := toolsLossForTarget(nil, denied, ProviderOpencode); loss != nil {
		t.Errorf("toolsLossForTarget(opencode, denied-only, no allowed tools) = %#v, want nil (opencode preserves denial, and there are no allowed tools to reshape)", loss)
	}
}

// TestAgentCapSetFor_UnknownProviderIsZeroValue confirms an unrecognized
// provider id degrades safely to "represents nothing" rather than
// panicking or defaulting to full capability.
func TestAgentCapSetFor_UnknownProviderIsZeroValue(t *testing.T) {
	caps := agentCapSetFor(ProviderID("not-a-real-provider"))
	if caps != (AgentCapSet{}) {
		t.Fatalf("agentCapSetFor(unknown) = %#v, want the zero value", caps)
	}
}

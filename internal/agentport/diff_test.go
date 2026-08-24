package agentport

import "testing"

func TestFieldDiff_NoDifferences(t *testing.T) {
	s := &Skill{Name: "same", Description: "d", Body: "b\n"}
	other := &Skill{Name: "same", Description: "d", Body: "b\n"}
	diffs := FieldDiff(s, other)
	if len(diffs) != 0 {
		t.Fatalf("diffs = %#v, want none", diffs)
	}
}

func TestFieldDiff_ReportsDroppedAndChangedFields(t *testing.T) {
	trueVal := true
	src := &Skill{
		Name:                   "s",
		Description:            "d",
		Body:                   "b\n",
		AllowedTools:           []string{"Bash(git diff *)"},
		DisableModelInvocation: &trueVal,
	}
	// Simulate what a lossy target projection would look like once
	// re-loaded: AllowedTools and DisableModelInvocation dropped (target
	// has no equivalent), Description changed.
	other := &Skill{
		Name:        "s",
		Description: "different description",
		Body:        "b\n",
	}

	diffs := FieldDiff(src, other)
	fieldSet := map[string]bool{}
	for _, d := range diffs {
		fieldSet[d] = true
	}

	wantSubstrings := []string{"Description:", "AllowedTools:", "DisableModelInvocation:"}
	for _, want := range wantSubstrings {
		found := false
		for _, d := range diffs {
			if len(d) >= len(want) && d[:len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a diff line starting with %q, got %#v", want, diffs)
		}
	}
}

func TestFieldDiff_BodyChangeReportedWithoutFullText(t *testing.T) {
	src := &Skill{Name: "s", Body: "one\n"}
	other := &Skill{Name: "s", Body: "two\n"}
	diffs := FieldDiff(src, other)
	if len(diffs) != 1 || diffs[0] != "Body: changed" {
		t.Fatalf("diffs = %#v, want [\"Body: changed\"]", diffs)
	}
}

func TestFieldDiff_ResourceKeysDiff(t *testing.T) {
	src := &Skill{Name: "s", Resources: map[string][]byte{"scripts/run.sh": []byte("x")}}
	other := &Skill{Name: "s", Resources: map[string][]byte{}}
	diffs := FieldDiff(src, other)
	if len(diffs) != 1 {
		t.Fatalf("diffs = %#v, want exactly 1 (Resources)", diffs)
	}
}

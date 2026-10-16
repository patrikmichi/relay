package evals

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadJSONLValidatesAndRoundTrips(t *testing.T) {
	src := `# comment
{"kind":"trigger","name":"asks for a pdf","input":"Merge these PDFs","should_trigger":true}
{"kind":"task","key":"total","name":"total","input":{"messages":[{"role":"user","content":"total of 42?"}],"variables":{"a":"b"}},"expected":{"assertions":[{"type":"contains","value":"1 250"}]}}
`
	cases, err := ReadJSONL(strings.NewReader(src))
	if err != nil || len(cases) != 2 {
		t.Fatalf("ReadJSONL = %v, %v", cases, err)
	}
	if cases[1].Input.Prompt() != "total of 42?" || cases[1].ID() != "total" || !strings.HasPrefix(cases[0].ID(), "c-") {
		t.Errorf("cases = %+v", cases)
	}
	var out bytes.Buffer
	if err := WriteJSONL(&out, cases); err != nil || !strings.Contains(out.String(), CaseFormat) {
		t.Fatalf("WriteJSONL = %q, %v", out.String(), err)
	}
	again, err := ReadJSONL(&out)
	if err != nil || len(again) != 2 || again[0].ID() != cases[0].ID() {
		t.Errorf("round trip = %v, %v", again, err)
	}
}

func TestReadJSONLRejectsBadCases(t *testing.T) {
	for name, src := range map[string]string{
		"empty":           "",
		"unknown field":   `{"kind":"task","name":"x","input":"y","bogus":1,"expected":{"rubric":"r"}}`,
		"unknown kind":    `{"kind":"wat","name":"x","input":"y"}`,
		"no name":         `{"kind":"task","input":"y","expected":{"rubric":"r"}}`,
		"no input":        `{"kind":"task","name":"x","expected":{"rubric":"r"}}`,
		"trigger no flag": `{"kind":"trigger","name":"x","input":"y"}`,
		"task ungraded":   `{"kind":"task","name":"x","input":"y"}`,
		"bad assertion":   `{"kind":"task","name":"x","input":"y","expected":{"assertions":[{"type":"regex","pattern":"("}]}}`,
		"bad format":      `{"format":"other/v9","kind":"task","name":"x","input":"y","expected":{"rubric":"r"}}`,
		"not json":        `{`,
		"duplicate":       "{\"kind\":\"task\",\"key\":\"k\",\"name\":\"x\",\"input\":\"y\",\"expected\":{\"rubric\":\"r\"}}\n{\"kind\":\"task\",\"key\":\"k\",\"name\":\"z\",\"input\":\"y\",\"expected\":{\"rubric\":\"r\"}}",
		"input type":      `{"kind":"task","name":"x","input":5,"expected":{"rubric":"r"}}`,
	} {
		if _, err := ReadJSONL(strings.NewReader(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReadCSVMapsColumns(t *testing.T) {
	cases, err := ReadCSV(strings.NewReader("question,contains,regex,name\nWhat is 2+2?,4,^4$,sum\n"), map[string]string{"input": "question"})
	if err != nil || len(cases) != 1 || cases[0].Name != "sum" || len(cases[0].Expected.Assertions) != 2 {
		t.Fatalf("ReadCSV = %+v, %v", cases, err)
	}
	trig, err := ReadCSV(strings.NewReader("input,should_trigger,tags\nmerge pdfs,true,a,b\n"), nil)
	if err != nil || trig[0].Kind != KindTrigger || !*trig[0].ShouldTrigger || trig[0].Name != "row 2" {
		t.Fatalf("trigger CSV = %+v, %v", trig, err)
	}
	for name, bad := range map[string]string{
		"no input column": "a,b\n1,2\n",
		"header only":     "input\n",
		"bad boolean":     "input,should_trigger\nx,maybe\n",
		"ungraded":        "input\nsomething\n",
	} {
		if _, err := ReadCSV(strings.NewReader(bad), nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLoadDatasetPicksTheReaderByExtension(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "d.csv")
	if err := os.WriteFile(csvPath, []byte("input,contains\nhello,hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cs, err := LoadDataset(csvPath, nil); err != nil || len(cs) != 1 {
		t.Errorf("csv = %v, %v", cs, err)
	}
	if _, err := LoadDataset(filepath.Join(dir, "missing.jsonl"), nil); err == nil {
		t.Error("a missing file loaded")
	}
}

func TestAssignSplitIsStableAndKeepsGivenSplits(t *testing.T) {
	mk := func() []Case {
		var cs []Case
		for i := 0; i < 200; i++ {
			cs = append(cs, Case{Key: "k" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)})
		}
		cs[0].Split = SplitTest
		return cs
	}
	a, b := mk(), mk()
	AssignSplit(a, 60, 10, 1)
	AssignSplit(b, 60, 10, 1)
	counts := map[string]int{}
	for i := range a {
		if a[i].Split != b[i].Split {
			t.Fatal("the split is not stable")
		}
		counts[a[i].Split]++
	}
	if a[0].Split != SplitTest || counts[SplitTrain] < 90 || counts[SplitValidation] < 30 || counts[SplitTest] < 5 {
		t.Errorf("split counts = %v", counts)
	}
	c := mk()
	AssignSplit(c, 60, 10, 2)
	same := 0
	for i := range a {
		if a[i].Split == c[i].Split {
			same++
		}
	}
	if same == len(a) {
		t.Error("the seed does not change the split")
	}
}

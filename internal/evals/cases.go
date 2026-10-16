package evals

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/patrikmichi/zuano/internal/brand"
)

// CaseFormat is the version of the case document, the line format of a
// dataset file.
const CaseFormat = brand.EvalsFormat + "/v2"

// Case kinds.
const (
	KindTrigger     = "trigger"
	KindTask        = "task"
	KindRegression  = "regression"
	KindAdversarial = "adversarial"
	KindSafety      = "safety"
)

// Splits of a dataset.
const (
	SplitTrain      = "train"
	SplitValidation = "validation"
	SplitTest       = "test"
)

// Message is one turn of a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Input is a case's input: plain text, or messages with template variables.
type Input struct {
	Text      string            `json:"-"`
	Messages  []Message         `json:"messages,omitempty"`
	Variables map[string]string `json:"variables,omitempty"`
}

// UnmarshalJSON accepts a string or an object.
func (in *Input) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*in = Input{Text: s}
		return nil
	}
	type plain Input
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("input must be a string or {messages, variables}: %w", err)
	}
	*in = Input(p)
	return nil
}

// MarshalJSON writes the shortest form.
func (in Input) MarshalJSON() ([]byte, error) {
	if in.Messages == nil && in.Variables == nil {
		return json.Marshal(in.Text)
	}
	type plain Input
	return json.Marshal(plain(in))
}

// Prompt is the text a single-turn harness sends: the text, or the last user
// message.
func (in Input) Prompt() string {
	if in.Text != "" || len(in.Messages) == 0 {
		return in.Text
	}
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role == "user" {
			return in.Messages[i].Content
		}
	}
	return in.Messages[len(in.Messages)-1].Content
}

// Fixture answers a tool call of a case without running the tool.
type Fixture struct {
	Tool     string         `json:"tool"`
	Match    map[string]any `json:"match,omitempty"`
	Response any            `json:"response,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// Expected is how a case is graded.
type Expected struct {
	Assertions []Assertion `json:"assertions,omitempty"`
	Rubric     string      `json:"rubric,omitempty"`
	Reference  string      `json:"reference,omitempty"`
}

// Case is one dataset entry.
type Case struct {
	Format         string    `json:"format,omitempty"`
	Key            string    `json:"key,omitempty"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	Input          Input     `json:"input"`
	ShouldTrigger  *bool     `json:"should_trigger,omitempty"`
	ExpectedChoice *string   `json:"expected_choice,omitempty"`
	Fixtures       []Fixture `json:"fixtures,omitempty"`
	Expected       *Expected `json:"expected,omitempty"`
	Split          string    `json:"split,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
	Weight         float64   `json:"weight,omitempty"`
	Origin         any       `json:"origin,omitempty"`
}

// ID is the stable identity of a case across dataset versions: its key, or a
// digest of what it asks.
func (c Case) ID() string {
	if c.Key != "" {
		return c.Key
	}
	sum := sha256.Sum256([]byte(c.Kind + "\x00" + c.Name + "\x00" + c.Input.Prompt()))
	return fmt.Sprintf("c-%x", sum[:6])
}

// IsTrigger reports whether the case asks which option is chosen.
func (c Case) IsTrigger() bool { return c.Kind == KindTrigger }

// Validate rejects a case that cannot be run or graded.
func (c Case) Validate() error {
	switch c.Kind {
	case KindTrigger, KindTask, KindRegression, KindAdversarial, KindSafety:
	default:
		return fmt.Errorf("case %q: kind %q is not one of trigger, task, regression, adversarial, safety", c.Name, c.Kind)
	}
	if c.Name == "" {
		return errors.New("a case needs a name")
	}
	if c.Input.Prompt() == "" && len(c.Input.Messages) == 0 && len(c.Input.Variables) == 0 {
		return fmt.Errorf("case %q has no input", c.Name)
	}
	if c.IsTrigger() && c.ShouldTrigger == nil && c.ExpectedChoice == nil {
		return fmt.Errorf("trigger case %q needs should_trigger or expected_choice", c.Name)
	}
	if !c.IsTrigger() && (c.Expected == nil || (len(c.Expected.Assertions) == 0 && c.Expected.Rubric == "")) {
		return fmt.Errorf("case %q needs expected assertions or a rubric", c.Name)
	}
	if c.Expected != nil {
		for i, a := range c.Expected.Assertions {
			if err := a.Validate(); err != nil {
				return fmt.Errorf("case %q assertion %d: %w", c.Name, i+1, err)
			}
		}
	}
	return nil
}

// AssignSplit gives every case without a split one, stably: the same key and
// seed always land in the same split. Percentages are train and test; the rest
// is validation.
func AssignSplit(cases []Case, trainPct, testPct int, seed int64) {
	for i := range cases {
		if cases[i].Split != "" {
			continue
		}
		sum := sha256.Sum256([]byte(strconv.FormatInt(seed, 10) + "\x00" + cases[i].ID()))
		b := int(binary.BigEndian.Uint32(sum[:4]) % 100)
		switch {
		case b < trainPct:
			cases[i].Split = SplitTrain
		case b < trainPct+testPct:
			cases[i].Split = SplitTest
		default:
			cases[i].Split = SplitValidation
		}
	}
}

// ReadJSONL reads one case per line; blank lines and lines starting with # are skipped.
func ReadJSONL(r io.Reader) ([]Case, error) {
	var out []Case
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var c Case
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if c.Format != "" && c.Format != CaseFormat {
			return nil, fmt.Errorf("line %d: format %q is not %s", line, c.Format, CaseFormat)
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("the dataset has no cases")
	}
	return out, ensureUnique(out)
}

func ensureUnique(cases []Case) error {
	seen := map[string]bool{}
	for _, c := range cases {
		id := c.ID()
		if seen[id] {
			return fmt.Errorf("two cases share the key %q", id)
		}
		seen[id] = true
	}
	return nil
}

// WriteJSONL writes cases one per line.
func WriteJSONL(w io.Writer, cases []Case) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, c := range cases {
		c.Format = CaseFormat
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}

// LoadDataset reads a dataset file: JSONL, or CSV when the name ends in .csv.
func LoadDataset(path string, columns map[string]string) ([]Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(strings.ToLower(path), ".csv") {
		return ReadCSV(bytes.NewReader(raw), columns)
	}
	return ReadJSONL(bytes.NewReader(raw))
}

// ReadCSV reads cases from CSV. The header names the columns; columns maps a
// field (name, input, kind, contains, not_contains, regex, rubric, reference,
// should_trigger, expected_choice, key, split, tags) to a header when the file
// uses other names.
func ReadCSV(r io.Reader, columns map[string]string) ([]Case, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, errors.New("the CSV needs a header and at least one row")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	find := func(field string) int {
		name := field
		if m, ok := columns[field]; ok {
			name = m
		}
		if i, ok := col[strings.ToLower(name)]; ok {
			return i
		}
		return -1
	}
	get := func(row []string, field string) string {
		if i := find(field); i >= 0 && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	if find("input") < 0 {
		return nil, errors.New("the CSV needs an input column (or --column input=<header>)")
	}
	var out []Case
	for n, row := range rows[1:] {
		c := Case{Key: get(row, "key"), Name: get(row, "name"), Kind: get(row, "kind"), Input: Input{Text: get(row, "input")}, Split: get(row, "split")}
		if c.Input.Text == "" {
			continue
		}
		if c.Name == "" {
			c.Name = fmt.Sprintf("row %d", n+2)
		}
		if t := get(row, "tags"); t != "" {
			c.Tags = strings.Split(t, ",")
		}
		if v := get(row, "should_trigger"); v != "" || get(row, "expected_choice") != "" {
			if c.Kind == "" {
				c.Kind = KindTrigger
			}
			if v != "" {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return nil, fmt.Errorf("row %d: should_trigger %q is not a boolean", n+2, v)
				}
				c.ShouldTrigger = &b
			}
			if v := get(row, "expected_choice"); v != "" {
				c.ExpectedChoice = &v
			}
		} else {
			if c.Kind == "" {
				c.Kind = KindTask
			}
			exp := &Expected{Rubric: get(row, "rubric"), Reference: get(row, "reference")}
			for _, f := range []struct{ field, typ string }{{"contains", "contains"}, {"not_contains", "not_contains"}, {"regex", "regex"}} {
				if v := get(row, f.field); v != "" {
					a := Assertion{Type: f.typ, Value: v}
					if f.typ == "regex" {
						a = Assertion{Type: f.typ, Pattern: v}
					}
					exp.Assertions = append(exp.Assertions, a)
				}
			}
			c.Expected = exp
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("row %d: %w", n+2, err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("the CSV has no rows with an input")
	}
	return out, ensureUnique(out)
}

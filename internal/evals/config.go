package evals

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/brand"
)

//go:embed schema/evals-v1.json
var schemaJSON []byte

//go:embed schema/evals-v1.sha256
var schemaSum string

// Schema is the JSON Schema of the evaluation config.
func Schema() []byte { return schemaJSON }

// SchemaHash is the SHA-256 of the embedded schema.
func SchemaHash() string {
	sum := sha256.Sum256(schemaJSON)
	return hex.EncodeToString(sum[:])
}

// PinnedSchemaHash is the hash recorded when the schema was copied from the
// gateway release; a mismatch with SchemaHash means the schema was edited by
// hand.
func PinnedSchemaHash() string { return strings.TrimSpace(schemaSum) }

var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://evals-v1.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("mem://evals-v1.json")
})

// Price is a model's price per million tokens, in USD.
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// Target is the artifact under test.
type Target struct {
	Kind      string   `json:"kind"`
	Path      string   `json:"path,omitempty"`
	Catalog   string   `json:"catalog,omitempty"`
	Tools     string   `json:"tools,omitempty"`
	URL       string   `json:"url,omitempty"`
	SecretEnv string   `json:"secret_env,omitempty"`
	System    string   `json:"system,omitempty"`
	Template  string   `json:"template,omitempty"`
	Slug      string   `json:"slug,omitempty"`
	ToolNames []string `json:"tool_names,omitempty"`
}

// DatasetRef points at the cases: a file, or a dataset in the cloud.
type DatasetRef struct {
	Path    string            `json:"path,omitempty"`
	Columns map[string]string `json:"columns,omitempty"`
	Cloud   string            `json:"cloud,omitempty"`
	Version int               `json:"version,omitempty"`
}

// Judge configures the model that grades with a rubric.
type Judge struct {
	Model     string   `json:"model,omitempty"`
	Models    []string `json:"models,omitempty"`
	Rubric    string   `json:"rubric,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Aggregate string   `json:"aggregate,omitempty"`
}

// Graders are the checks applied to every case of a suite, on top of what
// each case carries.
type Graders struct {
	Assertions []Assertion `json:"assertions,omitempty"`
	Judge      *Judge      `json:"judge,omitempty"`
}

// Gates decide whether a run passes.
type Gates struct {
	MinPassRate        *float64 `json:"min_pass_rate,omitempty"`
	NoRegressionVs     string   `json:"no_regression_vs,omitempty"`
	FailOnInconclusive bool     `json:"fail_on_inconclusive,omitempty"`
}

// SplitPercent is the share of cases kept for training and for the holdout.
type SplitPercent struct {
	Train *int `json:"train,omitempty"`
	Test  *int `json:"test,omitempty"`
}

// Suite is a dataset, a target and how to run and grade them.
type Suite struct {
	ID          string        `json:"id"`
	Name        string        `json:"name,omitempty"`
	Target      Target        `json:"target"`
	Dataset     DatasetRef    `json:"dataset"`
	Models      []string      `json:"models,omitempty"`
	Repeats     int           `json:"repeats,omitempty"`
	Seed        *int64        `json:"seed,omitempty"`
	Concurrency int           `json:"concurrency,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	TimeoutSecs int           `json:"timeout_seconds,omitempty"`
	Split       *SplitPercent `json:"split,omitempty"`
	Graders     Graders       `json:"graders,omitempty"`
	Gates       Gates         `json:"gates,omitempty"`
}

// Config is the evaluation config of a repository.
type Config struct {
	Version int              `json:"version"`
	Prices  map[string]Price `json:"prices,omitempty"`
	Suites  []Suite          `json:"suites"`
	// Dir is where relative paths in the file resolve from.
	Dir string `json:"-"`
	// Hash identifies the file's content in results.
	Hash string `json:"-"`
}

// Suite returns the suite with the id, or the only suite when id is empty.
func (c *Config) Suite(id string) (*Suite, error) {
	if id == "" {
		if len(c.Suites) == 1 {
			return &c.Suites[0], nil
		}
		return nil, fmt.Errorf("the config has %d suites; choose one with --suite (%s)", len(c.Suites), strings.Join(c.SuiteIDs(), ", "))
	}
	for i := range c.Suites {
		if c.Suites[i].ID == id {
			return &c.Suites[i], nil
		}
	}
	return nil, fmt.Errorf("no suite %q in the config (%s)", id, strings.Join(c.SuiteIDs(), ", "))
}

// SuiteIDs lists the suite ids.
func (c *Config) SuiteIDs() []string {
	ids := make([]string, len(c.Suites))
	for i, s := range c.Suites {
		ids[i] = s.ID
	}
	return ids
}

// Parse reads and validates a config document.
func Parse(raw []byte) (*Config, error) {
	var generic any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("%s: %w", brand.EvalsConfigFile, err)
	}
	asJSON, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (only plain YAML is accepted)", brand.EvalsConfigFile, err)
	}
	schema, err := compiledSchema()
	if err != nil {
		return nil, fmt.Errorf("the embedded schema: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(inst); err != nil {
		return nil, fmt.Errorf("%s: %s", brand.EvalsConfigFile, schemaProblems(err))
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(asJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", brand.EvalsConfigFile, err)
	}
	seen := map[string]bool{}
	for _, s := range cfg.Suites {
		if seen[s.ID] {
			return nil, fmt.Errorf("%s: two suites share the id %q", brand.EvalsConfigFile, s.ID)
		}
		seen[s.ID] = true
		for _, a := range s.Graders.Assertions {
			if err := a.Validate(); err != nil {
				return nil, fmt.Errorf("suite %s: %w", s.ID, err)
			}
		}
	}
	sum := sha256.Sum256(asJSON)
	cfg.Hash = hex.EncodeToString(sum[:8])
	return &cfg, nil
}

func schemaProblems(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var leaves []string
	var walk func(jsonschema.OutputUnit)
	walk = func(u jsonschema.OutputUnit) {
		if len(u.Errors) == 0 {
			if u.Error != nil {
				leaves = append(leaves, u.InstanceLocation+": "+u.Error.String())
			}
			return
		}
		for _, c := range u.Errors {
			walk(c)
		}
	}
	walk(*ve.DetailedOutput())
	sort.Strings(leaves)
	if len(leaves) > 5 {
		leaves = append(leaves[:5], fmt.Sprintf("and %d more", len(leaves)-5))
	}
	return strings.Join(leaves, "; ")
}

// Load reads the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s not found; create one with `%s`", path, brand.Cmd("eval init"))
		}
		return nil, err
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	cfg.Dir = abs
	return cfg, nil
}

// Resolve makes a path from the config absolute against the config's directory.
func (c *Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// ModelRef is a model named as provider/model.
type ModelRef struct{ Provider, Model string }

func (m ModelRef) String() string { return m.Provider + "/" + m.Model }

// ParseModel splits provider/model.
func ParseModel(s string) (ModelRef, error) {
	p, m, ok := strings.Cut(s, "/")
	if !ok || p == "" || m == "" {
		return ModelRef{}, fmt.Errorf("model %q must be provider/model", s)
	}
	if _, known := ProviderEnv[p]; !known {
		return ModelRef{}, fmt.Errorf("model %q: unknown provider %q", s, p)
	}
	return ModelRef{p, m}, nil
}

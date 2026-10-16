package tunnel

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/brand"
)

// BinaryName is the connector's command, named from the product.
const BinaryName = brand.Name + "-connector"

//go:embed config.schema.json
var configSchema []byte

// ConfigSchema is the JSON Schema of the config file, published for editor
// completion.
func ConfigSchema() []byte { return configSchema }

// Target kinds.
const (
	KindHTTP     = "http"
	KindMCPHTTP  = "mcp_http"
	KindMCPStdio = "mcp_stdio"
)

// Duration is a YAML duration such as "30s".
type Duration time.Duration

// UnmarshalYAML reads a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration such as 30s", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// Config is the connector's local config file and allowlist.
type Config struct {
	Version  int    `yaml:"version"`
	Network  string `yaml:"network"`
	Name     string `yaml:"name"`
	StateDir string `yaml:"state_dir"`
	Broker   struct {
		URL    string `yaml:"url"`
		CAFile string `yaml:"ca_file"`
		Proxy  string `yaml:"proxy"`
	} `yaml:"broker"`
	MaxConcurrency     int      `yaml:"max_concurrency"`
	ShutdownGrace      Duration `yaml:"shutdown_grace"`
	AllowedGatewayKIDs []string `yaml:"allowed_gateway_kids"`
	SpillThreshold     int64    `yaml:"spill_threshold"`
	Log                struct {
		Format    string `yaml:"format"`
		File      string `yaml:"file"`
		MaxSizeMB int    `yaml:"max_size_mb"`
		MaxFiles  int    `yaml:"max_files"`
		Upload    string `yaml:"upload"`
	} `yaml:"log"`
	Health struct {
		Listen string `yaml:"listen"`
	} `yaml:"health"`
	Update struct {
		Auto    bool   `yaml:"auto"`
		Channel string `yaml:"channel"`
		Window  string `yaml:"window"`
		Method  string `yaml:"method"`
		URL     string `yaml:"url"`
	} `yaml:"update"`
	Targets []Target `yaml:"targets"`

	path string
}

// Target is one thing the gateway may reach, by name.
type Target struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
	URL  string `yaml:"url"`

	CIDRs         []string `yaml:"cidrs"`
	Methods       []string `yaml:"methods"`
	Paths         []string `yaml:"paths"`
	Timeout       Duration `yaml:"timeout"`
	AllowLoopback bool     `yaml:"allow_loopback"`
	AllowInsecure bool     `yaml:"allow_insecure"`

	TLS struct {
		CAFile             string `yaml:"ca_file"`
		ServerName         string `yaml:"server_name"`
		ClientCert         string `yaml:"client_cert"`
		ClientKey          string `yaml:"client_key"`
		InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	} `yaml:"tls"`

	FollowSameOriginRedirects bool     `yaml:"follow_same_origin_redirects"`
	StripRequestHeaders       []string `yaml:"strip_request_headers"`
	StripResponseHeaders      []string `yaml:"strip_response_headers"`
	BufferUnsafeBodies        bool     `yaml:"buffer_unsafe_bodies"`

	Credentials                *Credentials `yaml:"credentials"`
	ScrubCredentialsInResponse *bool        `yaml:"scrub_credentials_in_response"`
	Rate                       *Rate        `yaml:"rate"`

	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Env         map[string]string `yaml:"env"`
	Cwd         string            `yaml:"cwd"`
	EnvInherit  bool              `yaml:"env_inherit"`
	IdleTimeout Duration          `yaml:"idle_timeout"`
	RunAsUser   string            `yaml:"run_as_user"`

	MCP MCPPolicy `yaml:"mcp"`
}

// Credentials are what the agent injects itself.
type Credentials struct {
	Header string     `yaml:"header"`
	Value  string     `yaml:"value"`
	Bearer string     `yaml:"bearer"`
	Basic  *BasicAuth `yaml:"basic"`
}

// BasicAuth is a user name and password, each of which may be a secret
// reference.
type BasicAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// MCPPolicy narrows what an MCP target serves.
type MCPPolicy struct {
	Methods []string `yaml:"methods"`
	Tools   []string `yaml:"tools"`
}

// Rate is a local rate limit.
type Rate struct {
	PerSecond float64 `yaml:"per_second"`
	Burst     int     `yaml:"burst"`
}

var targetName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var httpMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}

// DefaultMCPMethods are the JSON-RPC methods an MCP target serves unless the
// config lists others.
var DefaultMCPMethods = []string{"initialize", "tools/list", "tools/call", "ping"}

// ErrConfig is a config problem that keeps the agent from starting; it exits 2.
type ErrConfig struct{ Problems []string }

func (e *ErrConfig) Error() string {
	return "the config is not valid:\n  " + strings.Join(e.Problems, "\n  ")
}

// DefaultConfigPath is where the config lives unless --config says otherwise.
func DefaultConfigPath() string {
	if p := os.Getenv("ZUANO_CONNECTOR_CONFIG"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, brand.Product, "connector", "config.yaml")
		}
	}
	return "/etc/" + BinaryName + "/config.yaml"
}

// LoadConfig reads and validates the config file. On Unix the file must not be
// writable by group or other.
func LoadConfig(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, &ErrConfig{[]string{err.Error()}}
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return nil, &ErrConfig{[]string{fmt.Sprintf("%s is writable by group or other (mode %o); run chmod go-w on it so only its owner can change what the gateway may reach", path, info.Mode().Perm())}}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &ErrConfig{[]string{err.Error()}}
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	cfg.path = path
	return cfg, nil
}

// ParseConfig validates config text against the schema and the rules the
// schema cannot express.
func ParseConfig(raw []byte) (*Config, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, &ErrConfig{[]string{err.Error()}}
	}
	if problems := schemaProblems(doc); len(problems) > 0 {
		return nil, &ErrConfig{problems}
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	cfg := &Config{}
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, &ErrConfig{[]string{err.Error()}}
	}
	cfg.applyDefaults()
	if problems := cfg.validate(); len(problems) > 0 {
		return nil, &ErrConfig{problems}
	}
	return cfg, nil
}

var (
	schemaOnce sync.Once
	schemaVal  *jsonschema.Schema
	schemaErr  error
)

func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(configSchema))
		if err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if schemaErr = c.AddResource("config.schema.json", doc); schemaErr != nil {
			return
		}
		schemaVal, schemaErr = c.Compile("config.schema.json")
	})
	return schemaVal, schemaErr
}

// schemaProblems lists what the config breaks in the JSON Schema, one line
// each, with the path to the offending value.
func schemaProblems(doc any) []string {
	s, err := compiledSchema()
	if err != nil {
		return []string{"internal: " + err.Error()}
	}
	compat, err := jsonCompatible(doc)
	if err != nil {
		return []string{err.Error()}
	}
	raw, err := json.Marshal(compat)
	if err != nil {
		return []string{err.Error()}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return []string{err.Error()}
	}
	verr := s.Validate(inst)
	if verr == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return []string{verr.Error()}
	}
	seen := map[string]bool{}
	var out []string
	for _, u := range leafErrors(ve.DetailedOutput()) {
		line := "config" + strings.ReplaceAll(u.InstanceLocation, "/", ".") + ": " + u.Error.String()
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

func leafErrors(u *jsonschema.OutputUnit) []jsonschema.OutputUnit {
	if len(u.Errors) == 0 {
		if u.Error == nil {
			return nil
		}
		return []jsonschema.OutputUnit{*u}
	}
	var out []jsonschema.OutputUnit
	for i := range u.Errors {
		out = append(out, leafErrors(&u.Errors[i])...)
	}
	return out
}

// jsonCompatible turns YAML maps with any-typed keys into JSON-shaped values.
func jsonCompatible(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			c, err := jsonCompatible(x)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("a key (%v) is not text", k)
			}
			c, err := jsonCompatible(x)
			if err != nil {
				return nil, err
			}
			out[ks] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			c, err := jsonCompatible(x)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case uint64:
		return float64(t), nil
	case time.Time:
		return t.Format(time.RFC3339), nil
	}
	return v, nil
}

func (c *Config) applyDefaults() {
	if c.MaxConcurrency == 0 {
		c.MaxConcurrency = 32
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = Duration(30 * time.Second)
	}
	if c.SpillThreshold == 0 {
		c.SpillThreshold = 8 << 20
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
	if c.Log.Upload == "" {
		c.Log.Upload = "errors"
	}
	if c.Log.MaxSizeMB == 0 {
		c.Log.MaxSizeMB = 100
	}
	if c.Log.MaxFiles == 0 {
		c.Log.MaxFiles = 10
	}
	if c.Update.Channel == "" {
		c.Update.Channel = "stable"
	}
	if c.Update.Method == "" {
		c.Update.Method = "binary"
	}
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Timeout == 0 {
			t.Timeout = Duration(60 * time.Second)
		}
		if t.IdleTimeout == 0 {
			t.IdleTimeout = Duration(15 * time.Minute)
		}
		if (t.Kind == KindMCPHTTP || t.Kind == KindMCPStdio) && len(t.MCP.Methods) == 0 {
			t.MCP.Methods = append([]string(nil), DefaultMCPMethods...)
		}
		for j, m := range t.Methods {
			t.Methods[j] = strings.ToUpper(m)
		}
	}
}

// DefaultStateDir is where identity and the control socket live.
func DefaultStateDir() string {
	if p := os.Getenv("ZUANO_CONNECTOR_STATE_DIR"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, brand.Product, "connector", "state")
		}
	}
	return "/var/lib/" + BinaryName
}

func (c *Config) validate() []string {
	var p []string
	if c.Version != 1 {
		p = append(p, fmt.Sprintf("version must be 1, not %d", c.Version))
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		p = append(p, "log.format must be json or text")
	}
	switch c.Log.Upload {
	case "off", "errors", "metadata":
	default:
		p = append(p, "log.upload must be off, errors or metadata")
	}
	if c.Update.Method != "binary" && c.Update.Method != "package" {
		p = append(p, "update.method must be binary or package")
	}
	if c.Update.Window != "" {
		if _, err := ParseWindow(c.Update.Window); err != nil {
			p = append(p, "update.window: "+err.Error())
		}
	}
	if c.Broker.URL != "" {
		if u, err := url.Parse(c.Broker.URL); err != nil || u.Scheme != "wss" && u.Scheme != "https" && !isLoopbackURL(u) || u.Host == "" {
			p = append(p, "broker.url must be a wss:// or https:// address")
		}
	}
	if c.Health.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Health.Listen); err != nil {
			p = append(p, "health.listen must be host:port")
		}
	}
	seen := map[string]bool{}
	for i, t := range c.Targets {
		at := fmt.Sprintf("targets[%d] (%s)", i, t.Name)
		if !targetName.MatchString(t.Name) {
			p = append(p, at+": name must be lowercase letters, digits, - or _")
		}
		if seen[t.Name] {
			p = append(p, at+": duplicate name")
		}
		seen[t.Name] = true
		p = append(p, t.validate(at)...)
	}
	return p
}

func isLoopbackURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (t Target) validate(at string) []string {
	var p []string
	switch t.Kind {
	case KindHTTP, KindMCPHTTP:
		u, err := url.Parse(t.URL)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
			p = append(p, at+": url must be http:// or https:// with a host")
		case u.User != nil:
			p = append(p, at+": url must not carry a user name or password; use credentials")
		case u.RawQuery != "" || u.Fragment != "":
			p = append(p, at+": url must not have a query or fragment")
		}
		if t.Command != "" {
			p = append(p, at+": command only applies to mcp_stdio")
		}
	case KindMCPStdio:
		if t.Command == "" {
			p = append(p, at+": mcp_stdio needs a command")
		}
		if t.URL != "" {
			p = append(p, at+": url does not apply to mcp_stdio")
		}
	default:
		p = append(p, at+": kind must be http, mcp_http or mcp_stdio")
	}
	for _, cidr := range t.CIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			p = append(p, fmt.Sprintf("%s: %q is not a CIDR", at, cidr))
		}
	}
	for _, m := range t.Methods {
		if !httpMethods[m] {
			p = append(p, fmt.Sprintf("%s: %q is not an HTTP method", at, m))
		}
	}
	if t.Kind == KindHTTP && len(t.Methods) == 0 {
		p = append(p, at+": list the methods the gateway may use")
	}
	if t.Kind == KindHTTP && len(t.Paths) == 0 {
		p = append(p, at+": list the path patterns the gateway may use")
	}
	for _, g := range t.Paths {
		if !strings.HasPrefix(g, "/") {
			p = append(p, fmt.Sprintf("%s: path pattern %q must start with /", at, g))
		}
	}
	if t.TLS.InsecureSkipVerify && !t.AllowInsecure {
		p = append(p, at+": tls.insecure_skip_verify needs allow_insecure: true")
	}
	if (t.TLS.ClientCert == "") != (t.TLS.ClientKey == "") {
		p = append(p, at+": tls.client_cert and tls.client_key go together")
	}
	if c := t.Credentials; c != nil {
		n := 0
		if c.Header != "" || c.Value != "" {
			n++
			if c.Header == "" || c.Value == "" {
				p = append(p, at+": credentials.header and credentials.value go together")
			}
		}
		if c.Bearer != "" {
			n++
		}
		if c.Basic != nil {
			n++
		}
		if n > 1 {
			p = append(p, at+": give one of credentials.header/value, bearer or basic")
		}
	}
	if t.Rate != nil && (t.Rate.PerSecond <= 0 || t.Rate.Burst < 1) {
		p = append(p, at+": rate needs per_second above 0 and burst of at least 1")
	}
	return p
}

// AdvertisedTarget is what leaves the agent about a target: its name, kind and
// the shape of what it allows; never URLs, hosts, CIDRs or credentials.
type AdvertisedTarget struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Methods   []string `json:"methods,omitempty"`
	PathGlobs []string `json:"path_globs,omitempty"`
	MCPTools  []string `json:"mcp_tools,omitempty"`
}

// Allowlist is what HELLO advertises.
type Allowlist struct {
	Hash    string             `json:"hash"`
	Targets []AdvertisedTarget `json:"targets"`
}

// Allowlist computes the advertised view and its hash.
func (c *Config) Allowlist() Allowlist {
	out := Allowlist{Targets: []AdvertisedTarget{}}
	for _, t := range c.Targets {
		a := AdvertisedTarget{Name: t.Name, Kind: t.Kind, Methods: t.Methods, PathGlobs: t.Paths, MCPTools: t.MCP.Tools}
		if t.Kind != KindHTTP {
			a.Methods = t.MCP.Methods
		}
		out.Targets = append(out.Targets, a)
	}
	sort.Slice(out.Targets, func(i, j int) bool { return out.Targets[i].Name < out.Targets[j].Name })
	raw, _ := json.Marshal(out.Targets)
	sum := sha256.Sum256(raw)
	out.Hash = hex.EncodeToString(sum[:8])
	return out
}

// Target returns the target with the given name.
func (c *Config) Target(name string) (Target, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

var secretRef = regexp.MustCompile(`\$\{(file|env):([^}]+)\}`)

// ResolveSecret expands ${file:path} and ${env:NAME} references. A file is
// read on every call so a rotated secret is picked up without a restart.
func ResolveSecret(s string) (string, error) {
	var firstErr error
	out := secretRef.ReplaceAllStringFunc(s, func(m string) string {
		parts := secretRef.FindStringSubmatch(m)
		switch parts[1] {
		case "env":
			v, ok := os.LookupEnv(parts[2])
			if !ok && firstErr == nil {
				firstErr = fmt.Errorf("environment variable %s is not set", parts[2])
			}
			return v
		default:
			b, err := os.ReadFile(parts[2])
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("secret file %s: %w", parts[2], err)
				}
				return ""
			}
			return strings.TrimRight(string(b), "\r\n")
		}
	})
	return out, firstErr
}

// UpdateWindow is a maintenance window such as "Sat 02:00-04:00 Europe/Prague".
type UpdateWindow struct {
	Day        time.Weekday
	AnyDay     bool
	Start, End time.Duration
	Loc        *time.Location
}

// ParseWindow reads "[Day ]HH:MM-HH:MM [Zone]".
func ParseWindow(s string) (UpdateWindow, error) {
	w := UpdateWindow{Loc: time.UTC, AnyDay: true}
	fields := strings.Fields(s)
	days := map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}
	if len(fields) > 0 {
		if d, ok := days[strings.ToLower(fields[0])]; ok {
			w.Day, w.AnyDay = d, false
			fields = fields[1:]
		}
	}
	if len(fields) == 0 || len(fields) > 2 {
		return w, errors.New(`use "[Day ]HH:MM-HH:MM [Zone]", such as "Sat 02:00-04:00 Europe/Prague"`)
	}
	from, to, ok := strings.Cut(fields[0], "-")
	if !ok {
		return w, errors.New("the time range needs a dash, such as 02:00-04:00")
	}
	var err error
	if w.Start, err = parseClock(from); err != nil {
		return w, err
	}
	if w.End, err = parseClock(to); err != nil {
		return w, err
	}
	if len(fields) == 2 {
		if w.Loc, err = time.LoadLocation(fields[1]); err != nil {
			return w, fmt.Errorf("unknown time zone %q", fields[1])
		}
	}
	return w, nil
}

func parseClock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a time such as 02:00", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Contains reports whether now is inside the window.
func (w UpdateWindow) Contains(now time.Time) bool {
	l := now.In(w.Loc)
	if !w.AnyDay && l.Weekday() != w.Day {
		return false
	}
	since := time.Duration(l.Hour())*time.Hour + time.Duration(l.Minute())*time.Minute
	if w.Start <= w.End {
		return since >= w.Start && since < w.End
	}
	return since >= w.Start || since < w.End
}

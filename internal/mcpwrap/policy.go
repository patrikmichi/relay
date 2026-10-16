package mcpwrap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Policy is the offline firewall policy: the per-server subset of the
// organisation's firewall section.
type Policy struct {
	Firewall struct {
		Upstreams map[string]UpstreamPolicy `yaml:"upstreams"`
	} `yaml:"firewall"`
}

// UpstreamPolicy is the policy of one server.
type UpstreamPolicy struct {
	// OnDefinitionChange is block (hide a changed tool until reviewed) or warn
	// (keep serving the pinned definition).
	OnDefinitionChange string `yaml:"on_definition_change"`
	Tools              struct {
		Deny     []string          `yaml:"deny"`
		Classify map[string]string `yaml:"classify"`
	} `yaml:"tools"`
	// Inbound is withhold (default) or warn for results that carry
	// instructions to the model.
	Inbound string `yaml:"inbound"`
}

// LoadPolicy reads firewall.yaml; a missing file is the default policy. An
// unknown key is an error, so a typo is not silently ignored.
func LoadPolicy(file string) (*Policy, error) {
	p := &Policy{}
	raw, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for name, u := range p.Firewall.Upstreams {
		switch u.OnDefinitionChange {
		case "", "block", "warn":
		default:
			return nil, fmt.Errorf("%s: upstreams.%s.on_definition_change must be block or warn offline", file, name)
		}
		switch u.Inbound {
		case "", "withhold", "warn":
		default:
			return nil, fmt.Errorf("%s: upstreams.%s.inbound must be withhold or warn", file, name)
		}
		for tool, risk := range u.Tools.Classify {
			switch risk {
			case "read", "write", "destructive", "outward":
			default:
				return nil, fmt.Errorf("%s: upstreams.%s.tools.classify.%s must be read, write, destructive or outward", file, name, tool)
			}
		}
	}
	return p, nil
}

// For returns the policy of one server.
func (p *Policy) For(name string) UpstreamPolicy {
	u := p.Firewall.Upstreams[name]
	if u.OnDefinitionChange == "" {
		u.OnDefinitionChange = "block"
	}
	if u.Inbound == "" {
		u.Inbound = "withhold"
	}
	return u
}

// Denied reports whether a deny glob covers the tool.
func (u UpstreamPolicy) Denied(tool string) bool {
	for _, g := range u.Tools.Deny {
		if ok, _ := path.Match(g, tool); ok {
			return true
		}
	}
	return false
}

var verbRisk = []struct {
	risk  string
	verbs []string
}{
	{"destructive", []string{"delete", "drop", "purge", "destroy", "terminate", "remove", "wipe", "truncate"}},
	{"outward", []string{"send", "post", "publish", "notify", "invite", "share", "transfer", "pay", "refund", "email", "tweet", "broadcast"}},
	{"write", []string{"create", "update", "set", "write", "upload", "merge", "close", "edit", "add", "put", "patch", "move", "rename", "run", "exec"}},
	{"read", []string{"get", "list", "search", "read", "describe", "find", "fetch", "show", "query", "lookup", "view"}},
}

// Classify gives a tool's risk class: the administrator's classification, or
// from its name, and write when nothing says otherwise. A read-only hint does
// not lower it.
func (u UpstreamPolicy) Classify(def Definition) string {
	name := nameOf(def)
	if r, ok := u.Tools.Classify[name]; ok {
		return r
	}
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == ' ' })
	if len(words) == 0 {
		return "write"
	}
	for _, vr := range verbRisk {
		for _, v := range vr.verbs {
			if words[0] == v {
				return vr.risk
			}
		}
	}
	if ann, _ := def["annotations"].(map[string]any); ann != nil {
		if d, _ := ann["destructiveHint"].(bool); d {
			return "destructive"
		}
	}
	return "write"
}

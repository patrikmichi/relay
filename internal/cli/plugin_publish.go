package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/nofollow"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// pluginManifestName is the file that describes a bundle in a directory.
const pluginManifestName = "plugin.yaml"

const (
	maxPluginInline = 1 << 20
	maxPluginFile   = 256 << 10
)

var (
	semverPattern   = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	componentName   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	pinnedRefFormat = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]{0,63})@(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$`)
)

// pluginSpec is plugin.yaml.
type pluginSpec struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
	Components  struct {
		Skills []struct {
			Ref string `yaml:"ref"`
		} `yaml:"skills"`
		Commands []fileComponent `yaml:"commands"`
		Rules    []fileComponent `yaml:"rules"`
		Hooks    []struct {
			Event      string   `yaml:"event"`
			Matcher    string   `yaml:"matcher"`
			Command    string   `yaml:"command"`
			TimeoutSec int      `yaml:"timeoutSec"`
			Platforms  []string `yaml:"platforms"`
		} `yaml:"hooks"`
		MCP []struct {
			Name string `yaml:"name"`
			Ref  string `yaml:"ref"`
			HTTP string `yaml:"http"`
		} `yaml:"mcp"`
	} `yaml:"components"`
}

type fileComponent struct {
	File string `yaml:"file"`
	Name string `yaml:"name"`
}

// pluginComponent is one component as the gateway receives it: a pinned
// reference to a published resource, or inline content.
type pluginComponent struct {
	Kind    string         `json:"kind"`
	Name    string         `json:"name"`
	Ref     *pluginRef     `json:"ref,omitempty"`
	Content map[string]any `json:"content,omitempty"`
}

type pluginRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// buildPluginComponents reads plugin.yaml and the files it names and returns
// the bundle's components, validated. Nothing is read from outside root.
func buildPluginComponents(root string, spec *pluginSpec) ([]pluginComponent, []string, error) {
	var comps []pluginComponent
	var risky []string
	seen := map[string]bool{}
	add := func(c pluginComponent) error {
		key := c.Kind + "/" + c.Name
		if seen[key] {
			return fmt.Errorf("%s %q is listed twice", c.Kind, c.Name)
		}
		seen[key] = true
		comps = append(comps, c)
		return nil
	}
	pinned := func(kind, ref string) (*pluginRef, error) {
		m := pinnedRefFormat.FindStringSubmatch(ref)
		if m == nil {
			return nil, fmt.Errorf("%s ref %q must name a published version: name@1.2.3", kind, textsafe.Clean(ref))
		}
		return &pluginRef{Name: m[1], Version: m[2]}, nil
	}

	for _, s := range spec.Components.Skills {
		ref, err := pinned("skill", s.Ref)
		if err != nil {
			return nil, nil, err
		}
		if err := add(pluginComponent{Kind: "skill", Name: ref.Name, Ref: ref}); err != nil {
			return nil, nil, err
		}
	}
	for _, kind := range []struct {
		name  string
		items []fileComponent
	}{{"command", spec.Components.Commands}, {"rule", spec.Components.Rules}} {
		for _, item := range kind.items {
			data, err := readBundleFile(root, item.File)
			if err != nil {
				return nil, nil, fmt.Errorf("%s %q: %w", kind.name, textsafe.Clean(item.File), err)
			}
			name := item.Name
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(item.File), filepath.Ext(item.File))
			}
			if !componentName.MatchString(name) {
				return nil, nil, fmt.Errorf("%s name %q must be kebab-case (letters, digits and hyphens)", kind.name, textsafe.Clean(name))
			}
			fm, body := splitMarkdown(data)
			content := map[string]any{"body": body}
			if d, ok := fm["description"].(string); ok {
				content["description"] = d
			}
			if kind.name == "command" {
				if h, ok := fm["argument-hint"].(string); ok {
					content["argumentHint"] = h
				}
			} else {
				if g := frontmatterGlobs(fm["globs"]); len(g) > 0 {
					content["globs"] = g
				}
				if a, ok := fm["always"].(bool); ok {
					content["alwaysApply"] = a
				}
			}
			if err := add(pluginComponent{Kind: kind.name, Name: name, Content: content}); err != nil {
				return nil, nil, err
			}
		}
	}
	for i, h := range spec.Components.Hooks {
		if strings.TrimSpace(h.Event) == "" || strings.TrimSpace(h.Command) == "" || strings.ContainsAny(h.Command, "\r\n") {
			return nil, nil, fmt.Errorf("hook %d needs an event and a one-line command", i+1)
		}
		if h.TimeoutSec < 0 {
			return nil, nil, fmt.Errorf("hook %d has a negative timeoutSec", i+1)
		}
		content := map[string]any{"event": h.Event, "command": h.Command}
		if h.Matcher != "" {
			content["matcher"] = h.Matcher
		}
		if h.TimeoutSec > 0 {
			content["timeoutSec"] = h.TimeoutSec
		}
		if len(h.Platforms) > 0 {
			content["platforms"] = h.Platforms
		}
		if err := add(pluginComponent{Kind: "hook", Name: fmt.Sprintf("%s-%d", strings.ToLower(h.Event), i+1), Content: content}); err != nil {
			return nil, nil, err
		}
		risky = append(risky, "hook "+h.Event+": "+h.Command)
	}
	for i, m := range spec.Components.MCP {
		if (m.Ref == "") == (m.HTTP == "") {
			return nil, nil, fmt.Errorf("mcp entry %d needs exactly one of ref or http", i+1)
		}
		if m.Ref != "" {
			ref, err := pinned("mcp", m.Ref)
			if err != nil {
				return nil, nil, err
			}
			if err := add(pluginComponent{Kind: "mcp_server", Name: ref.Name, Ref: ref}); err != nil {
				return nil, nil, err
			}
			continue
		}
		u, err := url.Parse(m.HTTP)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, nil, fmt.Errorf("mcp http address %q must be an https URL without credentials", textsafe.Clean(m.HTTP))
		}
		name := m.Name
		if name == "" {
			name = strings.ReplaceAll(strings.Split(u.Hostname(), ".")[0], "_", "-")
		}
		if !componentName.MatchString(name) {
			return nil, nil, fmt.Errorf("mcp name %q must be kebab-case", textsafe.Clean(name))
		}
		if err := add(pluginComponent{Kind: "mcp_server", Name: name, Content: map[string]any{"type": "http", "url": u.String()}}); err != nil {
			return nil, nil, err
		}
	}
	if len(comps) == 0 {
		return nil, nil, errors.New("plugin.yaml lists no components")
	}
	sort.SliceStable(comps, func(i, j int) bool { return comps[i].Kind < comps[j].Kind })
	return comps, risky, nil
}

// readBundleFile reads a regular file inside root, refusing paths that leave
// it and links.
func readBundleFile(root, rel string) ([]byte, error) {
	if rel == "" || !filepath.IsLocal(rel) {
		return nil, errors.New("the file must be a relative path inside the plugin directory")
	}
	f, err := os.OpenFile(filepath.Join(root, rel), nofollow.ReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("the file must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPluginFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPluginFile {
		return nil, fmt.Errorf("the file is larger than %d bytes", maxPluginFile)
	}
	return data, nil
}

// splitMarkdown separates YAML frontmatter from the body.
func splitMarkdown(data []byte) (map[string]any, string) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return nil, text
	}
	head, body, found := strings.Cut(rest, "\n---\n")
	if !found {
		if head, found = strings.CutSuffix(rest, "\n---"); !found {
			return nil, text
		}
		body = ""
	}
	var fm map[string]any
	if yaml.Unmarshal([]byte(head), &fm) != nil {
		return nil, text
	}
	return fm, body
}

func frontmatterGlobs(v any) []string {
	switch t := v.(type) {
	case string:
		var out []string
		for _, g := range strings.Split(t, ",") {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
			}
		}
		return out
	case []any:
		var out []string
		for _, g := range t {
			if s, ok := g.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// loadPluginSpec reads and validates plugin.yaml from a directory, or from the
// file itself.
func loadPluginSpec(target, manifestRel string) (root string, raw []byte, spec pluginSpec, err error) {
	info, err := os.Stat(target)
	if err != nil {
		return "", nil, spec, err
	}
	file := target
	root = target
	if info.IsDir() {
		file = filepath.Join(target, firstNonEmptyString(manifestRel, pluginManifestName))
	} else {
		root = filepath.Dir(target)
	}
	if raw, err = readBundleFile(root, mustRel(root, file)); err != nil {
		return "", nil, spec, fmt.Errorf("read %s: %w", textsafe.Clean(file), err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return "", nil, spec, fmt.Errorf("%s: %w", pluginManifestName, err)
	}
	switch {
	case !pluginNamePattern.MatchString(spec.Name):
		return "", nil, spec, fmt.Errorf("%s: name %q must be lower case letters, digits, dots, hyphens or underscores", pluginManifestName, textsafe.Clean(spec.Name))
	case !semverPattern.MatchString(spec.Version):
		return "", nil, spec, fmt.Errorf("%s: version %q must be a semantic version such as 1.2.0", pluginManifestName, textsafe.Clean(spec.Version))
	}
	return root, raw, spec, nil
}

func mustRel(root, file string) string {
	if rel, err := filepath.Rel(root, file); err == nil {
		return rel
	}
	return file
}

// runPublishPlugin publishes a bundle described by plugin.yaml: the manifest
// and the components, with command and rule files read into the request, so
// the gateway needs no archive.
func runPublishPlugin(d publishDoer, target string, opts publishOpts, channel string) error {
	out := outputWriter([]io.Writer{opts.out})
	root, raw, spec, err := loadPluginSpec(target, opts.manifestPath)
	if err != nil {
		return err
	}
	comps, risky, err := buildPluginComponents(root, &spec)
	if err != nil {
		return err
	}
	componentsJSON, err := json.Marshal(map[string]any{"version": spec.Version, "description": spec.Description, "components": comps})
	if err != nil {
		return err
	}
	if len(componentsJSON) > maxPluginInline {
		return fmt.Errorf("the bundle's inline content is larger than %d bytes", maxPluginInline)
	}
	name := firstNonEmptyString(opts.name, spec.Name)

	counts := map[string]int{}
	for _, c := range comps {
		counts[c.Kind]++
	}
	if opts.dryRun {
		fmt.Fprintln(out, "[dry-run] would publish to:", publishAPIPath)
		fmt.Fprintf(out, "[dry-run] type:    plugin\n[dry-run] name:    %s\n[dry-run] version: %s\n[dry-run] channel: %s\n", textsafe.Clean(name), spec.Version, channel)
		for _, k := range []string{"skill", "command", "rule", "hook", "mcp_server"} {
			if counts[k] > 0 {
				fmt.Fprintf(out, "[dry-run] %ss: %d\n", k, counts[k])
			}
		}
		for _, r := range risky {
			fmt.Fprintf(out, "[dry-run] runs on user machines (needs admin review): %s\n", textsafe.Clean(r))
		}
		fmt.Fprintln(out, "[dry-run] no API call made")
		return nil
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mh := textproto.MIMEHeader{}
	mh.Set("Content-Disposition", `form-data; name="manifest"; filename="plugin.yaml"`)
	mh.Set("Content-Type", "application/yaml")
	part, err := mw.CreatePart(mh)
	if err != nil {
		return err
	}
	if _, err := part.Write(raw); err != nil {
		return err
	}
	for k, v := range map[string]string{"type": "plugin", "channel": channel, "pluginComponents": string(componentsJSON)} {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	if opts.name != "" {
		if err := mw.WriteField("name", opts.name); err != nil {
			return err
		}
	}
	if opts.slug != "" {
		if err := mw.WriteField("slug", opts.slug); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	if len(risky) > 0 {
		fmt.Fprintf(out, "note: %d hook(s) run on user machines, so an organisation admin must review this bundle before it is published\n", len(risky))
	}
	resp, err := d.Post(publishAPIPath, mw.FormDataContentType(), &buf)
	if err != nil {
		return fmt.Errorf("POST %s: %w", publishAPIPath, err)
	}
	defer resp.Body.Close()
	rawBody, err := readLimitedResponse(resp.Body, maxResponseBytes)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if err := handlePublishResponse(resp.StatusCode, resp.Header, rawBody, opts.gatewayURL, out); err != nil {
		return err
	}
	if opts.watch && (resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK) {
		var pub publishResponse
		if json.Unmarshal(rawBody, &pub) == nil && pub.VersionID != "" {
			statusDoer, ok := d.(publishStatusDoer)
			if !ok {
				return errors.New("--watch requires a client that supports status polling (internal error)")
			}
			fmt.Fprintln(out)
			return watchPublishStatus(statusDoer, pub.VersionID, out)
		}
	}
	return nil
}

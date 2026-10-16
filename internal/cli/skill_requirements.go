package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/agentport"
	"github.com/patrikmichi/zuano/internal/agentsync"
)

// connectorRequirement is one entry of GET /api/marketplace/skills/{id}/requirements:
// a connector a skill needs and whether the caller has connected it.
type connectorRequirement struct {
	Slug      string   `json:"slug"`
	Tools     []string `json:"tools,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	Connected bool     `json:"connected"`
}

type skillRequirements struct {
	SkillID      string                 `json:"skillId"`
	Version      string                 `json:"version"`
	Requirements []connectorRequirement `json:"requirements"`
	Satisfied    bool                   `json:"satisfied"`
	Missing      []string               `json:"missing"`
}

var errSkillNotFound = errors.New("skill not found")

func skillRequirementsPath(id string) string {
	return "/api/marketplace/skills/" + url.PathEscape(id) + "/requirements"
}

// fetchSkillRequirements reads the connectors a published skill needs and which
// of them the caller has connected. A missing skill, or a gateway without the
// route, is errSkillNotFound.
func fetchSkillRequirements(parent context.Context, doer skillSearchDoer, id string) (*skillRequirements, error) {
	ctx, cancel := context.WithTimeout(parent, searchTimeout)
	defer cancel()
	resp, err := doer.GetContext(ctx, skillRequirementsPath(id))
	if err != nil {
		return nil, fmt.Errorf("skill requirements: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errSkillNotFound
	case http.StatusUnauthorized:
		return nil, errors.New(offlineGuidance)
	default:
		return nil, fmt.Errorf("skill requirements failed (HTTP %d)", resp.StatusCode)
	}
	data, err := readLimitedResponse(resp.Body, maxSearchResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read skill requirements: %w", err)
	}
	var doc struct {
		skillRequirements
		Satisfied *bool `json:"satisfied"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.Satisfied == nil {
		return nil, errors.New("the gateway returned an unexpected skill requirements response")
	}
	out := doc.skillRequirements
	out.Satisfied = *doc.Satisfied
	return &out, nil
}

// resolveSkillRef turns a res_ id or a slug into a catalog entry. A res_ id
// the search does not return is used as given.
func resolveSkillRef(ctx context.Context, doer skillSearchDoer, ref string) (skillSearchEntry, error) {
	results, err := searchSkills(ctx, doer, ref)
	if err != nil {
		if strings.HasPrefix(ref, "res_") {
			return skillSearchEntry{ID: ref}, nil
		}
		return skillSearchEntry{}, err
	}
	for _, r := range results {
		if r.ID == ref || strings.EqualFold(r.Slug, ref) {
			return r, nil
		}
	}
	if strings.HasPrefix(ref, "res_") {
		return skillSearchEntry{ID: ref}, nil
	}
	return skillSearchEntry{}, fmt.Errorf("no skill %q in the catalog; try `relay skill search %s`", ref, ref)
}

type requirementOpts struct {
	yes        bool
	noConnect  bool
	ignore     bool
	dryRun     bool
	gatewayURL string
}

// connectConnector links one connector; a variable so tests can stand in for
// the interactive connect flow.
var connectConnector = func(cmd *cobra.Command, slug string, gatewayURL string) error {
	return runConnect(cmd, slug, connectFlags{gatewayURL: gatewayURL, wait: 5 * time.Minute})
}

// askYesNo reads one answer; an empty line means yes and end of input means no.
func askYesNo(r *bufio.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [Y/n] ", question)
	line, err := r.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if err != nil && answer == "" {
		return false
	}
	return answer == "" || answer == "y" || answer == "yes"
}

// canPrompt is false when stdin is the process's own and not a terminal, so a
// pipeline or an agent harness never waits for an answer that cannot come.
func canPrompt(in io.Reader) bool {
	f, isFile := in.(*os.File)
	return !isFile || f != os.Stdin || isInteractiveTerminal(os.Stdin)
}

// ensureSkillRequirements checks the connectors a catalog skill needs before
// it is installed, offers to connect the missing ones, re-checks, and fails
// when some stay unmet unless they are ignored. A gateway that cannot answer
// does not block the install.
func ensureSkillRequirements(cmd *cobra.Command, doer skillSearchDoer, id string, o requirementOpts) error {
	ctx, out, errOut := cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	req, err := fetchSkillRequirements(ctx, doer, id)
	if err != nil {
		if !errors.Is(err, errSkillNotFound) {
			fmt.Fprintf(errOut, "warning: could not check the skill's connector requirements: %v\n", err)
		}
		return nil
	}
	if req.Satisfied || len(req.Missing) == 0 {
		return nil
	}

	fmt.Fprintf(out, "This skill needs connectors you have not connected: %s\n", strings.Join(req.Missing, ", "))
	if o.dryRun {
		fmt.Fprintln(out, "dry run: nothing is connected; connect them with `relay connect <slug>` before installing")
		return nil
	}

	if !o.noConnect {
		in := cmd.InOrStdin()
		reader := bufio.NewReader(in)
		interactive := canPrompt(in)
		for _, slug := range req.Missing {
			switch {
			case o.yes:
			case !interactive:
				fmt.Fprintf(out, "Not asking to connect %s: no terminal. Pass --yes to connect, or run `relay connect %s`.\n", slug, slug)
				continue
			case !askYesNo(reader, out, fmt.Sprintf("Connect %s now?", slug)):
				continue
			}
			if err := connectConnector(cmd, slug, o.gatewayURL); err != nil {
				fmt.Fprintf(errOut, "warning: could not connect %s: %v\n", slug, err)
			}
		}
		req, err = fetchSkillRequirements(ctx, doer, id)
		if err != nil {
			return fmt.Errorf("re-check the skill's connector requirements: %w", err)
		}
		if req.Satisfied || len(req.Missing) == 0 {
			return nil
		}
	}

	missing := strings.Join(req.Missing, ", ")
	if o.ignore {
		fmt.Fprintf(errOut, "warning: installing without the connectors it needs (%s); the skill will not work until they are connected\n", missing)
		return nil
	}
	return fmt.Errorf("the skill needs connectors that are not connected: %s\nconnect them with `relay connect <slug>`, or install anyway with --ignore-requirements", missing)
}

// SkillInfoCmd returns `relay skill info <id|slug>`.
func SkillInfoCmd() *cobra.Command {
	var gatewayURL string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "info <catalog-id|slug>",
		Short: "Show a catalog skill, its connector requirements and whether you can use it",
		Long: `Show a catalog skill's latest version, the connectors it needs and which of
them you have connected, and where it is installed on this machine.

Examples:
  relay skill info weekly-timesheet
  relay skill info res_abc123 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillInfo(cmd, args[0], gatewayURL, jsonOut)
		},
	}
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print one JSON document on stdout; the text goes to stderr")
	return cmd
}

type installedSkill struct {
	Provider string `json:"provider"`
	Scope    string `json:"scope"`
	Version  string `json:"version,omitempty"`
}

// installedCopies lists the manifest entries recorded for a catalog skill.
func installedCopies(catalogID string) []installedSkill {
	m, err := agentport.LoadManifest()
	if err != nil {
		return nil
	}
	var out []installedSkill
	for _, e := range m.Entries {
		if e.Kind == agentport.KindSkill && e.Provenance.CatalogID == catalogID {
			out = append(out, installedSkill{Provider: string(e.Provider), Scope: string(e.Scope), Version: e.Provenance.Version})
		}
	}
	return out
}

func runSkillInfo(cmd *cobra.Command, ref, gatewayURL string, jsonOut bool) error {
	doc, finish := beginAction(cmd, jsonOut, "skill info")
	return finish(doSkillInfo(cmd, ref, gatewayURL, doc))
}

func doSkillInfo(cmd *cobra.Command, ref, gatewayURL string, doc actionDoc) error {
	gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
	if err != nil {
		return err
	}
	c, err := resolveClient(gURL)
	if err != nil {
		return err
	}
	entry, err := resolveSkillRef(cmd.Context(), c, ref)
	if err != nil {
		return err
	}
	req, err := fetchSkillRequirements(cmd.Context(), c, entry.ID)
	if errors.Is(err, errSkillNotFound) {
		return fmt.Errorf("skill %q not found in the catalog", ref)
	}
	if err != nil {
		return err
	}
	installed := installedCopies(entry.ID)
	if installed == nil {
		installed = []installedSkill{}
	}
	doc.put("skill", entry)
	doc.put("requirements", req)
	doc.put("installed", installed)

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "ID:          %s\n", entry.ID)
	if entry.Slug != "" {
		fmt.Fprintf(out, "Slug:        %s\n", entry.Slug)
	}
	version := req.Version
	if version == "" && entry.Version != nil {
		version = *entry.Version
	}
	fmt.Fprintf(out, "Version:     %s\n", dash(version))
	if entry.Description != nil && *entry.Description != "" {
		fmt.Fprintf(out, "Description: %s\n", *entry.Description)
	}
	if len(installed) == 0 {
		fmt.Fprintln(out, "Installed:   no")
	} else {
		for i, in := range installed {
			label := "Installed:  "
			if i > 0 {
				label = "            "
			}
			fmt.Fprintf(out, "%s%s (%s) %s\n", label, in.Provider, in.Scope, in.Version)
		}
	}

	if len(req.Requirements) == 0 {
		fmt.Fprintln(out, "Requires:    no connectors")
		return nil
	}
	fmt.Fprintln(out, "Requires:")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  CONNECTOR\tTOOLS\tSCOPES\tSTATUS")
	for _, r := range req.Requirements {
		status := "missing"
		if r.Connected {
			status = "connected"
		}
		tools := "all"
		if len(r.Tools) > 0 {
			tools = strings.Join(r.Tools, ",")
		}
		scopes := "read,write"
		if len(r.Scopes) > 0 {
			scopes = strings.Join(r.Scopes, ",")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.Slug, tools, scopes, status)
	}
	_ = tw.Flush()
	if req.Satisfied {
		fmt.Fprintln(out, "Status:      ready, every connector is connected")
	} else {
		fmt.Fprintf(out, "Status:      not ready, connect: %s (relay connect <slug>)\n", strings.Join(req.Missing, ", "))
	}
	return nil
}

// connectorRef reads one entry of a SKILL.md requires.connectors list, which
// is either {slug: ...} or a bare slug.
type connectorRef struct{ Slug string }

func (c *connectorRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Slug = n.Value
		return nil
	}
	var obj struct {
		Slug string `yaml:"slug"`
	}
	if err := n.Decode(&obj); err != nil {
		return err
	}
	c.Slug = obj.Slug
	return nil
}

// skillFrontmatterRequires returns the connector slugs a SKILL.md declares in
// its frontmatter under requires.connectors.
func skillFrontmatterRequires(content []byte) []string {
	text := strings.TrimPrefix(string(content), string(rune(0xFEFF)))
	if !strings.HasPrefix(text, "---") {
		return nil
	}
	rest := strings.TrimPrefix(text[3:], "\r")
	rest = strings.TrimPrefix(rest, "\n")
	block, _, _ := strings.Cut(rest, "\n---")
	var fm struct {
		Requires struct {
			Connectors []connectorRef `yaml:"connectors"`
		} `yaml:"requires"`
	}
	if yaml.Unmarshal([]byte(block), &fm) != nil {
		return nil
	}
	var slugs []string
	for _, c := range fm.Requires.Connectors {
		if c.Slug != "" {
			slugs = append(slugs, c.Slug)
		}
	}
	return slugs
}

// skillRequirementWarnings names the source skills whose frontmatter requires
// connectors, so a sync does not copy a skill that cannot work yet without
// saying so. It reads local files only.
func skillRequirementWarnings(from string, scopes []agentsync.Scope) []string {
	src, ok := agentport.AdapterByID(syncSourceProvider(from))
	if !ok {
		return nil
	}
	var warns []string
	seen := map[string]bool{}
	for _, scope := range scopes {
		refs, err := agentport.List(src, agentport.Scope(scope))
		if err != nil {
			continue
		}
		for _, ref := range refs {
			info, err := os.Stat(ref.Path)
			if err != nil || !info.IsDir() || seen[ref.Name] {
				continue
			}
			content, err := os.ReadFile(filepath.Join(ref.Path, "SKILL.md"))
			if err != nil {
				continue
			}
			if slugs := skillFrontmatterRequires(content); len(slugs) > 0 {
				seen[ref.Name] = true
				warns = append(warns, fmt.Sprintf("skill %q requires connectors %s; sync does not connect them, so link them with `relay connect <slug>` (`relay connectors --status` shows which are linked)", ref.Name, strings.Join(slugs, ", ")))
			}
		}
	}
	return warns
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
)

// linkProbeTimeout bounds the per-service credential probes behind the
// connection status column; a slow gateway must not stall `zuano services`.
const linkProbeTimeout = 10 * time.Second

const linkProbeWorkers = 6

// serviceLink is the caller's credential state for one gateway service, as
// reported by GET /api/cli/services/authorize.
type serviceLink struct {
	Known             bool
	AccountLinked     bool
	AuthorizationPath string
	Err               string
}

// fetchServiceLink asks the gateway whether the caller has credentials for
// svc. The endpoint needs a tool scope, so the service's first tool stands
// in; the answer concerns the account link, not that tool.
func fetchServiceLink(ctx context.Context, c *client.Client, svc DiscoveryService) serviceLink {
	if len(svc.Tools) == 0 {
		return serviceLink{Err: "no tools to probe"}
	}
	q := url.Values{"service": {svc.ID}, "scope": {svc.Tools[0].Name}}
	resp, err := c.GetContext(ctx, "/api/cli/services/authorize?"+q.Encode())
	if err != nil {
		return serviceLink{Err: "unreachable"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var doc struct {
		AccountLinked     bool   `json:"accountLinked"`
		AuthorizationPath string `json:"authorizationPath"`
		Error             string `json:"error"`
	}
	_ = json.Unmarshal(body, &doc)
	switch resp.StatusCode {
	case http.StatusOK:
		return serviceLink{Known: true, AccountLinked: doc.AccountLinked, AuthorizationPath: doc.AuthorizationPath}
	case http.StatusConflict:
		return serviceLink{Known: true, AuthorizationPath: "/settings/connections", Err: "needs a personal connection"}
	default:
		if doc.Error != "" {
			return serviceLink{Err: doc.Error}
		}
		return serviceLink{Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
}

type statusAnswer int

const (
	statusAnswered    statusAnswer = iota // the gateway reported a definitive state
	statusUnavailable                     // 401/403/404: the gateway has no bearer-capable status endpoint
	statusFailed                          // transport error or unusable reply
)

// queryConnected asks GET /api/connections?service=<slug> for the caller's
// connection state. It is the authoritative source for declarative OAuth
// connectors, whose credentials the authorize probe cannot see.
func queryConnected(ctx context.Context, c *client.Client, slug string) (connected bool, answer statusAnswer) {
	resp, err := c.GetContext(ctx, "/api/connections?service="+url.QueryEscape(slug))
	if err != nil {
		return false, statusFailed
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return false, statusUnavailable
	default:
		return false, statusFailed
	}
	var doc struct {
		Connected *bool `json:"connected"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&doc) != nil || doc.Connected == nil {
		// The unfiltered listing shape: this gateway ignores ?service=.
		return false, statusUnavailable
	}
	return *doc.Connected, statusAnswered
}

// statusEndpointMissingAfter is how many unavailable answers, with no answer
// at all, show the gateway has no connection status endpoint.
const statusEndpointMissingAfter = 3

// fetchServiceLinks probes every accessible service that takes credentials,
// preferring the connection status endpoint and falling back to the
// authorize probe once the gateway turns out not to offer it.
func fetchServiceLinks(ctx context.Context, c *client.Client, services []DiscoveryService) map[string]serviceLink {
	// One service the endpoint cannot answer for (a 404 for an unknown slug)
	// falls back to the authorize probe for that service only. The endpoint
	// counts as missing when several in a row were unavailable and none ever
	// answered.
	var unavailable atomic.Int32
	var answeredAny atomic.Bool
	ctx, cancel := context.WithTimeout(ctx, linkProbeTimeout)
	defer cancel()
	out := map[string]serviceLink{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, linkProbeWorkers)
	for _, svc := range services {
		if !svc.Accessible || !takesCredentials(svc) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var l serviceLink
			answered := false
			if answeredAny.Load() || unavailable.Load() < statusEndpointMissingAfter {
				connected, answer := queryConnected(ctx, c, svc.ID)
				answered = answer == statusAnswered
				if answered {
					l = serviceLink{Known: true, AccountLinked: connected}
					answeredAny.Store(true)
				} else if answer == statusUnavailable {
					unavailable.Add(1)
				}
			}
			if !answered {
				l = fetchServiceLink(ctx, c, svc)
			}
			mu.Lock()
			out[svc.ID] = l
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// takesCredentials reports whether a service needs an account or key at all.
func takesCredentials(svc DiscoveryService) bool {
	switch svc.AuthType {
	case "", "none", "mcp":
		return false
	}
	return true
}

func authLabel(svc DiscoveryService) string {
	switch svc.AuthType {
	case "apikey":
		return "API key"
	case "multi-account-apikey":
		return "API key (multi-account)"
	case "oauth":
		return "OAuth"
	case "none":
		return "none"
	case "mcp":
		return "custom MCP"
	case "":
		return "-"
	}
	return svc.AuthType
}

// linkLabel is the connection status shown next to a service.
func linkLabel(svc DiscoveryService, links map[string]serviceLink) string {
	switch {
	case !svc.Accessible:
		return "no access"
	case !takesCredentials(svc):
		return "-"
	}
	l, ok := links[svc.ID]
	switch {
	case !ok || (!l.Known && l.Err != ""):
		return "unknown"
	case l.AccountLinked:
		return "connected"
	case l.AuthorizationPath == "/admin":
		return "not connected (admin)"
	}
	return "not connected"
}

func normalizeToken(s string) string {
	return strings.NewReplacer(" ", "", "_", "", "-", "").Replace(strings.ToLower(s))
}

func hasToken(haystack, token string) bool {
	return strings.Contains(strings.ToLower(haystack), token)
}

// scoreService mirrors the gateway's catalog search: every query token must
// match the service or one of its tools; id and name hits rank highest.
func scoreService(svc DiscoveryService, tokens []string) (int, bool) {
	if len(tokens) == 0 {
		return 0, true
	}
	id, name := normalizeToken(svc.ID), normalizeToken(svc.Name)
	total := 0
	for _, tok := range tokens {
		flat := normalizeToken(tok)
		best := 0
		switch {
		case id == flat || name == flat:
			best = 100
		case strings.HasPrefix(id, flat) || strings.HasPrefix(name, flat):
			best = 60
		case strings.Contains(id, flat) || strings.Contains(name, flat):
			best = 40
		case hasToken(svc.Category, tok):
			best = 30
		default:
			for _, tag := range svc.Tags {
				if hasToken(tag, tok) {
					best = 30
					break
				}
			}
		}
		if best == 0 {
			for _, t := range svc.Tools {
				if hasToken(strings.ReplaceAll(t.Name, "_", " "), tok) || hasToken(t.Name, tok) {
					best = 20
					break
				}
				if hasToken(t.Description, tok) {
					best = 10
				}
			}
		}
		if best == 0 && (hasToken(svc.Description, tok) || hasToken(svc.Summary, tok)) {
			best = 10
		}
		if best == 0 {
			return 0, false
		}
		total += best
	}
	return total, true
}

// filterServices applies a free-text query and an optional category, best
// matches first, then by name.
func filterServices(services []DiscoveryService, query, category string) []DiscoveryService {
	tokens := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return r == ' ' || r == ',' })
	type hit struct {
		svc   DiscoveryService
		score int
	}
	var hits []hit
	for _, svc := range services {
		if category != "" && !strings.EqualFold(svc.Category, category) {
			continue
		}
		if score, ok := scoreService(svc, tokens); ok {
			hits = append(hits, hit{svc, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].svc.Name < hits[j].svc.Name
	})
	out := make([]DiscoveryService, len(hits))
	for i, h := range hits {
		out[i] = h.svc
	}
	return out
}

type connectorsFlags struct {
	category   string
	categories bool
	status     bool
	jsonOut    bool
	gatewayURL string
}

func addConnectorsFlags(cmd *cobra.Command, f *connectorsFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.category, "category", "", "Only connectors in this category")
	fl.BoolVar(&f.categories, "categories", false, "List categories with counts instead of connectors")
	fl.BoolVar(&f.status, "status", false, "Also show whether your account is connected (one probe per connector)")
	fl.BoolVar(&f.jsonOut, "json", false, "Print raw JSON")
	fl.StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
}

// ConnectorsCmd returns `zuano connectors`: browse the gateway's service
// catalog.
func ConnectorsCmd() *cobra.Command {
	var f connectorsFlags
	cmd := &cobra.Command{
		Use:   "connectors",
		Short: "Browse the gateway's connector catalog",
		Long: `List the services the gateway offers: built-in integrations, declarative
connectors and catalog MCP servers, with category and how each one
authenticates. Narrow the list with 'zuano connectors search <query>' or
--category. Use 'zuano connect <slug>' to link your own account.

Examples:
  zuano connectors
  zuano connectors --category CRM
  zuano connectors search invoice
  zuano connectors --categories`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConnectors(cmd, "", f)
		},
	}
	addConnectorsFlags(cmd, &f)

	var sf connectorsFlags
	search := &cobra.Command{
		Use:   "search <query>",
		Short: "Search connectors by name, category, tag, description or tool",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnectors(cmd, strings.Join(args, " "), sf)
		},
	}
	addConnectorsFlags(search, &sf)
	cmd.AddCommand(search)
	return cmd
}

func runConnectors(cmd *cobra.Command, query string, f connectorsFlags) error {
	gURL, err := resolveGatewayURLOrFailClosed(f.gatewayURL)
	if err != nil {
		return err
	}
	c, err := resolveClient(gURL)
	if err != nil {
		return err
	}
	info, err := fetchIntegrations(c, cmd.Context())
	if err != nil {
		return fmt.Errorf("connectors failed: %w", err)
	}
	if info == nil {
		return errTokenRevoked
	}
	out := cmd.OutOrStdout()

	if f.categories {
		return printCategories(out, info.Services, f.jsonOut)
	}
	matches := filterServices(info.Services, query, f.category)
	var links map[string]serviceLink
	if f.status {
		links = fetchServiceLinks(cmd.Context(), c, matches)
	}
	if f.jsonOut {
		type row struct {
			DiscoveryService
			Status string `json:"status,omitempty"`
		}
		rows := make([]row, len(matches))
		for i, m := range matches {
			m.Tools = nil
			rows[i] = row{DiscoveryService: m}
			if f.status {
				rows[i].Status = linkLabel(m, links)
			}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(matches) == 0 {
		fmt.Fprintln(out, "no connectors match")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	header := "SLUG\tNAME\tCATEGORY\tAUTH\tTOOLS\tACCESS"
	if f.status {
		header += "\tSTATUS"
	}
	fmt.Fprintln(tw, header)
	for _, m := range matches {
		access := "yes"
		if !m.Accessible {
			access = "no"
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s", m.ID, m.Name, dash(m.Category), authLabel(m), m.ToolCount, access)
		if f.status {
			line += "\t" + linkLabel(m, links)
		}
		fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\n%d connectors. Link your account with `zuano connect <slug>`.\n", len(matches))
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printCategories(out io.Writer, services []DiscoveryService, jsonOut bool) error {
	counts := map[string]int{}
	for _, s := range services {
		counts[dash(s.Category)]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(counts)
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tCONNECTORS")
	for _, n := range names {
		fmt.Fprintf(tw, "%s\t%d\n", n, counts[n])
	}
	return tw.Flush()
}

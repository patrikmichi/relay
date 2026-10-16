package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/keychain"
)

// Exit statuses of the skill eval commands, on top of 0, 1 and the wait
// timeout status 17.
const (
	exitEvalFailed     = exitRunFailed // the run or job ended failed or cancelled
	exitEvalRegression = 21            // --fail-on-regression and a score dropped
)

// One request is bounded by --timeout; waiting for a run is bounded separately
// by --wait-timeout. Variables so tests can shorten them.
var (
	evalPollInterval = 3 * time.Second
	evalSettleDelay  = 2 * time.Second
)

const evalMaxResponse = 16 << 20

// apiError is a gateway refusal with the fields the eval routes put in an
// error body: {"error": "...", "code": "...", "missing": [...], "issues": [...]}.
type apiError struct {
	Status  int
	Code    string
	Message string
	Missing []string
}

func (e *apiError) Error() string { return e.Message }

// evalAPI talks to /api/marketplace/skills/{id}/... for one catalog skill.
type evalAPI struct {
	c     *client.Client
	skill skillSearchEntry
}

func (a *evalAPI) path(suffix string) string {
	return "/api/marketplace/skills/" + url.PathEscape(a.skill.ID) + suffix
}

func (a *evalAPI) label() string {
	if a.skill.Slug != "" {
		return a.skill.Slug
	}
	return a.skill.ID
}

// do sends one request to a route under the skill.
func (a *evalAPI) do(ctx context.Context, method, suffix string, body any) ([]byte, error) {
	return a.request(ctx, method, a.path(suffix), body)
}

// request sends one request. The raw body is returned for 2xx; anything else
// is an *apiError (or the agent-session guidance) carrying the gateway's message.
func (a *evalAPI) request(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	// One request is bounded like any other control request: --timeout and
	// ZUANO_TIMEOUT apply, 0 meaning no limit.
	ctx, cancel, err := requestContext(ctx, controlRequest)
	if err != nil {
		return nil, err
	}
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.c.GatewayURL+path, rd)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.c.Do(req)
	if err != nil {
		return nil, timeoutCause(ctx, fmt.Errorf("%s %s: %w", method, path, err))
	}
	defer resp.Body.Close()
	raw, err := readLimitedResponse(resp.Body, evalMaxResponse)
	if err != nil {
		return nil, timeoutCause(ctx, fmt.Errorf("read response: %w", err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	return nil, a.errorFor(method, resp.StatusCode, raw)
}

// errorFor turns a refusal into a message that says what to do next.
func (a *evalAPI) errorFor(method string, status int, raw []byte) error {
	mutating := method != http.MethodGet && method != http.MethodHead
	agent := a.c.Profile() == keychain.ProfileAgent
	// resolveMarketplaceCaller answers a refused agent credential with 401, not 403.
	if agent && mutating && status == http.StatusUnauthorized {
		return agentSessionDenied(keychain.ProfileAgent, http.StatusForbidden, raw)
	}
	if err := agentSessionDenied(a.c.Profile(), status, raw); err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		return errTokenRevoked
	}

	var doc struct {
		Code    string   `json:"code"`
		Missing []string `json:"missing"`
		Issues  []struct {
			Path    []any  `json:"path"`
			Message string `json:"message"`
		} `json:"issues"`
	}
	_ = json.Unmarshal(raw, &doc)
	msg := gatewayMessage(raw)
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", status)
	}
	for _, is := range doc.Issues {
		parts := make([]string, len(is.Path))
		for i, p := range is.Path {
			parts[i] = fmt.Sprint(p)
		}
		msg += fmt.Sprintf("\n  %s: %s", strings.Join(parts, "."), is.Message)
	}
	switch {
	case status == http.StatusServiceUnavailable && doc.Code == "NOT_ENABLED":
		msg = "this gateway has not enabled the hosted runtime, which evals and optimization need"
	case doc.Code == "SKILL_REQUIREMENTS_UNMET" && len(doc.Missing) > 0:
		msg += fmt.Sprintf("\nconnect %s with `zuano connect <slug>` first", strings.Join(doc.Missing, ", "))
	case status == http.StatusForbidden && doc.Code == "NO_USER":
		msg += "\nthe account needs a user row on the gateway to run evals as"
	}
	return &apiError{Status: status, Code: doc.Code, Message: msg, Missing: doc.Missing}
}

func (a *evalAPI) getJSON(ctx context.Context, suffix string, out any) ([]byte, error) {
	raw, err := a.do(ctx, http.MethodGet, suffix, nil)
	if err != nil {
		return nil, err
	}
	return raw, decodeEval(raw, out)
}

func (a *evalAPI) sendJSON(ctx context.Context, method, suffix string, body, out any) ([]byte, error) {
	raw, err := a.do(ctx, method, suffix, body)
	if err != nil {
		return nil, err
	}
	return raw, decodeEval(raw, out)
}

func decodeEval(raw []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the gateway returned an unexpected response: %w", err)
	}
	return nil
}

// evalFlags are the options every skill eval command shares.
type evalFlags struct {
	gatewayURL string
	jsonOut    bool
}

func (f *evalFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().BoolVar(&f.jsonOut, "json", false, "Print the gateway's JSON instead of text")
}

// open authenticates and resolves the skill the command is about.
func (f *evalFlags) open(cmd *cobra.Command, skillRef string) (*evalAPI, error) {
	gURL, err := resolveGatewayURLOrFailClosed(f.gatewayURL)
	if err != nil {
		return nil, err
	}
	c, err := resolveClient(gURL)
	if err != nil {
		return nil, err
	}
	entry, err := resolveSkillRef(cmd.Context(), c, skillRef)
	if err != nil {
		return nil, err
	}
	return &evalAPI{c: c, skill: entry}, nil
}

// printIndented writes a gateway JSON body indented.
func printIndented(out io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		_, err = out.Write(raw)
		return err
	}
	buf.WriteByte('\n')
	_, err := out.Write(buf.Bytes())
	return err
}

type evalSet struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	VersionID    string `json:"versionId"`
	TrainPercent int    `json:"trainPercent"`
	Counts       struct {
		Trigger    int `json:"trigger"`
		Task       int `json:"task"`
		Train      int `json:"train"`
		Validation int `json:"validation"`
	} `json:"counts"`
	Cases []evalCase `json:"cases"`
}

type evalCase struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Input         string `json:"input"`
	Split         string `json:"split"`
	ShouldTrigger *bool  `json:"shouldTrigger"`
}

// resolveSet finds a set by id or exact name. With no reference it takes the
// skill's only set.
func (a *evalAPI) resolveSet(ctx context.Context, ref string) (evalSet, error) {
	var doc struct {
		Sets []evalSet `json:"sets"`
	}
	if _, err := a.getJSON(ctx, "/evals/sets", &doc); err != nil {
		return evalSet{}, err
	}
	if ref == "" {
		switch len(doc.Sets) {
		case 0:
			return evalSet{}, fmt.Errorf("%s has no eval sets; create one with `zuano skill evals sets create %s --name NAME` or import one", a.label(), a.label())
		case 1:
			return doc.Sets[0], nil
		}
		names := make([]string, len(doc.Sets))
		for i, s := range doc.Sets {
			names[i] = fmt.Sprintf("%s (%s)", s.Name, s.ID)
		}
		return evalSet{}, fmt.Errorf("%s has several eval sets, pick one with --set: %s", a.label(), strings.Join(names, ", "))
	}
	for _, s := range doc.Sets {
		if s.ID == ref {
			return s, nil
		}
	}
	var byName []evalSet
	for _, s := range doc.Sets {
		if s.Name == ref {
			byName = append(byName, s)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return evalSet{}, fmt.Errorf("no eval set %q on %s; see `zuano skill evals sets list %s`", ref, a.label(), a.label())
	}
	return evalSet{}, fmt.Errorf("several eval sets are named %q; use the id", ref)
}

type skillVersion struct {
	ID     string `json:"id"`
	Semver string `json:"semver"`
	State  string `json:"state"`
}

// resolveVersion maps a semver or a rev_ id to a version of the skill; an
// empty reference is the current published version.
func (a *evalAPI) resolveVersion(ctx context.Context, ref string) (skillVersion, error) {
	var doc struct {
		Versions       []skillVersion `json:"versions"`
		CurrentVersion *skillVersion  `json:"currentVersion"`
	}
	if _, err := a.detail(ctx, &doc); err != nil {
		return skillVersion{}, err
	}
	if ref == "" {
		if doc.CurrentVersion == nil {
			return skillVersion{}, fmt.Errorf("%s has no published version; name one with --version or use --draft", a.label())
		}
		return *doc.CurrentVersion, nil
	}
	want := strings.TrimPrefix(ref, "v")
	for _, v := range doc.Versions {
		if v.ID == ref || v.Semver == want {
			return v, nil
		}
	}
	return skillVersion{}, fmt.Errorf("%s has no version %q", a.label(), ref)
}

// detail reads GET /api/catalog/{id}, which lists the skill's versions.
func (a *evalAPI) detail(ctx context.Context, out any) ([]byte, error) {
	ctx, cancel, err := requestContext(ctx, controlRequest)
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := a.c.GetContext(ctx, "/api/catalog/"+url.PathEscape(a.skill.ID))
	if err != nil {
		return nil, timeoutCause(ctx, fmt.Errorf("skill versions: %w", err))
	}
	defer resp.Body.Close()
	raw, err := readLimitedResponse(resp.Body, evalMaxResponse)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not read the skill's versions (HTTP %d)", resp.StatusCode)
	}
	return raw, decodeEval(raw, out)
}

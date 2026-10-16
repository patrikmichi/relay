package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

const (
	automationManifestFile = "automation.json"
	maxAutomationManifest  = 1 << 20
)

var (
	stepIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	automationStepKinds = map[string]bool{"tool": true, "agent": true, "skill": true, "code": true, "condition": true, "foreach": true, "approval": true, "notify": true}
	automationTriggers  = map[string]bool{"webhook": true, "poll": true, "schedule": true, "event": true, "manual": true, "trigger_type": true}
	secretParamKey      = regexp.MustCompile(`(?i)(secret|token|password|api[_-]?key)`)
)

// precheckAutomation reports what can be seen without the gateway: the
// manifest version, names, the trigger and step kinds, unique step ids and the
// rules that keep a secret out of a parameter. The gateway checks the rest.
func precheckAutomation(raw []byte) (map[string]any, []string) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, []string{"automation.json is not a JSON object: " + err.Error()}
	}
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if v, _ := m["schema"].(float64); v != 1 {
		bad("schema must be 1")
	}
	if s, _ := m["slug"].(string); !templateSlugPattern.MatchString(s) {
		bad("slug %q must be 3-64 characters of a-z, 0-9 and -", s)
	}
	if s, _ := m["version"].(string); !semverPattern.MatchString(s) {
		bad("version %q must be semver", s)
	}
	if s, _ := m["title"].(string); strings.TrimSpace(s) == "" {
		bad("title is required")
	}
	if tr, ok := m["trigger"].(map[string]any); !ok || !automationTriggers[fmt.Sprint(tr["kind"])] {
		bad("trigger.kind must be one of webhook, poll, schedule, event, manual, trigger_type")
	} else if tr["kind"] == "trigger_type" {
		if t, _ := tr["type"].(string); t == "" {
			bad("a trigger_type trigger needs the type of the trigger, such as github.issue_opened")
		}
		if cfg, present := tr["config"]; present {
			if _, ok := cfg.(map[string]any); !ok {
				bad("trigger.config must be an object")
			}
		}
	}
	steps, _ := m["steps"].([]any)
	if len(steps) == 0 {
		bad("at least one step is required")
	}
	seen := map[string]bool{}
	var walk func(list []any)
	walk = func(list []any) {
		for _, s := range list {
			st, _ := s.(map[string]any)
			id, _ := st["id"].(string)
			kind, _ := st["kind"].(string)
			switch {
			case !stepIDPattern.MatchString(id):
				bad("step id %q must be lower_snake_case", id)
			case seen[id]:
				bad("step id %q is used twice", id)
			}
			seen[id] = true
			if !automationStepKinds[kind] {
				bad("step %q has unknown kind %q", id, kind)
			}
			if kind == "foreach" {
				inner, _ := st["steps"].([]any)
				walk(inner)
			}
		}
	}
	walk(steps)
	params, _ := m["params"].([]any)
	for _, p := range params {
		if pm, ok := p.(map[string]any); ok {
			if k, _ := pm["key"].(string); secretParamKey.MatchString(k) {
				bad("param %q looks like a secret; credentials come from connections, not parameters", k)
			}
		}
	}
	return m, problems
}

func readAutomationDir(dir string) ([]byte, error) {
	path := filepath.Join(dir, automationManifestFile)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", textsafe.Clean(path), err)
	}
	if info.Size() > maxAutomationManifest {
		return nil, fmt.Errorf("%s is larger than %d bytes", textsafe.Clean(path), maxAutomationManifest)
	}
	return os.ReadFile(path)
}

type automationAnalysis struct {
	Errors []struct {
		Path    string `json:"path"`
		Message string `json:"message"`
	} `json:"errors"`
	Analysis struct {
		Permissions []struct {
			Service string `json:"service"`
			Tool    string `json:"tool"`
			Risk    string `json:"risk"`
		} `json:"permissions"`
		MinimumPlan string   `json:"minimumPlan"`
		Preapprove  []string `json:"preapprovableSteps"`
	} `json:"analysis"`
}

func automationValidateCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "validate <dir>",
		Short: "Check an automation.json locally and with the gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := validateAutomation(cmd, args[0], f)
			return err
		},
	}
	f.bind(cmd, "Print the gateway's analysis unchanged")
	return cmd
}

func validateAutomation(cmd *cobra.Command, dir string, f approvalFlags) (map[string]any, error) {
	raw, err := readAutomationDir(dir)
	if err != nil {
		return nil, err
	}
	m, problems := precheckAutomation(raw)
	if len(problems) > 0 {
		return nil, errors.New("automation.json is not valid:\n  " + strings.Join(problems, "\n  "))
	}
	c, err := approvalsClient(f.gatewayURL)
	if err != nil {
		return nil, err
	}
	var res automationAnalysis
	body, err := gatewayJSON(cmd.Context(), c, http.MethodPost, "/api/automations/templates/validate", map[string]any{"manifest": m}, &res)
	if err != nil {
		return nil, err
	}
	out := cmd.OutOrStdout()
	if len(res.Errors) > 0 {
		var lines []string
		for _, e := range res.Errors {
			lines = append(lines, textsafe.Clean(e.Path)+": "+textsafe.Clean(e.Message))
		}
		return nil, errors.New("the gateway rejected automation.json:\n  " + strings.Join(lines, "\n  "))
	}
	if f.jsonOut {
		printRaw(out, body)
		return m, nil
	}
	fmt.Fprintf(out, "%s %s is valid.\n", textsafe.Clean(fmt.Sprint(m["slug"])), textsafe.Clean(fmt.Sprint(m["version"])))
	for _, p := range res.Analysis.Permissions {
		fmt.Fprintf(out, "  %-12s %s.%s\n", textsafe.Clean(p.Risk), textsafe.Clean(p.Service), textsafe.Clean(p.Tool))
	}
	if res.Analysis.MinimumPlan != "" {
		fmt.Fprintf(out, "  needs the %s plan or higher\n", textsafe.Clean(res.Analysis.MinimumPlan))
	}
	if len(res.Analysis.Preapprove) > 0 {
		fmt.Fprintf(out, "  steps that can be pre-approved: %s\n", textsafe.Clean(strings.Join(res.Analysis.Preapprove, ", ")))
	}
	return m, nil
}

func automationPublishCmd() *cobra.Command {
	var f approvalFlags
	var channel string
	cmd := &cobra.Command{
		Use:   "publish <dir>",
		Short: "Publish an automation template for your organisation",
		Long: `Validates the directory's automation.json and uploads it as a private template.
A template that pre-approves an outward step always waits for an administrator's review.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentPack("zuano automation publish"); err != nil {
				return err
			}
			if channel != "stable" && channel != "beta" {
				return fmt.Errorf("invalid --channel %q: use stable or beta", textsafe.Clean(channel))
			}
			m, err := validateAutomation(cmd, args[0], approvalFlags{gatewayURL: f.gatewayURL})
			if err != nil {
				return err
			}
			manifest, err := readAutomationDir(args[0])
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="manifest"; filename="`+automationManifestFile+`"`)
			h.Set("Content-Type", "application/json")
			part, err := mw.CreatePart(h)
			if err != nil {
				return err
			}
			if _, err := part.Write(manifest); err != nil {
				return err
			}
			fields := map[string]string{"type": "automation", "channel": channel, "slug": fmt.Sprint(m["slug"])}
			for k, v := range fields {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			if err := mw.Close(); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			ctx, cancel, err := requestContext(cmd.Context(), controlRequest)
			if err != nil {
				return err
			}
			defer cancel()
			resp, err := c.PostContext(ctx, publishAPIPath, mw.FormDataContentType(), &buf)
			if err != nil {
				return fmt.Errorf("POST %s: %w", publishAPIPath, err)
			}
			defer resp.Body.Close()
			body, err := readLimitedResponse(resp.Body, maxResponseBytes)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}
			w := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(w, body)
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
			return handlePublishResponse(resp.StatusCode, resp.Header, body, c.GatewayURL, w)
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&channel, "channel", "stable", "Release channel: stable or beta")
	return cmd
}

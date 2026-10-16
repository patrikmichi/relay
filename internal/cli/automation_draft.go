package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// maxDraftManifest bounds a manifest read from a file.
const maxDraftManifest = 1 << 20

type manifestIssue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type draftAnalysis struct {
	Trigger            any      `json:"trigger"`
	Steps              []any    `json:"steps"`
	DerivedPermissions []string `json:"derivedPermissions"`
	MaxRisk            string   `json:"maxRisk"`
	PreapprovableSteps []string `json:"preapprovableSteps"`
	MissingConnections []string `json:"missingConnections"`
	MinimumPlan        string   `json:"minimumPlan"`
}

func automationDraftCmd() *cobra.Command {
	var f planFlags
	var manifestFile, name, draftID string
	var binds []string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "draft [goal]",
		Short: "Save an automation you describe, or write as a manifest, as a private draft",
		Long: `Creates a private draft automation from a goal in words, from a manifest file
(--manifest), or both. With a goal alone the gateway writes the manifest with
your model key or its platform model; with a manifest it only checks it. The
draft does nothing: a person opens the page this prints, sees the diagram, the
permissions it needs and a dry run, and turns it on with a passkey. A draft
never gets standing permissions or pre-approvals from this command, so an
agent session may use it (as any write, it can be held for approval).

A manifest that is not valid prints every issue on its own line and exits 16.
--automation-id replaces the manifest of a draft you already made.`,
		Example: `  zuano automation draft "When a GitHub issue is opened in acme/web, label it and post to #triage"
  zuano automation draft --manifest triage.json --name "Issue triage" --param channel=C0123`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if len(args) == 1 {
				if strings.TrimSpace(args[0]) == "" {
					return errors.New("the goal is empty")
				}
				body["goal"] = args[0]
			}
			if manifestFile != "" {
				m, err := readDraftManifest(manifestFile)
				if err != nil {
					return err
				}
				body["manifest"] = m
			}
			if len(body) == 0 {
				return errors.New("describe the automation in words, or give a manifest with --manifest")
			}
			if name != "" {
				body["name"] = name
			}
			if draftID != "" {
				id, err := automationID(draftID)
				if err != nil {
					return err
				}
				body["automationId"] = id
			}
			params, err := parseParams(f.params, f.paramsFile)
			if err != nil {
				return err
			}
			if len(params) > 0 {
				body["params"] = params
			}
			if len(binds) > 0 {
				b, err := parseBindings(binds)
				if err != nil {
					return err
				}
				body["bindings"] = b
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			status, raw, err := approvalsDo(cmd.Context(), c, controlRequest, http.MethodPost, "/api/automations/draft", body)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if status == http.StatusUnprocessableEntity {
				if issues := manifestIssues(raw); issues != nil {
					if f.jsonOut {
						printRaw(out, raw)
					} else {
						printManifestIssues(cmd.ErrOrStderr(), issues)
					}
					return &ExitError{Code: exitInvalidManifest, Msg: fmt.Sprintf("the manifest has %d issue(s); nothing was saved", len(issues))}
				}
			}
			if held := pendingFromREST(status, raw); held != nil {
				if f.jsonOut {
					printRaw(out, raw)
				}
				return handlePending(cmd.Context(), c, held, f.jsonOut, callOpts{}, out)
			}
			if status < 200 || status > 299 {
				if refusal := policyRefusal(raw, nil); refusal != nil {
					return refusal
				}
				return gatewayStatusError(status, raw)
			}
			var res struct {
				AutomationID     string        `json:"automationId"`
				CustomManifestID string        `json:"customManifestId"`
				WizardURL        string        `json:"wizardUrl"`
				Analysis         draftAnalysis `json:"analysis"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode the draft: %w", err)
			}
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if res.AutomationID == "" || res.WizardURL == "" {
				return errors.New("the gateway did not return the draft or its page")
			}
			fmt.Fprintf(out, "Saved draft %s.\n", textsafe.Clean(res.AutomationID))
			printDraftAnalysis(out, res.Analysis)
			link, err := gatewayLink(c.GatewayURL, res.WizardURL)
			if err != nil {
				return err
			}
			showLink(cmd, link, noBrowser, "Test it and turn it on there")
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&manifestFile, "manifest", "", "JSON manifest file to validate and save")
	cmd.Flags().StringVar(&name, "name", "", "Name of the draft")
	cmd.Flags().StringVar(&draftID, "automation-id", "", "Replace the manifest of this draft (atm_...)")
	cmd.Flags().StringArrayVar(&binds, "bind", nil, "service=org|personal: whose connection a service uses; repeatable")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}

func parseBindings(binds []string) (map[string]string, error) {
	out := map[string]string{}
	for _, b := range binds {
		svc, scope, ok := strings.Cut(b, "=")
		if !ok || svc == "" || (scope != "org" && scope != "personal") {
			return nil, fmt.Errorf("--bind %q must be service=org or service=personal", textsafe.Clean(b))
		}
		out[svc] = scope
	}
	return out, nil
}

func readDraftManifest(path string) (map[string]any, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, maxDraftManifest+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDraftManifest {
		return nil, fmt.Errorf("%s is larger than %d bytes", textsafe.Clean(path), maxDraftManifest)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("%s is not a JSON object: a manifest is one object with a trigger and steps", textsafe.Clean(path))
	}
	return m, nil
}

// manifestIssues reads the issues of an invalid_manifest refusal; nil when the
// body is another 422.
func manifestIssues(raw []byte) []manifestIssue {
	var doc struct {
		Error  string          `json:"error"`
		Issues []manifestIssue `json:"issues"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Error != "invalid_manifest" {
		return nil
	}
	if doc.Issues == nil {
		return []manifestIssue{}
	}
	return doc.Issues
}

func printManifestIssues(w io.Writer, issues []manifestIssue) {
	for _, i := range issues {
		where := textsafe.Clean(i.Path)
		if where == "" {
			where = "manifest"
		}
		line := fmt.Sprintf("%s: %s", where, textsafe.Clean(i.Message))
		if i.Code != "" {
			line += " (" + textsafe.Clean(i.Code) + ")"
		}
		fmt.Fprintln(w, line)
	}
}

func printDraftAnalysis(w io.Writer, a draftAnalysis) {
	fmt.Fprintf(w, "  %d step(s), highest risk %s\n", len(a.Steps), orDash(a.MaxRisk))
	if len(a.DerivedPermissions) > 0 {
		fmt.Fprintf(w, "  needs: %s\n", textsafe.Clean(strings.Join(a.DerivedPermissions, ", ")))
	}
	if len(a.PreapprovableSteps) > 0 {
		fmt.Fprintf(w, "  steps that can be pre-approved: %s\n", textsafe.Clean(strings.Join(a.PreapprovableSteps, ", ")))
	}
	if len(a.MissingConnections) > 0 {
		fmt.Fprintf(w, "  connect first: %s (see `%s`)\n", textsafe.Clean(strings.Join(a.MissingConnections, ", ")), brand.Cmd("connect <service>"))
	}
	if a.MinimumPlan != "" {
		fmt.Fprintf(w, "  plan: %s or above\n", textsafe.Clean(a.MinimumPlan))
	}
}

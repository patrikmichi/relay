package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/patrikmichi/zuano/internal/agentport"
	"github.com/patrikmichi/zuano/internal/agentport/txn"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/dpop"
	"github.com/patrikmichi/zuano/internal/keychain"
	"github.com/spf13/cobra"
)

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func DoctorCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use: "doctor", Short: "Check provider directories, installed files, recovery state and gateway authentication", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := doctorChecks(cmd.Context())
			healthy := true
			for _, check := range checks {
				if !check.OK {
					healthy = false
				}
			}
			if jsonOut {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Healthy bool          `json:"healthy"`
					Checks  []doctorCheck `json:"checks"`
				}{healthy, checks}); err != nil {
					return err
				}
			} else {
				for _, check := range checks {
					status := "ok"
					if !check.OK {
						status = "fail"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %s: %s\n", status, check.Name, check.Detail)
				}
			}
			if !healthy {
				return fmt.Errorf("doctor found failing checks")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print structured diagnostic results")
	return cmd
}

func doctorChecks(ctx context.Context) []doctorCheck {
	checks := []doctorCheck{}
	add := func(name string, ok bool, detail string) { checks = append(checks, doctorCheck{name, ok, detail}) }
	for _, diagnostic := range agentport.OverrideDiagnostics() {
		ok := diagnostic.Status == "loaded" || diagnostic.Status == "project overrides disabled"
		add("override:"+diagnostic.Path, ok, diagnostic.Status)
	}
	for _, adapter := range agentport.AllAdapters() {
		detected := "not detected"
		if adapter.Detect() {
			detected = "detected"
		}
		add("provider:"+string(adapter.ID()), true, detected)
		for _, dir := range append(adapter.UserDirs()[:adapter.OwnUserDirCount()], adapter.ProjectDirs()[:adapter.OwnProjectDirCount()]...) {
			add("directory:"+string(adapter.ID())+":"+dir, doctorDirectoryWritable(dir), "directory or nearest parent must have write permission bits")
		}
	}
	for _, adapter := range agentport.AllAgentAdapters() {
		for _, dir := range append(adapter.UserDirs()[:adapter.OwnUserDirCount()], adapter.ProjectDirs()[:adapter.OwnProjectDirCount()]...) {
			add("agent-directory:"+string(adapter.ID())+":"+dir, doctorDirectoryWritable(dir), "directory or nearest parent must have write permission bits")
		}
	}
	manifest, err := agentport.LoadManifest()
	if err != nil {
		add("manifest", false, "manifest cannot be read or validated")
	} else {
		results := agentport.InspectManifest(manifest)
		add("manifest", true, fmt.Sprintf("%d current installations", len(results)))
		for _, result := range results {
			detail := "hashes match"
			if result.Reason != "" {
				detail = result.Reason
			}
			add("installation:"+result.ID, result.OK, detail)
		}
	}
	journals, err := txn.List()
	if err != nil {
		add("recovery", false, "transaction journals cannot be read")
	} else {
		pending := 0
		for _, journal := range journals {
			if journal.State != txn.StateCommitted && journal.State != txn.StateRestored {
				pending++
			}
		}
		add("recovery", pending == 0, fmt.Sprintf("%d transactions need recovery", pending))
	}
	doctorSyncChecks(add)
	doctorTranscripts(add)
	pluginLedger := doctorPlugins(add)
	if email, err := config.ResolveEmail(); err == nil && keychain.HasUnboundToken(email) {
		add("stray-session", false, "an email-only keychain entry exists that no gateway session can use; `"+brand.Cmd("login")+"` removes it")
	}
	gateway, err := config.GatewayURL()
	if err != nil {
		add("gateway", false, "gateway configuration is invalid")
		return checks
	}
	if gateway == "" {
		add("gateway", true, "no gateway configured; local checks only")
		return checks
	}
	if Offline() {
		add("gateway", true, "offline mode; gateway and token checks skipped")
		return checks
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	doctorHealth(ctx, gateway, add)
	c, err := client.Resolve(gateway)
	if err != nil {
		add("gateway", false, "authentication unavailable; run zuano login")
		return checks
	}
	response, err := c.GetContext(ctx, "/api/cli/whoami")
	if err != nil {
		add("gateway", false, "gateway request failed")
		return checks
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		add("gateway", false, fmt.Sprintf("authentication probe returned HTTP %d", response.StatusCode))
		return checks
	}
	body, err := readLimitedResponse(response.Body, 1<<20)
	var identity struct {
		Email      string `json:"email"`
		StrictMode bool   `json:"strict_mode"`
	}
	if err != nil || json.Unmarshal(body, &identity) != nil || identity.Email == "" {
		add("gateway", false, "invalid authentication probe response")
		return checks
	}
	add("gateway", true, "gateway reachable and session authenticated")
	doctorPluginFeed(ctx, c, pluginLedger, add)
	doctorPacks(ctx, c, add)
	doctorAutomations(ctx, c, add)
	doctorChat(ctx, c, add)
	doctorPasskeys(ctx, c, add)
	if os.Getenv("ZUANO_API_KEY") != "" {
		add("token", true, "bearer token accepted; expiry not supplied by gateway")
		return checks
	}
	profile := client.ActiveProfile()
	email, err := config.ResolveEmail()
	if profile == keychain.ProfileAgent {
		email, err = config.ResolveAgentEmail()
	}
	if err != nil {
		add("token", false, "session identity unavailable")
		return checks
	}
	token, err := keychain.ReadProfileToken(profile, gateway, email)
	if err != nil {
		add("token", false, "keychain session unavailable")
		return checks
	}
	add("token", token.ExpiresAt > time.Now().Unix(), fmt.Sprintf("access token expires at Unix time %d", token.ExpiresAt))
	doctorDeviceKey(identity.StrictMode, token, add)
	return checks
}

// doctorPasskeys reports how many passkeys the person enrolled. Only someone
// who approves calls needs one, so a missing passkey never fails the check.
func doctorPasskeys(ctx context.Context, c *client.Client, add func(string, bool, string)) {
	var list struct {
		Passkeys []json.RawMessage `json:"passkeys"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, "/api/me/passkeys", nil, &list); err != nil {
		add("passkeys", true, "could not read the passkey list (browser sessions only on some gateways)")
		return
	}
	if len(list.Passkeys) == 0 {
		add("passkeys", true, "none enrolled; add one under Settings, Security in the dashboard to approve calls")
		return
	}
	add("passkeys", true, fmt.Sprintf("%d enrolled", len(list.Passkeys)))
}

// doctorDeviceKey checks the key a strict organisation binds the session to.
func doctorDeviceKey(strict bool, token keychain.TokenData, add func(string, bool, string)) {
	switch {
	case token.DPoPKey != "":
		key, err := dpop.Load(token.DPoPKey)
		if err != nil {
			add("device-key", false, err.Error())
			return
		}
		kind := "stored in a file"
		if key.Hardware() {
			kind = "hardware-backed (" + key.Backend() + ")"
		}
		add("device-key", true, "session is bound to a device key, "+kind)
	case strict:
		add("device-key", false, "the organisation requires a device-bound session and this one is not; run `"+brand.Cmd("login")+"` again")
	default:
		add("device-key", true, "not required: the organisation does not use strict mode")
	}
}

// Checks filesystem permissions without creating directories or probe files.
func doctorDirectoryWritable(dir string) bool {
	for {
		info, err := os.Lstat(dir)
		if err == nil {
			return info.IsDir() && info.Mode().Perm()&0222 != 0
		}
		if !os.IsNotExist(err) {
			return false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

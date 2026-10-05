package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/patrikmichi/relay/internal/agentport"
	"github.com/patrikmichi/relay/internal/agentport/txn"
	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
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
	c, err := client.Resolve(gateway)
	if err != nil {
		add("gateway", false, "authentication unavailable; run relay login")
		return checks
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
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
		Email string `json:"email"`
	}
	if err != nil || json.Unmarshal(body, &identity) != nil || identity.Email == "" {
		add("gateway", false, "invalid authentication probe response")
		return checks
	}
	add("gateway", true, "gateway reachable and session authenticated")
	if os.Getenv("GATEWAY_API_KEY") != "" {
		add("token", true, "bearer token accepted; expiry not supplied by gateway")
		return checks
	}
	email, err := config.ResolveEmail()
	if err != nil {
		add("token", false, "session identity unavailable")
		return checks
	}
	token, err := keychain.ReadToken(gateway, email)
	if err != nil {
		add("token", false, "keychain session unavailable")
		return checks
	}
	add("token", token.ExpiresAt > time.Now().Unix(), fmt.Sprintf("access token expires at Unix time %d", token.ExpiresAt))
	return checks
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

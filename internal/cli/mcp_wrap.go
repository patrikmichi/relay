package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/keychain"
	"github.com/patrikmichi/zuano/internal/mcpwrap"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const childGrace = 5 * time.Second

// gatewayWrapAPI lets the online governor use the CLI's gateway client.
type gatewayWrapAPI struct {
	c *client.Client
}

func (a gatewayWrapAPI) Exchange(ctx context.Context, method, path string, body any) (mcpwrap.Reply, error) {
	r, err := gatewayExchange(ctx, a.c, method, path, body, nil)
	return mcpwrap.Reply{Status: r.Status, Header: r.Header, Body: r.Body}, err
}

func mcpDirs() (pins mcpwrap.PinStore, audit, policy string, err error) {
	dir, err := config.Dir()
	if err != nil {
		return mcpwrap.PinStore{}, "", "", err
	}
	return mcpwrap.PinStore{Dir: filepath.Join(dir, "mcp-pins")}, filepath.Join(dir, "mcp-audit"), filepath.Join(dir, "firewall.yaml"), nil
}

// packageSpec names the package a launcher such as npx or uvx runs, which is
// what identifies a server across machines.
func packageSpec(command []string) string {
	if len(command) == 0 {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(command[0]), ".exe")
	launcher := map[string]bool{"npx": true, "bunx": true, "uvx": true, "pipx": true, "pnpx": true}
	rest := command[1:]
	if base == "pnpm" || base == "yarn" || base == "bun" {
		if len(rest) == 0 || (rest[0] != "dlx" && rest[0] != "x") {
			return ""
		}
		rest = rest[1:]
	} else if !launcher[base] {
		return ""
	}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--from" && i+1 < len(rest) {
			return rest[i+1]
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func commandFingerprint(command []string) string {
	sum := sha256.Sum256([]byte(strings.Join(command, "\x00")))
	return hex.EncodeToString(sum[:])
}

// McpWrapCmd returns `zuano mcp wrap`, which stands between an agent and a
// local MCP server.
func McpWrapCmd() *cobra.Command {
	var (
		upstream, screen, gatewayURL, name string
		offline                            bool
		envKeychain                        []string
	)
	cmd := &cobra.Command{
		Use:   "wrap [flags] -- <command> [args...]",
		Short: "Run a local MCP server behind the firewall",
		Long: `Run an MCP server as the agent's child and pass its messages through the
firewall. Agents start this command instead of the server, which 'zuano sync
--firewall' sets up.

The tools the agent sees are the definitions your organisation approved. A
tool the server added or changed is hidden until it is reviewed. Each call is
checked before it reaches the server and each result before it reaches the
agent; a call that needs a person is held, and the person decides in the
browser with a passkey.

--offline (or no gateway, or ` + "`--offline`" + ` on zuano itself) uses local pins,
firewall.yaml and a local audit log instead: approve tools with
'zuano mcp pins approve'. The server gets your environment without ZUANO_*
variables, plus the values named by --env-from-keychain.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 {
				return errors.New("put the server command after --, for example: zuano mcp wrap --upstream files -- npx -y @acme/files-mcp")
			}
			if screen != "local" && screen != "gateway" {
				return fmt.Errorf("invalid --screen %q: use local or gateway", screen)
			}
			return runMcpWrap(cmd, args, wrapOptions{upstream, screen, gatewayURL, name, offline || Offline(), envKeychain})
		},
	}
	f := cmd.Flags()
	f.StringVar(&upstream, "upstream", "", "Name of the server in the gateway's firewall (default: the program name)")
	f.BoolVar(&offline, "offline", false, "Use local pins, firewall.yaml and a local audit log; contact no gateway")
	f.StringVar(&screen, "screen", "gateway", "gateway sends arguments and results to the gateway for screening; local sends only digests and local findings")
	f.StringSliceVar(&envKeychain, "env-from-keychain", nil, "Environment variables to read from the system keychain for the server")
	f.StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	f.StringVar(&name, "name", "", "Name of the local pins (default: --upstream, then the program name)")
	return cmd
}

type wrapOptions struct {
	upstream, screen, gatewayURL, name string
	offline                            bool
	envKeychain                        []string
}

func pinName(o wrapOptions, command []string) string {
	for _, n := range []string{o.name, o.upstream} {
		if mcpwrap.ValidName(n) {
			return n
		}
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(command[0]), ".exe"))
	if mcpwrap.ValidName(base) {
		return base
	}
	return "server"
}

func serverEnv(server string, names []string) ([]string, error) {
	extra := map[string]string{}
	for _, n := range names {
		v, err := keychain.ReadSecret(server, n)
		if err != nil {
			return nil, err
		}
		extra[n] = v
	}
	return mcpwrap.Environment(mcpwrap.Environ(), "ZUANO_", extra), nil
}

func runMcpWrap(cmd *cobra.Command, command []string, o wrapOptions) error {
	stderr := cmd.ErrOrStderr()
	logf := func(format string, args ...any) {
		fmt.Fprintf(stderr, brand.Name+": "+format+"\n", args...)
	}
	pins, auditDir, policyFile, err := mcpDirs()
	if err != nil {
		return err
	}
	name := pinName(o, command)
	var gov mcpwrap.Governor
	if o.offline {
		policy, err := mcpwrap.LoadPolicy(policyFile)
		if err != nil {
			return err
		}
		audit, err := mcpwrap.OpenAudit(auditDir, name)
		if err != nil {
			return err
		}
		gov = &mcpwrap.OfflineGovernor{Name: name, Pins: pins, Policy: policy.For(name), Audit: audit, Logf: logf}
	} else {
		c, err := approvalsClient(o.gatewayURL)
		if err != nil {
			return fmt.Errorf("%w; use --offline to run without a gateway", err)
		}
		installID, _ := config.InstallID()
		gov = &mcpwrap.OnlineGovernor{
			API: gatewayWrapAPI{c}, Screen: o.screen, Logf: logf,
			Verify:      func(r mcpwrap.Reply) error { return gatewayReply{r.Status, r.Header, r.Body}.verify(cmd.Context(), c) },
			Fingerprint: commandFingerprint(command), PackageSpec: packageSpec(command), InstallID: installID,
			AgentID: os.Getenv("ZUANO_AGENT_ID"), Upstream: o.upstream,
		}
	}
	env, err := serverEnv(name, o.envKeychain)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	child, err := mcpwrap.Start(ctx, command[0], command[1:], env, stderr)
	if err != nil {
		return fmt.Errorf("start the server: %w", err)
	}
	defer child.Stop(childGrace)
	shim := &mcpwrap.Shim{Gov: gov, Agent: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Child: child.Stdout, ChildW: child.Stdin, Logf: logf}
	err = shim.Run(ctx)
	var refusal *mcpwrap.Refusal
	if errors.As(err, &refusal) {
		code := exitDenied
		if refusal.Reason == "upstream_quarantined" {
			code = exitQuarantined
		}
		return &ExitError{Code: code, Msg: textsafe.Clean(refusal.Message)}
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

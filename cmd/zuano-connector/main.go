// Command zuano-connector is the agent that runs inside a customer's network:
// it keeps one outbound connection to the broker and serves the HTTP and MCP
// targets its local config names.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/patrikmichi/zuano/internal/tunnel"
)

// Set at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = tunnel.ExitConfig
	exitRevoked = tunnel.ExitRevoked
)

func main() {
	tunnel.Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `Usage: ` + tunnel.BinaryName + ` <command> [flags]

Commands:
  enroll   --token nxt_... --url https://app.example   enroll this agent in a network
  run      [--health-addr 127.0.0.1:9464]               connect to the broker and serve requests
  status   [--json]                                      show the running agent's state
  check    [--print] [--hints]                           validate the config offline
  rotate                                                 rotate the agent's keys now
  pause | resume                                         refuse every request, or serve again
  update   [--check] [--allow-downgrade]                 update this binary
  version                                                print the version

Exit status: 0 ok, 1 failure, 2 invalid config or usage, 3 the agent was revoked.
`

type common struct {
	config   string
	stateDir string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.config, "config", tunnel.DefaultConfigPath(), "path of the config file")
	fs.StringVar(&c.stateDir, "state-dir", "", "directory of identity.json and the control socket (default: state_dir of the config)")
}

// resolveStateDir finds the state directory: the flag, the config, the default.
func (c *common) resolveStateDir() string {
	if c.stateDir != "" {
		return c.stateDir
	}
	if cfg, err := tunnel.LoadConfig(c.config); err == nil {
		return cfg.StateDir
	}
	return tunnel.DefaultStateDir()
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitConfig
	}
	cmd, rest := args[0], args[1:]
	var code int
	switch cmd {
	case "enroll":
		code = cmdEnroll(ctx, rest, stdout, stderr)
	case "run":
		code = cmdRun(ctx, rest, stdout, stderr)
	case "status":
		code = cmdStatus(rest, stdout, stderr)
	case "check":
		code = cmdCheck(ctx, rest, stdout, stderr)
	case "rotate":
		code = cmdRotate(ctx, rest, stdout, stderr)
	case "pause", "resume":
		code = cmdPause(cmd, rest, stdout, stderr)
	case "update":
		code = cmdUpdate(ctx, rest, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "%s %s (protocol %d, commit %s, built %s, %s/%s)\n", tunnel.BinaryName, version, tunnel.ProtoVersion, commit, date, runtime.GOOS, runtime.GOARCH)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		code = exitConfig
	}
	return code
}

// exitFor maps an error to the exit status.
func exitFor(err error) int {
	var ce *tunnel.ErrConfig
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ce), errors.Is(err, tunnel.ErrNotEnrolled):
		return exitConfig
	case errors.Is(err, tunnel.ErrRevoked):
		return exitRevoked
	}
	return exitFailure
}

func parse(fs *flag.FlagSet, args []string, stderr io.Writer) (ok bool, code int) {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, exitOK
		}
		return false, exitConfig
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return false, exitConfig
	}
	return true, 0
}

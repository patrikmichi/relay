package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/patrikmichi/zuano/internal/tunnel"
)

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// controlRequest talks to a running agent; tests replace it.
var controlRequest = tunnel.ControlRequest

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "error: %v\n", err)
	return exitFor(err)
}

func cmdEnroll(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	c.bind(fs)
	token := fs.String("token", os.Getenv("ZUANO_CONNECTOR_ENROLL_TOKEN"), "enrollment token (nxt_...); or ZUANO_CONNECTOR_ENROLL_TOKEN")
	appURL := fs.String("url", firstEnv("ZUANO_CONNECTOR_URL", "ZUANO_URL"), "address of the gateway; or ZUANO_CONNECTOR_URL")
	name := fs.String("name", "", "label shown in the dashboard (default: the host name)")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	if *token == "" || *appURL == "" {
		fmt.Fprintln(stderr, "enroll needs --token and --url")
		return exitConfig
	}
	state := c.resolveStateDir()
	if _, err := tunnel.LoadIdentity(state); err == nil {
		fmt.Fprintf(stderr, "this agent is already enrolled (%s); remove its identity.json to enroll again\n", filepath.Join(state, tunnel.IdentityFile))
		return exitFailure
	}
	id, err := tunnel.Enroll(ctx, tunnel.EnrollOptions{AppURL: *appURL, Token: *token, Name: *name, StateDir: state, ConfigPath: c.config})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Enrolled as %s in network %s.\nIdentity: %s\nConfig:   %s\n", id.AgentID, id.NetworkID, filepath.Join(state, tunnel.IdentityFile), c.config)
	fmt.Fprintf(stdout, "List the targets the gateway may reach in the config, check it with `%s check`, then start the agent with `%s run`.\n", tunnel.BinaryName, tunnel.BinaryName)
	fmt.Fprintln(stdout, "If your network requires approval, an administrator approves this agent in the dashboard before it receives requests.")
	return exitOK
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	c.bind(fs)
	healthAddr := fs.String("health-addr", "", "serve /healthz, /readyz and /metrics here (default: health.listen of the config)")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	// Background goroutines write warnings next to the main flow.
	stderr = &lockedWriter{w: stderr}
	exe, _ := os.Executable()
	opts := tunnel.AgentOptions{ConfigPath: c.config, StateDir: c.stateDir, Stdout: stdout, Executable: exe}
	// A container started with a token enrolls itself on first start.
	if token, appURL := os.Getenv("ZUANO_CONNECTOR_ENROLL_TOKEN"), firstEnv("ZUANO_CONNECTOR_URL", "ZUANO_URL"); token != "" && appURL != "" {
		state := c.resolveStateDir()
		if _, ierr := tunnel.LoadIdentity(state); errors.Is(ierr, tunnel.ErrNotEnrolled) {
			if _, eerr := tunnel.Enroll(ctx, tunnel.EnrollOptions{AppURL: appURL, Token: token, StateDir: state, ConfigPath: c.config}); eerr != nil {
				return fail(stderr, eerr)
			}
		}
	}
	a, err := tunnel.NewAgent(opts)
	if err != nil {
		return fail(stderr, err)
	}
	defer a.Close()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case <-hup:
				a.Reload()
			case <-rctx.Done():
				return
			}
		}
	}()
	go func() {
		if cerr := a.ServeControl(rctx); cerr != nil {
			fmt.Fprintf(stderr, "warning: the control socket is not available: %v\n", cerr)
		}
	}()
	addr := *healthAddr
	if addr == "" {
		addr = a.HealthAddr()
	}
	if addr != "" {
		go func() {
			if herr := a.ServeHealth(rctx, addr); herr != nil {
				fmt.Fprintf(stderr, "warning: the health endpoint is not available: %v\n", herr)
			}
		}()
	}
	err = a.Run(rctx)
	if errors.Is(err, tunnel.ErrRestart) {
		a.Close()
		if rerr := reexec(exe, os.Args); rerr != nil {
			return fail(stderr, rerr)
		}
		return exitOK
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	c.bind(fs)
	asJSON := fs.Bool("json", false, "print the status as JSON")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	st, err := controlRequest(c.resolveStateDir(), "status")
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		return printJSON(stdout, st)
	}
	fmt.Fprintf(stdout, "State:      %s\nAgent:      %s\nNetwork:    %s\nBroker:     %s\nVersion:    %s (protocol %d, channel %s)\nAllowlist:  %s (%d targets)\nIn flight:  %d\nUptime:     %s\nKey age:    %d days\n",
		st.State, st.AgentID, st.NetworkID, st.Broker, st.Version, st.Proto, st.UpdateChannel, st.AllowlistHash, st.Targets, st.InFlight, (time.Duration(st.UptimeSeconds) * time.Second).String(), st.KeyAgeDays)
	if st.Paused {
		fmt.Fprintln(stdout, "Paused:     yes: every request is refused")
	}
	if st.LastError != "" {
		fmt.Fprintf(stdout, "Last error: %s\n", st.LastError)
	}
	return exitOK
}

func printJSON(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return exitFailure
	}
	return exitOK
}

func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	c.bind(fs)
	print := fs.Bool("print", false, "print the config with literal secrets hidden")
	hints := fs.Bool("hints", false, "also ask the gateway which targets its bindings use (the only part that contacts it)")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	cfg, err := tunnel.LoadConfig(c.config)
	if err != nil {
		return fail(stderr, err)
	}
	if *print {
		out, perr := cfg.PrintConfig()
		if perr != nil {
			return fail(stderr, perr)
		}
		_, _ = stdout.Write(out)
		return exitOK
	}
	findings := tunnel.CheckTargets(ctx, cfg, net.DefaultResolver)
	if *hints {
		state := c.stateDir
		if state == "" {
			state = cfg.StateDir
		}
		id, ierr := tunnel.LoadIdentity(state)
		if ierr != nil {
			return fail(stderr, ierr)
		}
		names, herr := tunnel.ConfigHints(ctx, &http.Client{Timeout: 15 * time.Second}, id, time.Now())
		if herr != nil {
			return fail(stderr, herr)
		}
		for _, n := range tunnel.UnservedHints(cfg, names) {
			findings = append(findings, tunnel.Finding{Target: n, Level: "warn", Msg: "a binding uses this target and this config has no target of that name"})
		}
	}
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
	}
	if tunnel.HasErrors(findings) {
		fmt.Fprintf(stdout, "%d target(s) checked: problems found\n", len(cfg.Targets))
		return exitFailure
	}
	fmt.Fprintf(stdout, "%d target(s) checked: ok (allowlist %s)\n", len(cfg.Targets), cfg.Allowlist().Hash)
	return exitOK
}

func cmdRotate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("rotate", flag.ContinueOnError)
	c.bind(fs)
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	state := c.resolveStateDir()
	if _, err := controlRequest(state, "rotate"); err == nil {
		fmt.Fprintln(stdout, "The running agent was asked to rotate its keys.")
		return exitOK
	}
	id, err := tunnel.LoadIdentity(state)
	if err != nil {
		return fail(stderr, err)
	}
	if err := tunnel.RotateKeys(ctx, &http.Client{Timeout: 30 * time.Second}, id, state, time.Now()); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Rotated: the new key is %s; the old one still works for 24 hours.\n", id.Current().KID())
	return exitOK
}

func cmdPause(cmd string, args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	c.bind(fs)
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	st, err := controlRequest(c.resolveStateDir(), cmd)
	if err != nil {
		return fail(stderr, err)
	}
	if st.Paused {
		fmt.Fprintln(stdout, "Paused: every request is refused with agent_paused; the agent stays connected.")
	} else {
		fmt.Fprintln(stdout, "Resumed.")
	}
	return exitOK
}

func cmdUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	c.bind(fs)
	checkOnly := fs.Bool("check", false, "only say whether a newer version exists")
	downgrade := fs.Bool("allow-downgrade", false, "install the channel's version even when it is older")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	if tunnel.InContainer() {
		fmt.Fprintln(stderr, "this is a container: its image is immutable. Update by pulling the new image tag or with `helm upgrade`.")
		return exitFailure
	}
	cfg, err := tunnel.LoadConfig(c.config)
	if err != nil {
		return fail(stderr, err)
	}
	url := cfg.Update.URL
	if url == "" {
		url = os.Getenv("ZUANO_CONNECTOR_RELEASES_URL")
	}
	if url == "" {
		fmt.Fprintln(stderr, "no releases address: set update.url in the config or ZUANO_CONNECTOR_RELEASES_URL")
		return exitConfig
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	info, err := tunnel.CheckUpdate(ctx, client, url, cfg.Update.Channel, tunnel.Version, tunnel.UpdateKeys(), *downgrade)
	if err != nil {
		return fail(stderr, err)
	}
	if info == nil {
		fmt.Fprintf(stdout, "%s is up to date.\n", tunnel.Version)
		return exitOK
	}
	fmt.Fprintf(stdout, "%s is available (%s update, you have %s).\n", info.Manifest.Version, info.Class, tunnel.Version)
	if *checkOnly {
		return exitOK
	}
	if cfg.Update.Method == "package" {
		fmt.Fprintln(stderr, "update.method is package: update through the package manager")
		return exitFailure
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(stderr, err)
	}
	path, err := tunnel.Download(ctx, client, info.Artifact, filepath.Dir(exe))
	if err != nil {
		return fail(stderr, err)
	}
	if err := tunnel.Swap(exe, path); err != nil {
		_ = os.Remove(path)
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Installed %s; the previous binary is kept as %s. Restart the service to run it (%s).\n", info.Manifest.Version, exe+tunnel.PreviousSuffix, runtime.GOOS)
	return exitOK
}

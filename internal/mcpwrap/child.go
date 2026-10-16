package mcpwrap

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Child is a running server process.
type Child struct {
	cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
}

// Environment is the server's environment: the parent's without zuano's
// session variables, plus extra.
func Environment(parent []string, prefix string, extra map[string]string) []string {
	var out []string
	for _, kv := range parent {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// Start runs command with args, without a shell, in its own process group,
// with its stderr passed to stderr.
func Start(ctx context.Context, command string, args []string, env []string, stderr io.Writer) (*Child, error) {
	if command == "" {
		return nil, errors.New("no server command")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = env
	cmd.Stderr = stderr
	configure(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Child{cmd: cmd, Stdin: in, Stdout: out}, nil
}

// Stop ends the server: it is asked to stop, then killed after grace.
func (c *Child) Stop(grace time.Duration) {
	_ = c.Stdin.Close()
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(grace / 2):
	}
	terminate(c.cmd)
	select {
	case <-done:
	case <-time.After(grace):
		kill(c.cmd)
		<-done
	}
}

// ExitCode is the server's exit status once it has stopped.
func (c *Child) ExitCode() int {
	if c.cmd.ProcessState == nil {
		return 1
	}
	return c.cmd.ProcessState.ExitCode()
}

// Environ is os.Environ, for callers that build the server's environment.
func Environ() []string { return os.Environ() }

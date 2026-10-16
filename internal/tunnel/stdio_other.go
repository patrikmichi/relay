//go:build !unix

package tunnel

import (
	"errors"
	"os/exec"
)

func configureStdioProc(_ *exec.Cmd, runAs string) error {
	if runAs != "" {
		return errors.New("run_as_user is not supported on this system")
	}
	return nil
}

func terminateProc(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killProc(cmd *exec.Cmd) { terminateProc(cmd) }

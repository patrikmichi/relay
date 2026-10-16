//go:build !unix

package mcpwrap

import "os/exec"

func configure(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func kill(cmd *exec.Cmd) { terminate(cmd) }

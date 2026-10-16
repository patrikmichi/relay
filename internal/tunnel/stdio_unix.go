//go:build unix

package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// configureStdioProc puts the process in its own group so it can be stopped
// with everything it started, and runs it as another user when the connector
// itself runs as root.
func configureStdioProc(cmd *exec.Cmd, runAs string) error {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if runAs != "" {
		if os.Geteuid() != 0 {
			return fmt.Errorf("run_as_user needs the connector to run as root")
		}
		u, err := user.Lookup(runAs)
		if err != nil {
			return fmt.Errorf("run_as_user: %w", err)
		}
		uid, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			return err
		}
		gid, err := strconv.ParseUint(u.Gid, 10, 32)
		if err != nil {
			return err
		}
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	cmd.SysProcAttr = attr
	return nil
}

func terminateProc(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func killProc(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

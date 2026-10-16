//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// reexec starts the (updated) binary; the service manager restarts the old
// process's slot, and this one ends.
func reexec(exe string, args []string) error {
	cmd := exec.Command(exe, args[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Start()
}

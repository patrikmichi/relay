//go:build unix

package main

import (
	"os"
	"syscall"
)

// reexec replaces this process with the (updated) binary.
func reexec(exe string, args []string) error {
	return syscall.Exec(exe, args, os.Environ())
}

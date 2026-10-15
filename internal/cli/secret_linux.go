//go:build linux

package cli

import "golang.org/x/sys/unix"

const (
	termiosGet = unix.TCGETS
	termiosSet = unix.TCSETS
)

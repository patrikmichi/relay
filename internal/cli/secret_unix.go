//go:build linux || darwin

package cli

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// readSecret reads one line from the terminal f without echoing it.
func readSecret(f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, termiosGet)
	if err != nil {
		return "", err
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, termiosSet, &quiet); err != nil {
		return "", err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, termiosSet, old) }()
	line, err := bufio.NewReader(f).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

//go:build linux || darwin

package cli

import (
	"bufio"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// restoreOnInterrupt runs restore and exits with status 130 if SIGINT or
// SIGTERM arrives before stop is called: the default handler would end the
// process with the terminal still not echoing what the user types next.
func restoreOnInterrupt(restore func(), exit func(int)) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			restore()
			exit(130)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

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
	restore := func() { _ = unix.IoctlSetTermios(fd, termiosSet, old) }
	defer restore()
	stop := restoreOnInterrupt(restore, os.Exit)
	defer stop()
	line, err := bufio.NewReader(f).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

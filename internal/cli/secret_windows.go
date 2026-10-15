//go:build windows

package cli

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// readSecret reads one line from the console f without echoing it.
func readSecret(f *os.File) (string, error) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return "", err
	}
	if err := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return "", err
	}
	defer func() { _ = windows.SetConsoleMode(h, mode) }()
	line, err := bufio.NewReader(f).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

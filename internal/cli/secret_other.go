//go:build !linux && !darwin && !windows

package cli

import (
	"errors"
	"os"
)

func readSecret(*os.File) (string, error) {
	return "", errors.New("hidden input is not supported on this platform; pass --field KEY=VALUE")
}

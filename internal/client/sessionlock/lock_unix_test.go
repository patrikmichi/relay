//go:build unix

package sessionlock

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIsWouldBlock_OnlyContention(t *testing.T) {
	for _, err := range []error{unix.EWOULDBLOCK, unix.EAGAIN} {
		if !isWouldBlock(err) {
			t.Errorf("%v must be retried", err)
		}
	}
	for _, err := range []error{unix.EBADF, unix.ENOLCK, errors.New("other")} {
		if isWouldBlock(err) {
			t.Errorf("%v must not be retried", err)
		}
	}
}

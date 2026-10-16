//go:build linux

package dpop

import (
	"errors"
	"fmt"
	"os"
)

// tpmDevice is the kernel's TPM resource manager. It gives each process its
// own handle space and flushes it when the file closes.
const tpmDevice = "/dev/tpmrm0"

type deviceTransport struct{ f *os.File }

func openTPM() (tpmTransport, error) {
	f, err := os.OpenFile(tpmDevice, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", tpmDevice, err)
	}
	return &deviceTransport{f: f}, nil
}

func (t *deviceTransport) transmit(command []byte) ([]byte, error) {
	if n, err := t.f.Write(command); err != nil || n != len(command) {
		return nil, fmt.Errorf("send a command to the TPM: %w", errors.Join(err, errShortWrite(n, len(command))))
	}
	resp := make([]byte, 4096)
	n, err := t.f.Read(resp)
	if err != nil {
		return nil, fmt.Errorf("read the TPM response: %w", err)
	}
	return resp[:n], nil
}

func errShortWrite(n, want int) error {
	if n == want {
		return nil
	}
	return fmt.Errorf("wrote %d of %d bytes", n, want)
}

func (t *deviceTransport) close() error { return t.f.Close() }

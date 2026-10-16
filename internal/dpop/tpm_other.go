//go:build !linux && !windows

package dpop

import "errors"

func openTPM() (tpmTransport, error) {
	return nil, errors.New("this platform has no TPM support in relay")
}

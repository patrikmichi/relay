package signing

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
)

// VerifyMessage checks an ed25519 signature over msg against the pinned key
// named kid. A key that is not pinned never verifies anything.
func VerifyMessage(keys []Key, kid string, msg, sig []byte) error {
	if len(keys) == 0 {
		return errors.New("no signing key is pinned for this gateway")
	}
	idx := slices.IndexFunc(keys, func(k Key) bool { return k.KID == kid })
	if idx < 0 {
		return fmt.Errorf("signed with key %q, which is not pinned for this gateway", kid)
	}
	pub, err := decodePublicKey(keys[idx])
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, msg, sig) {
		return errors.New("the signature does not match the content")
	}
	return nil
}

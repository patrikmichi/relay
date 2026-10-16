package auth

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/patrikmichi/zuano/internal/dpop"
	"github.com/patrikmichi/zuano/internal/keychain"
)

// deviceBinding is the device key a login offers the gateway. An organisation
// in strict mode answers a token request that carries a proof with a token
// bound to the key; any other organisation answers with an ordinary bearer
// token and the key is dropped again.
type deviceBinding struct {
	key    dpop.Key
	signer *dpop.Signer
}

// newDeviceBinding makes a device key, in the TPM where there is one. Without
// one it makes a file key and says so only if the session ends up bound to
// it. A failure to make any key is not fatal: only a strict organisation
// needs one, and its gateway refuses the login then.
func newDeviceBinding(diagnostic io.Writer) *deviceBinding {
	key, err := dpop.Create(false)
	if err != nil {
		fmt.Fprintf(diagnostic, "Warning: could not make a device key (%v); an organisation that requires one will refuse this login\n", err)
		return nil
	}
	return &deviceBinding{key: key, signer: dpop.NewSigner(key)}
}

// keyType is what the gateway needs to know to enforce a hardware-key
// requirement: where the private key lives.
func (b *deviceBinding) keyType() string {
	if b.key.Hardware() {
		return "hardware"
	}
	return "software"
}

// sign puts a DPoP proof for req, which carries no access token yet, on it.
func (b *deviceBinding) sign(req *http.Request) error {
	if b == nil {
		return nil
	}
	proof, err := b.signer.Proof(req.Method, req.URL.String(), "")
	if err != nil {
		return err
	}
	req.Header.Set("DPoP", proof)
	return nil
}

// settle keeps the key when the gateway bound the session to it and drops it
// otherwise, and returns the reference to store with the session.
func (b *deviceBinding) settle(tokenType string, diagnostic io.Writer) string {
	if b == nil {
		return ""
	}
	if !strings.EqualFold(tokenType, "DPoP") {
		b.discard()
		return ""
	}
	if b.key.Hardware() {
		fmt.Fprintf(diagnostic, "This session is bound to a hardware key (%s) on this device.\n", b.key.Backend())
	} else {
		fmt.Fprintln(diagnostic, "Warning: this session is bound to a key kept in a file, because this machine has no TPM 2.0. Anyone who can read your home directory can use it; an organisation that requires a hardware key refuses file keys.")
	}
	closeKey(b.key)
	return b.key.Ref()
}

func (b *deviceBinding) discard() {
	if b == nil {
		return
	}
	closeKey(b.key)
	_ = dpop.Delete(b.key.Ref())
}

func closeKey(k dpop.Key) {
	if c, ok := k.(io.Closer); ok {
		_ = c.Close()
	}
}

// errHardwareKeyRequired explains the gateway's refusal of a software key.
var errHardwareKeyRequired = errors.New("your organisation requires a hardware-backed device key (TPM 2.0) and this machine has none")

// tokenErrorHint names a token endpoint's error codes that have a remedy the
// person can act on.
func tokenErrorHint(code string) error {
	if code == "hardware_key_required" {
		return errHardwareKeyRequired
	}
	return nil
}

// ForgetSession removes the stored session for (profile, origin, email) and the
// device key bound to it, if any.
func ForgetSession(profile keychain.Profile, origin, email string) error {
	if data, err := keychain.ReadProfileToken(profile, origin, email); err == nil && data.DPoPKey != "" {
		_ = dpop.Delete(data.DPoPKey)
	}
	return keychain.DeleteProfileToken(profile, origin, email)
}

// dropReplacedKey deletes the device key of the session a new login replaces,
// unless the new session uses the same key.
func dropReplacedKey(profile keychain.Profile, origin, email, keeping string) {
	data, err := keychain.ReadProfileToken(profile, origin, email)
	if err == nil && data.DPoPKey != "" && data.DPoPKey != keeping {
		_ = dpop.Delete(data.DPoPKey)
	}
}

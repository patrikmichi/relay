package dpop

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// fakeTPM answers the two commands zuano sends the way a TPM 2.0 lays out its
// responses, and records every command so a test can check its bytes against
// the specification's layout.
type fakeTPM struct {
	keys     map[string]*ecdsa.PrivateKey
	commands [][]byte
	failWith uint32
	closed   bool
	current  *ecdsa.PrivateKey
}

func (f *fakeTPM) transmit(cmd []byte) ([]byte, error) {
	f.commands = append(f.commands, append([]byte(nil), cmd...))
	code := binary.BigEndian.Uint32(cmd[6:10])
	if f.failWith != 0 {
		return tpmResponse(tpmStNoSessions, f.failWith, nil), nil
	}
	switch code {
	case tpmCCCreatePrimary:
		// ... public | x(2+32) y(2) | outsideInfo(2) creationPCR(4)
		unique := cmd[len(cmd)-6-2-32 : len(cmd)-6-2]
		key := f.keys[string(unique)]
		if key == nil {
			key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			f.keys[string(unique)] = key
		}
		f.current = key
		point, _ := key.PublicKey.Bytes()
		var pub []byte
		pub = binary.BigEndian.AppendUint16(pub, tpmAlgECC)
		pub = binary.BigEndian.AppendUint16(pub, tpmAlgSHA256)
		pub = binary.BigEndian.AppendUint32(pub, tpmSigningKeyAttributes)
		pub = binary.BigEndian.AppendUint16(pub, 0)
		pub = binary.BigEndian.AppendUint16(pub, tpmAlgNull)
		pub = binary.BigEndian.AppendUint16(pub, tpmAlgECDSA)
		pub = binary.BigEndian.AppendUint16(pub, tpmAlgSHA256)
		pub = binary.BigEndian.AppendUint16(pub, tpmECCNISTP256)
		pub = binary.BigEndian.AppendUint16(pub, tpmAlgNull)
		pub = binary.BigEndian.AppendUint16(pub, 32)
		pub = append(pub, point[1:33]...)
		pub = binary.BigEndian.AppendUint16(pub, 32)
		pub = append(pub, point[33:65]...)
		var params []byte
		params = binary.BigEndian.AppendUint16(params, uint16(len(pub)))
		params = append(params, pub...)
		params = append(params, make([]byte, 20)...) // creation data, hash, ticket, name
		body := binary.BigEndian.AppendUint32(nil, 0x80000000)
		body = binary.BigEndian.AppendUint32(body, uint32(len(params)))
		return tpmResponse(tpmStSessions, 0, append(body, params...)), nil
	case tpmCCSign:
		digest := cmd[10+4+4+9+2 : 10+4+4+9+2+32]
		der, err := ecdsa.SignASN1(rand.Reader, f.current, digest)
		if err != nil {
			return nil, err
		}
		var sig struct{ R, S *big.Int }
		if _, err := asn1.Unmarshal(der, &sig); err != nil {
			return nil, err
		}
		var params []byte
		params = binary.BigEndian.AppendUint16(params, tpmAlgECDSA)
		params = binary.BigEndian.AppendUint16(params, tpmAlgSHA256)
		// A real TPM trims leading zeros of r and s.
		r, s := sig.R.Bytes(), sig.S.Bytes()
		params = binary.BigEndian.AppendUint16(params, uint16(len(r)))
		params = append(params, r...)
		params = binary.BigEndian.AppendUint16(params, uint16(len(s)))
		params = append(params, s...)
		body := binary.BigEndian.AppendUint32(nil, uint32(len(params)))
		return tpmResponse(tpmStSessions, 0, append(body, params...)), nil
	case tpmCCFlushContext:
		return tpmResponse(tpmStNoSessions, 0, nil), nil
	}
	return nil, errors.New("unexpected command")
}

func (f *fakeTPM) close() error { f.closed = true; return nil }

func tpmResponse(tag uint16, rc uint32, body []byte) []byte {
	out := binary.BigEndian.AppendUint16(nil, tag)
	out = binary.BigEndian.AppendUint32(out, uint32(10+len(body)))
	out = binary.BigEndian.AppendUint32(out, rc)
	return append(out, body...)
}

func fakeBackend(f *fakeTPM) tpmBackend {
	return tpmBackend{open: func() (tpmTransport, error) { return f, nil }}
}

func TestTPMCreatePrimaryCommandMatchesTheSpecificationLayout(t *testing.T) {
	f := &fakeTPM{keys: map[string]*ecdsa.PrivateKey{}}
	id := strings.Repeat("ab", 16)
	if _, err := fakeBackend(f).create(id); err != nil {
		t.Fatal(err)
	}
	unique := sha256.Sum256([]byte("zuano dpop " + id))

	var public []byte
	for _, h := range []string{
		"0023", "000b", "00040072", "0000", // ECC, SHA-256, signing key attributes, empty authPolicy
		"0010", "0018", "000b", "0003", "0010", // no symmetric, ECDSA with SHA-256, P-256, no KDF
		"0020",
	} {
		public = append(public, mustHex(t, h)...)
	}
	public = append(public, unique[:]...)
	public = append(public, 0, 0) // empty y

	var want []byte
	want = append(want, mustHex(t, "8002")...)
	body := mustHex(t, "40000001")                              // owner hierarchy
	body = append(body, mustHex(t, "00000009")...)              // authorization area size
	body = append(body, mustHex(t, "40000009 0000 01 0000")...) // password session, no nonce, continue, no HMAC
	body = append(body, mustHex(t, "0004 0000 0000")...)        // empty sensitive data
	body = binary.BigEndian.AppendUint16(body, uint16(len(public)))
	body = append(body, public...)
	body = append(body, mustHex(t, "0000 00000000")...) // outside info, no PCRs
	want = binary.BigEndian.AppendUint32(want, uint32(10+len(body)))
	want = append(want, mustHex(t, "00000131")...)
	want = append(want, body...)

	if !bytes.Equal(f.commands[0], want) {
		t.Errorf("CreatePrimary command\n got %x\nwant %x", f.commands[0], want)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.ReplaceAll(s, " ", "")
	out := make([]byte, len(s)/2)
	for i := range out {
		var v byte
		for _, c := range []byte{s[2*i], s[2*i+1]} {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= c - '0'
			case c >= 'a' && c <= 'f':
				v |= c - 'a' + 10
			default:
				t.Fatalf("bad hex %q", s)
			}
		}
		out[i] = v
	}
	return out
}

func TestTPMKeySignsProofsAGatewayAccepts(t *testing.T) {
	f := &fakeTPM{keys: map[string]*ecdsa.PrivateKey{}}
	k, err := fakeBackend(f).create(strings.Repeat("cd", 16))
	if err != nil {
		t.Fatal(err)
	}
	if k.Backend() != "tpm" || !k.Hardware() || !strings.HasPrefix(k.Ref(), "tpm:") {
		t.Errorf("key = %s hardware %v ref %s", k.Backend(), k.Hardware(), k.Ref())
	}
	for range 20 { // enough that a short r or s is likely to occur
		jws, err := Proof(k, "POST", "https://gateway.example/api/cli/refresh", "", time.Now(), "j")
		if err != nil {
			t.Fatal(err)
		}
		p := checkProof(t, jws)
		if !p.pub.Equal(k.Public()) {
			t.Fatal("the proof names a different key than the TPM key")
		}
	}
	signCmd := f.commands[len(f.commands)-1]
	if binary.BigEndian.Uint32(signCmd[6:10]) != tpmCCSign || binary.BigEndian.Uint32(signCmd[10:14]) != 0x80000000 {
		t.Errorf("last command %x is not a Sign on the key's handle", signCmd)
	}
}

func TestTheSameIDGivesTheSameKeyAndADifferentIDADifferentOne(t *testing.T) {
	f := &fakeTPM{keys: map[string]*ecdsa.PrivateKey{}}
	b := fakeBackend(f)
	a1, _ := b.load(strings.Repeat("01", 16))
	a2, _ := b.load(strings.Repeat("01", 16))
	other, _ := b.load(strings.Repeat("02", 16))
	if !a1.Public().Equal(a2.Public()) || a1.Public().Equal(other.Public()) {
		t.Error("a TPM key must be a function of its id")
	}
}

func TestTPMErrorsAreReportedAndTheTransportIsReleased(t *testing.T) {
	f := &fakeTPM{keys: map[string]*ecdsa.PrivateKey{}, failWith: 0x9a2}
	if _, err := fakeBackend(f).create(strings.Repeat("ef", 16)); err == nil || !strings.Contains(err.Error(), "0x9a2") {
		t.Fatalf("err = %v", err)
	}
	if !f.closed {
		t.Error("a failed create left the TPM connection open")
	}

	f = &fakeTPM{keys: map[string]*ecdsa.PrivateKey{}}
	k, _ := fakeBackend(f).create(strings.Repeat("ef", 16))
	if _, err := k.Sign([]byte("short")); err == nil {
		t.Error("a digest of the wrong size was signed")
	}
	f.failWith = 0x101
	if _, err := k.Sign(make([]byte, 32)); err == nil || !strings.Contains(err.Error(), "0x101") {
		t.Errorf("sign err = %v", err)
	}
	if c, ok := k.(interface{ Close() error }); !ok || c.Close() != nil || !f.closed {
		t.Error("Close did not release the connection")
	}
}

func TestMalformedTPMResponsesAreRejected(t *testing.T) {
	for name, resp := range map[string][]byte{
		"short":         {0x80, 0x02},
		"size mismatch": append(tpmResponse(tpmStSessions, 0, nil), 0),
	} {
		if _, err := tpmResponseBody(resp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseECCPublic([]byte{0, 0, 0, 0, 0, 5, 1}); err == nil {
		t.Error("a truncated public area was accepted")
	}
	if _, err := parseECDSASignature([]byte{0, 0, 0, 0, 0, 0x14}); err == nil {
		t.Error("a signature that is not ECDSA was accepted")
	}
}

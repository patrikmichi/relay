package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// The TPM 2.0 constants and structures this file needs, from the TCG TPM 2.0
// Library specification, parts 2 and 3.
const (
	tpmStNoSessions uint16 = 0x8001
	tpmStSessions   uint16 = 0x8002

	tpmCCCreatePrimary uint32 = 0x00000131
	tpmCCFlushContext  uint32 = 0x00000165
	tpmCCSign          uint32 = 0x0000015D

	tpmRHOwner uint32 = 0x40000001
	tpmRHNull  uint32 = 0x40000007
	tpmRSPW    uint32 = 0x40000009

	tpmAlgECC    uint16 = 0x0023
	tpmAlgSHA256 uint16 = 0x000B
	tpmAlgNull   uint16 = 0x0010
	tpmAlgECDSA  uint16 = 0x0018

	tpmECCNISTP256 uint16 = 0x0003
	tpmStHashCheck uint16 = 0x8024

	// fixedTPM | fixedParent | sensitiveDataOrigin | userWithAuth | sign: an
	// unrestricted signing key that cannot be duplicated or exported.
	tpmSigningKeyAttributes uint32 = 0x00040072
)

// tpmTransport carries one command to the TPM and returns its response.
type tpmTransport interface {
	transmit(command []byte) ([]byte, error)
	close() error
}

// tpmBackend keeps device keys in the machine's TPM 2.0. A key is a primary
// key under the owner hierarchy made from a template whose unique field is
// derived from the key's id, so the same id always yields the same key and
// nothing has to be stored: the private half never leaves the TPM and is
// bound to this machine.
type tpmBackend struct {
	open func() (tpmTransport, error)
}

func init() { hardware = tpmBackend{open: openTPM} }

func (b tpmBackend) create(id string) (Key, error) { return b.load(id) }

func (b tpmBackend) load(id string) (Key, error) {
	tr, err := b.open()
	if err != nil {
		return nil, err
	}
	k := &tpmKey{id: id, tr: tr}
	if err := k.derive(); err != nil {
		_ = tr.close()
		return nil, err
	}
	return k, nil
}

// delete has nothing to remove: the key is derived, not stored.
func (tpmBackend) delete(string) error { return nil }

type tpmKey struct {
	id string
	tr tpmTransport

	mu     sync.Mutex
	handle uint32
	pub    *ecdsa.PublicKey
}

func (k *tpmKey) Public() *ecdsa.PublicKey { return k.pub }
func (k *tpmKey) Backend() string          { return backendTPM }
func (k *tpmKey) Hardware() bool           { return true }
func (k *tpmKey) Ref() string              { return backendTPM + ":" + k.id }

// derive creates the primary key in the TPM and reads its public half. The
// handle stays loaded for the life of the process; the TPM resource manager
// drops it when the connection closes.
func (k *tpmKey) derive() error {
	unique := sha256.Sum256([]byte("relay dpop " + k.id))
	resp, err := k.tr.transmit(tpmCommand(tpmStSessions, tpmCCCreatePrimary, []uint32{tpmRHOwner}, tpmPasswordAuth(), createPrimaryParams(unique[:])))
	if err != nil {
		return err
	}
	body, err := tpmResponseBody(resp)
	if err != nil {
		return fmt.Errorf("create the TPM key: %w", err)
	}
	if len(body) < 4 {
		return errors.New("tpm: short CreatePrimary response")
	}
	handle := binary.BigEndian.Uint32(body[:4])
	pub, err := parseECCPublic(body[4:])
	if err != nil {
		k.flush(handle)
		return err
	}
	k.handle, k.pub = handle, pub
	return nil
}

func (k *tpmKey) flush(handle uint32) {
	_, _ = k.tr.transmit(tpmCommand(tpmStNoSessions, tpmCCFlushContext, nil, nil, binary.BigEndian.AppendUint32(nil, handle)))
}

func (k *tpmKey) Sign(digest []byte) ([]byte, error) {
	if len(digest) != sha256.Size {
		return nil, errors.New("tpm: sign takes a SHA-256 digest")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	params := binary.BigEndian.AppendUint16(nil, uint16(len(digest)))
	params = append(params, digest...)
	params = binary.BigEndian.AppendUint16(params, tpmAlgECDSA)
	params = binary.BigEndian.AppendUint16(params, tpmAlgSHA256)
	params = binary.BigEndian.AppendUint16(params, tpmStHashCheck)
	params = binary.BigEndian.AppendUint32(params, tpmRHNull)
	params = binary.BigEndian.AppendUint16(params, 0)
	resp, err := k.tr.transmit(tpmCommand(tpmStSessions, tpmCCSign, []uint32{k.handle}, tpmPasswordAuth(), params))
	if err != nil {
		return nil, err
	}
	body, err := tpmResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("sign with the TPM key: %w", err)
	}
	return parseECDSASignature(body)
}

// tpmCommand assembles a command: header, handles, the authorization area for
// a command with sessions, then the parameters.
func tpmCommand(tag uint16, code uint32, handles []uint32, auth, params []byte) []byte {
	var body []byte
	for _, h := range handles {
		body = binary.BigEndian.AppendUint32(body, h)
	}
	if tag == tpmStSessions {
		body = binary.BigEndian.AppendUint32(body, uint32(len(auth)))
		body = append(body, auth...)
	}
	body = append(body, params...)
	out := binary.BigEndian.AppendUint16(nil, tag)
	out = binary.BigEndian.AppendUint32(out, uint32(10+len(body)))
	out = binary.BigEndian.AppendUint32(out, code)
	return append(out, body...)
}

// tpmPasswordAuth is the authorization area of a password session with an
// empty password: the handle, an empty nonce, continueSession and an empty
// HMAC.
func tpmPasswordAuth() []byte {
	out := binary.BigEndian.AppendUint32(nil, tpmRSPW)
	out = binary.BigEndian.AppendUint16(out, 0)
	out = append(out, 0x01)
	return binary.BigEndian.AppendUint16(out, 0)
}

func createPrimaryParams(unique []byte) []byte {
	// inSensitive: an empty TPMS_SENSITIVE_CREATE inside its TPM2B.
	out := []byte{0, 4, 0, 0, 0, 0}

	var pub []byte
	pub = binary.BigEndian.AppendUint16(pub, tpmAlgECC)
	pub = binary.BigEndian.AppendUint16(pub, tpmAlgSHA256)
	pub = binary.BigEndian.AppendUint32(pub, tpmSigningKeyAttributes)
	pub = binary.BigEndian.AppendUint16(pub, 0) // authPolicy
	pub = binary.BigEndian.AppendUint16(pub, tpmAlgNull)
	pub = binary.BigEndian.AppendUint16(pub, tpmAlgECDSA)
	pub = binary.BigEndian.AppendUint16(pub, tpmAlgSHA256)
	pub = binary.BigEndian.AppendUint16(pub, tpmECCNISTP256)
	pub = binary.BigEndian.AppendUint16(pub, tpmAlgNull) // kdf
	pub = binary.BigEndian.AppendUint16(pub, uint16(len(unique)))
	pub = append(pub, unique...)
	pub = binary.BigEndian.AppendUint16(pub, 0)

	out = binary.BigEndian.AppendUint16(out, uint16(len(pub)))
	out = append(out, pub...)
	out = binary.BigEndian.AppendUint16(out, 0)  // outsideInfo
	return binary.BigEndian.AppendUint32(out, 0) // creationPCR: no PCRs
}

// tpmResponseBody checks a response and returns what follows its header: the
// response handles, then the parameter size and the parameters.
func tpmResponseBody(resp []byte) ([]byte, error) {
	if len(resp) < 10 {
		return nil, errors.New("tpm: short response")
	}
	size := binary.BigEndian.Uint32(resp[2:6])
	if int(size) != len(resp) {
		return nil, fmt.Errorf("tpm: response says %d bytes and has %d", size, len(resp))
	}
	if rc := binary.BigEndian.Uint32(resp[6:10]); rc != 0 {
		return nil, fmt.Errorf("TPM error 0x%x", rc)
	}
	return resp[10:], nil
}

// Close releases the key's handle and the connection to the TPM.
func (k *tpmKey) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.flush(k.handle)
	return k.tr.close()
}

// parseECCPublic reads the public key out of a CreatePrimary response body
// after its object handle: the parameter size, then a TPM2B_PUBLIC.
func parseECCPublic(afterHandle []byte) (*ecdsa.PublicKey, error) {
	r := &reader{b: afterHandle}
	r.skip(4) // parameterSize
	size := r.u16()
	body := r.take(int(size))
	if r.err != nil {
		return nil, r.err
	}
	p := &reader{b: body}
	if p.u16() != tpmAlgECC {
		return nil, errors.New("tpm: the key is not an ECC key")
	}
	p.skip(2 + 4) // nameAlg, objectAttributes
	p.skip(int(p.u16()))
	if p.u16() != tpmAlgNull { // symmetric
		return nil, errors.New("tpm: unexpected symmetric parameters")
	}
	if p.u16() != tpmAlgECDSA {
		return nil, errors.New("tpm: the key does not sign with ECDSA")
	}
	p.skip(2) // scheme hash
	if p.u16() != tpmECCNISTP256 {
		return nil, errors.New("tpm: the key is not on P-256")
	}
	p.skip(2) // kdf
	x := p.take(int(p.u16()))
	y := p.take(int(p.u16()))
	if p.err != nil {
		return nil, p.err
	}
	if len(x) > 32 || len(y) > 32 {
		return nil, errors.New("tpm: oversized key coordinates")
	}
	point := make([]byte, 65)
	point[0] = 4
	copy(point[1+32-len(x):33], x)
	copy(point[33+32-len(y):65], y)
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
}

// parseECDSASignature reads the TPMT_SIGNATURE of a Sign response body (after
// the handle area, which Sign has none of) into r||s.
func parseECDSASignature(body []byte) ([]byte, error) {
	r := &reader{b: body}
	r.skip(4) // parameterSize
	if r.u16() != tpmAlgECDSA {
		return nil, errors.New("tpm: the signature is not ECDSA")
	}
	r.skip(2) // hash
	rr := r.take(int(r.u16()))
	ss := r.take(int(r.u16()))
	if r.err != nil {
		return nil, r.err
	}
	if len(rr) > 32 || len(ss) > 32 {
		return nil, errors.New("tpm: oversized signature")
	}
	out := make([]byte, 64)
	copy(out[32-len(rr):32], rr)
	copy(out[64-len(ss):], ss)
	return out, nil
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.err = errors.New("tpm: truncated structure")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) skip(n int) { r.take(n) }

func (r *reader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}

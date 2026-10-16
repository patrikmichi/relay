package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHardware stands in for a platform backend.
type fakeHardware struct {
	createErr, deleteErr error
	created, deleted     []string
	loaded               Key
}

func (f *fakeHardware) create(id string) (Key, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, id)
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return &fileKey{id: id, priv: priv}, nil
}

func (f *fakeHardware) load(id string) (Key, error) { return f.loaded, nil }

func (f *fakeHardware) delete(id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func withHardware(t *testing.T, h hardwareBackend) {
	t.Helper()
	old := hardware
	hardware = h
	t.Cleanup(func() { hardware = old })
}

func keyDir(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".config", "zuano", "dpop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

const someID = "0123456789abcdef0123456789abcdef"

func TestCreatePrefersHardwareAndFallsBackToAFileUnlessRequired(t *testing.T) {
	withHome(t)
	hw := &fakeHardware{}
	withHardware(t, hw)
	k, err := Create(false)
	if err != nil || len(hw.created) != 1 {
		t.Fatalf("hardware not used: %v %v", err, hw.created)
	}
	if !strings.HasPrefix(k.Ref(), "file:") { // the fake hands back file keys
		t.Errorf("ref = %q", k.Ref())
	}

	hw.createErr = errors.New("tpm busy")
	fk, err := Create(false)
	if err != nil || fk.Hardware() || fk.Backend() != backendFile {
		t.Fatalf("no file fallback: %v hardware=%v", err, fk != nil && fk.Hardware())
	}
	if _, err := Create(true); !errors.Is(err, ErrNoHardwareKey) || !strings.Contains(err.Error(), "tpm busy") {
		t.Errorf("required hardware err = %v", err)
	}
}

func TestHardwareAvailableReflectsTheBackend(t *testing.T) {
	hw := &fakeHardware{}
	withHardware(t, hw)
	if !HardwareAvailable() {
		t.Error("a working backend was reported unavailable")
	}
	hw.createErr = errors.New("no tpm")
	if HardwareAvailable() {
		t.Error("a failing backend was reported available")
	}
}

func TestTPMReferencesNeedAHardwareBackend(t *testing.T) {
	withHome(t)
	ref := backendTPM + ":" + someID

	withoutHardware(t)
	if _, err := Load(ref); err == nil || !strings.Contains(err.Error(), "TPM") {
		t.Errorf("Load without a backend: %v", err)
	}
	if err := Delete(ref); err != nil {
		t.Errorf("Delete without a backend: %v", err)
	}

	want, _ := Create(false)
	hw := &fakeHardware{loaded: want}
	withHardware(t, hw)
	if got, err := Load(ref); err != nil || got != want {
		t.Errorf("Load = %v, %v", got, err)
	}
	if err := Delete(ref); err != nil || len(hw.deleted) != 1 || hw.deleted[0] != someID {
		t.Errorf("Delete = %v, deleted %v", err, hw.deleted)
	}
	hw.deleteErr = errors.New("denied")
	if err := Delete(ref); err == nil {
		t.Error("a backend delete failure was swallowed")
	}
}

func TestCreateFileFailsWhereTheKeyCannotBeStored(t *testing.T) {
	home := withHome(t)
	cfg := filepath.Join(home, ".config", "zuano")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "dpop"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createFile(someID); err == nil || !strings.Contains(err.Error(), "key directory") {
		t.Errorf("blocked directory: %v", err)
	}

	if err := os.Remove(filepath.Join(cfg, "dpop")); err != nil {
		t.Fatal(err)
	}
	if _, err := createFile(someID); err != nil {
		t.Fatal(err)
	}
	if _, err := createFile(someID); err == nil || !strings.Contains(err.Error(), "store the device key") {
		t.Errorf("an existing key was overwritten: %v", err)
	}
}

func TestLoadFileRejectsKeysItCannotTrust(t *testing.T) {
	home := withHome(t)
	dir := keyDir(t, home)
	write := func(id string, block *pem.Block, mode os.FileMode) {
		t.Helper()
		data := []byte("not pem")
		if block != nil {
			data = pem.EncodeToMemory(block)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".pem"), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	id := func(c string) string { return strings.Repeat(c, 32) }

	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384DER, _ := x509.MarshalPKCS8PrivateKey(p384)

	write(id("a"), nil, 0o600)
	write(id("b"), &pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}, 0o600)
	write(id("c"), &pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}, 0o600)
	write(id("d"), &pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER}, 0o600)
	write(id("e"), &pem.Block{Type: "PRIVATE KEY", Bytes: p384DER}, 0o600)
	for c, want := range map[string]string{"a": "not a PKCS#8", "b": "not a PKCS#8", "c": "", "d": "not a P-256", "e": "not a P-256"} {
		if _, err := loadFile(id(c)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("key %s: err = %v, want %q", c, err, want)
		}
	}

	link := id("f")
	if err := os.Symlink(filepath.Join(dir, id("a")+".pem"), filepath.Join(dir, link+".pem")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := loadFile(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("a symlinked key was used: %v", err)
	}
}

func TestLoadDispatchesOnTheBackendName(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, err := Create(false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(k.Ref())
	if err != nil || got.Ref() != k.Ref() {
		t.Fatalf("Load = %v, %v", got, err)
	}
	if !got.Public().Equal(k.Public()) {
		t.Error("a reloaded key has a different public key")
	}
}

func TestThumbprintAndProofRefuseKeysThatAreNotP256(t *testing.T) {
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := Thumbprint(&p384.PublicKey); err == nil {
		t.Error("a P-384 thumbprint was produced")
	}
	if _, err := Proof(&fileKey{id: someID, priv: p384}, "GET", "https://gw.example.com/x", "", time.Now(), "j"); err == nil {
		t.Error("a proof was built for a P-384 key")
	}
}

type failingKey struct{ Key }

func (failingKey) Sign([]byte) ([]byte, error) { return nil, errors.New("device locked") }

func TestProofReportsSigningFailures(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, _ := Create(false)
	_, err := Proof(failingKey{k}, "POST", "https://gw.example.com/token", "", time.Now(), "j")
	if err == nil || !strings.Contains(err.Error(), "device locked") {
		t.Errorf("err = %v", err)
	}
}

func TestProofNormalisesTheTargetURL(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, _ := Create(false)
	jws, err := Proof(k, "get", "HTTPS://GW.Example.com/a%20b?q=1#frag", "", time.Unix(1700000000, 0), "j1")
	if err != nil {
		t.Fatal(err)
	}
	claims := checkProof(t, jws).claims
	if claims["htu"] != "https://gw.example.com/a%20b" || claims["htm"] != "GET" || claims["jti"] != "j1" {
		t.Errorf("claims = %v", claims)
	}
}

func TestRawSignatureRejectsMalformedDER(t *testing.T) {
	trailing, _ := asn1.Marshal(struct{ R, S int }{1, 2})
	for name, der := range map[string][]byte{
		"not DER":  {1, 2, 3},
		"trailing": append(trailing, 0),
	} {
		if _, err := rawSignature(der); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	zero, _ := asn1.Marshal(struct{ R, S int }{0, 5})
	if _, err := rawSignature(zero); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("zero r: err = %v", err)
	}
	ok, _ := asn1.Marshal(struct{ R, S int }{1, 2})
	sig, err := rawSignature(ok)
	if err != nil || len(sig) != 64 || sig[31] != 1 || sig[63] != 2 {
		t.Errorf("sig = %x, %v", sig, err)
	}
}

package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// withoutHardware runs a test as if the machine had no TPM.
func withoutHardware(t *testing.T) {
	t.Helper()
	old := hardware
	hardware = nil
	t.Cleanup(func() { hardware = old })
}

type proofParts struct {
	header map[string]any
	claims map[string]any
	pub    *ecdsa.PublicKey
}

// checkProof verifies a proof the way a gateway does: the signature against
// the key in the proof's own header.
func checkProof(t *testing.T, jws string) proofParts {
	t.Helper()
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		t.Fatalf("a proof has three parts, got %d", len(parts))
	}
	var p proofParts
	for i, into := range []*map[string]any{&p.header, &p.claims} {
		raw, err := b64.DecodeString(parts[i])
		if err != nil || json.Unmarshal(raw, into) != nil {
			t.Fatalf("part %d is not base64url JSON: %v", i, err)
		}
	}
	jwk, _ := p.header["jwk"].(map[string]any)
	x, _ := b64.DecodeString(jwk["x"].(string))
	y, _ := b64.DecodeString(jwk["y"].(string))
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append([]byte{4}, append(x, y...)...))
	if err != nil {
		t.Fatalf("header key: %v", err)
	}
	p.pub = pub
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: %v (%d bytes)", err, len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	der, _ := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
	if !ecdsa.VerifyASN1(pub, digest[:], der) {
		t.Fatal("the proof's signature does not verify against its own key")
	}
	return p
}

func TestProofCarriesTheClaimsAGatewayChecks(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, err := Create(false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1792144800, 0)
	jws, err := Proof(k, "get", "HTTPS://Gateway.Example/api/me/sessions?x=1#frag", "access-token", now, "jti-1")
	if err != nil {
		t.Fatal(err)
	}
	p := checkProof(t, jws)
	if p.header["typ"] != "dpop+jwt" || p.header["alg"] != "ES256" {
		t.Errorf("header = %v", p.header)
	}
	want := sha256.Sum256([]byte("access-token"))
	if p.claims["htm"] != "GET" || p.claims["htu"] != "https://gateway.example/api/me/sessions" || p.claims["jti"] != "jti-1" || p.claims["iat"] != float64(1792144800) || p.claims["ath"] != b64.EncodeToString(want[:]) {
		t.Errorf("claims = %v", p.claims)
	}
}

func TestProofForATokenRequestHasNoAccessTokenHash(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, _ := Create(false)
	jws, err := Proof(k, "POST", "https://gateway.example/api/cli/refresh", "", time.Now(), "j")
	if err != nil {
		t.Fatal(err)
	}
	if _, has := checkProof(t, jws).claims["ath"]; has {
		t.Error("a token-endpoint proof must not carry ath")
	}
}

func TestProofRefusesARelativeURL(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, _ := Create(false)
	if _, err := Proof(k, "GET", "/api/x", "t", time.Now(), "j"); err == nil {
		t.Fatal("a relative URL was accepted")
	}
}

func TestSignerUsesFreshIDsAndTheClock(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, _ := Create(false)
	s := NewSigner(k)
	seen := map[string]bool{}
	for range 5 {
		jws, err := s.Proof("GET", "https://gateway.example/x", "t")
		if err != nil {
			t.Fatal(err)
		}
		id := checkProof(t, jws).claims["jti"].(string)
		if seen[id] {
			t.Fatalf("jti %q repeated: a proof must not be replayable", id)
		}
		seen[id] = true
	}
	s.Now = func() time.Time { return time.Unix(42, 0) }
	jws, _ := s.Proof("GET", "https://gateway.example/x", "t")
	if checkProof(t, jws).claims["iat"] != float64(42) {
		t.Error("the signer ignored its clock")
	}
}

func TestThumbprintIsTheRFC7638HashOfTheCanonicalJWK(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	k, _ := Create(false)
	got, err := Thumbprint(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	point, _ := k.Public().Bytes()
	canonical := `{"crv":"P-256","kty":"EC","x":"` + b64.EncodeToString(point[1:33]) + `","y":"` + b64.EncodeToString(point[33:65]) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	if got != b64.EncodeToString(sum[:]) {
		t.Errorf("thumbprint = %q", got)
	}
}

func TestFileKeyIsStoredPrivatelyAndReloads(t *testing.T) {
	home := withHome(t)
	withoutHardware(t)
	k, err := Create(false)
	if err != nil {
		t.Fatal(err)
	}
	if k.Backend() != "file" || k.Hardware() || !strings.HasPrefix(k.Ref(), "file:") {
		t.Errorf("key = %s hardware %v ref %s", k.Backend(), k.Hardware(), k.Ref())
	}
	dir := filepath.Join(home, ".config", "relay", "dpop")
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("key directory: %v %v", info, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	if info, _ := entries[0].Info(); info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}

	again, err := Load(k.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if !again.Public().Equal(k.Public()) {
		t.Error("a reloaded key is a different key")
	}
	sig, err := again.Sign(make([]byte, 32))
	if err != nil || len(sig) != 64 {
		t.Errorf("sign: %d bytes, %v", len(sig), err)
	}
}

func TestLoadRefusesAKeyOthersCanRead(t *testing.T) {
	home := withHome(t)
	withoutHardware(t)
	k, _ := Create(false)
	path := filepath.Join(home, ".config", "relay", "dpop", strings.TrimPrefix(k.Ref(), "file:")+".pem")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(k.Ref()); err == nil || !strings.Contains(err.Error(), "only its owner") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAndDeleteHandleMissingAndMalformedReferences(t *testing.T) {
	withHome(t)
	withoutHardware(t)
	for _, ref := range []string{"", "file", "file:../../etc/passwd", "file:XYZ", "vault:00000000000000000000000000000000"} {
		if _, err := Load(ref); err == nil {
			t.Errorf("Load(%q) succeeded", ref)
		}
	}
	if _, err := Load("file:00000000000000000000000000000000"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a missing key: %v", err)
	}
	k, _ := Create(false)
	if err := Delete(k.Ref()); err != nil {
		t.Fatal(err)
	}
	if err := Delete(k.Ref()); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	if err := Delete("nonsense"); err != nil {
		t.Errorf("deleting a bad reference: %v", err)
	}
	if _, err := Load(k.Ref()); err == nil {
		t.Error("a deleted key loaded")
	}
}

func TestRequiringHardwareWithoutOneFailsInsteadOfFallingBack(t *testing.T) {
	home := withHome(t)
	withoutHardware(t)
	if _, err := Create(true); !errors.Is(err, ErrNoHardwareKey) {
		t.Fatalf("err = %v, want ErrNoHardwareKey", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".config", "relay", "dpop")); len(entries) != 0 {
		t.Errorf("a file key was written although hardware was required: %v", entries)
	}
	if HardwareAvailable() {
		t.Error("HardwareAvailable without a backend")
	}
}

package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/patrikmichi/zuano/internal/config"
)

// A key reference is "<backend>:<id>". The backends are the file store here
// and, where a TPM is present, the TPM.
const (
	backendFile = "file"
	backendTPM  = "tpm"
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// hardwareBackend is a key store the private half never leaves.
type hardwareBackend interface {
	create(id string) (Key, error)
	load(id string) (Key, error)
	delete(id string) error
}

// hardware is the platform's hardware backend, set by a file built for a
// platform that has one.
var hardware hardwareBackend

// Create makes a device key. It prefers a hardware-backed key; without one it
// makes a file key, unless requireHardware, which fails with ErrNoHardwareKey.
func Create(requireHardware bool) (Key, error) {
	id := randomID()
	if hardware != nil {
		if k, err := hardware.create(id); err == nil {
			return k, nil
		} else if requireHardware {
			return nil, fmt.Errorf("%w: %v", ErrNoHardwareKey, err)
		}
	} else if requireHardware {
		return nil, ErrNoHardwareKey
	}
	return createFile(id)
}

// Load opens the key a reference names.
func Load(ref string) (Key, error) {
	backend, id, ok := strings.Cut(ref, ":")
	if !ok || !idPattern.MatchString(id) {
		return nil, fmt.Errorf("dpop: %q is not a key reference", ref)
	}
	switch backend {
	case backendFile:
		return loadFile(id)
	case backendTPM:
		if hardware == nil {
			return nil, errors.New("dpop: this session's key is in a TPM, which this build cannot reach")
		}
		return hardware.load(id)
	}
	return nil, fmt.Errorf("dpop: unknown key backend %q", backend)
}

// Delete removes the key a reference names; a key that is already gone is not
// an error.
func Delete(ref string) error {
	backend, id, ok := strings.Cut(ref, ":")
	if !ok || !idPattern.MatchString(id) {
		return nil
	}
	switch backend {
	case backendFile:
		err := os.Remove(filePath(id))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	case backendTPM:
		if hardware != nil {
			return hardware.delete(id)
		}
	}
	return nil
}

// HardwareAvailable reports whether this machine can hold a hardware-backed
// key.
func HardwareAvailable() bool {
	if hardware == nil {
		return false
	}
	k, err := hardware.create(randomID())
	if err != nil {
		return false
	}
	if c, ok := k.(io.Closer); ok {
		_ = c.Close()
	}
	return true
}

func filePath(id string) string {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "dpop", id+".pem")
}

type fileKey struct {
	id   string
	priv *ecdsa.PrivateKey
}

func (k *fileKey) Public() *ecdsa.PublicKey { return &k.priv.PublicKey }
func (k *fileKey) Backend() string          { return backendFile }
func (k *fileKey) Hardware() bool           { return false }
func (k *fileKey) Ref() string              { return backendFile + ":" + k.id }

func (k *fileKey) Sign(digest []byte) ([]byte, error) {
	der, err := ecdsa.SignASN1(rand.Reader, k.priv, digest)
	if err != nil {
		return nil, err
	}
	return rawSignature(der)
}

func createFile(id string) (Key, error) {
	path := filePath(id)
	if path == "" {
		return nil, errors.New("dpop: no config directory for the device key")
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("dpop: create the key directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dpop: store the device key: %w", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &fileKey{id: id, priv: priv}, nil
}

func loadFile(id string) (Key, error) {
	path := filePath(id)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("dpop: the device key for this session is missing: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("dpop: refusing to use %s: it must be a regular file only its owner can access", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("dpop: %s is not a PKCS#8 private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("dpop: %s is not a P-256 key", path)
	}
	return &fileKey{id: id, priv: priv}, nil
}

// Package plugins fetches, verifies and records plugin bundles from the
// gateway's private feeds.
package plugins

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/patrikmichi/zuano/internal/signing"
)

// SignatureFile is the name of the signature at the root of a package.
const SignatureFile = "SIGNATURE.json"

const (
	signatureSchema = "plugin-signature/1"
	maxFiles        = 5000
	maxFileBytes    = 32 << 20
	maxTotalBytes   = 96 << 20
	// ExitVerify is the process exit status of a package that fails verification.
	ExitVerify = 4
)

// VerifyError is a package that does not verify: nothing was installed from
// it.
type VerifyError struct{ Reason string }

func (e *VerifyError) Error() string { return "plugin package rejected: " + e.Reason }

// ExitCode is the process exit status for the failure.
func (*VerifyError) ExitCode() int { return ExitVerify }

func reject(format string, args ...any) error {
	return &VerifyError{Reason: fmt.Sprintf(format, args...)}
}

// Expect is what a package must be.
type Expect struct {
	Plugin, Version, Platform string
	// SHA256 is the hash of the archive the feed lists.
	SHA256 string
	// Keys are the signing keys pinned for the gateway.
	Keys []signing.Key
}

// Package is a verified package: every file hashes to what the signature lists
// and nothing else is in it.
type Package struct {
	Files     map[string][]byte
	Signature Signature
}

// Signature is the content of SIGNATURE.json.
type Signature struct {
	Schema    string            `json:"schema"`
	Plugin    string            `json:"plugin"`
	Version   string            `json:"version"`
	Platform  string            `json:"platform"`
	Files     map[string]string `json:"files"`
	KID       string            `json:"kid"`
	Alg       string            `json:"alg"`
	Signature string            `json:"signature"`
}

// Paths returns the package's file paths, sorted.
func (p *Package) Paths() []string {
	paths := make([]string, 0, len(p.Files))
	for name := range p.Files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

// Verify checks an archive (".zip" or ".tgz") against the feed's hash and its
// in-package signature, and returns its files. Any mismatch, in the archive
// hash, the signature, a file's hash, or a file the signature does not list
// (or one it lists that is missing), is a *VerifyError.
func Verify(archive []byte, ext string, e Expect) (*Package, error) {
	if e.SHA256 == "" {
		return nil, reject("the feed lists no sha256 for the archive")
	}
	sum := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), e.SHA256) {
		return nil, reject("the archive's sha256 does not match the feed's")
	}
	files, err := unpack(archive, ext)
	if err != nil {
		return nil, err
	}
	raw, ok := files[SignatureFile]
	if !ok {
		return nil, reject("the package carries no %s", SignatureFile)
	}
	delete(files, SignatureFile)

	var sig Signature
	if err := json.Unmarshal(raw, &sig); err != nil {
		return nil, reject("%s is not valid JSON", SignatureFile)
	}
	switch {
	case sig.Schema != signatureSchema:
		return nil, reject("%s has the unknown schema %q", SignatureFile, sig.Schema)
	case sig.Alg != "ed25519":
		return nil, reject("%s uses the unsupported algorithm %q", SignatureFile, sig.Alg)
	case sig.Plugin != e.Plugin || sig.Version != e.Version || sig.Platform != e.Platform:
		return nil, reject("the signature is for %s %s (%s), not %s %s (%s)", sig.Plugin, sig.Version, sig.Platform, e.Plugin, e.Version, e.Platform)
	}
	message, err := signedMessage(raw)
	if err != nil {
		return nil, err
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return nil, reject("the signature is not base64")
	}
	if err := signing.VerifyMessage(e.Keys, sig.KID, message, sigBytes); err != nil {
		return nil, reject("%v", err)
	}

	for name, want := range sig.Files {
		got, present := files[name]
		if !present {
			return nil, reject("%s is listed in the signature but missing from the package", name)
		}
		h := sha256.Sum256(got)
		if !strings.EqualFold(hex.EncodeToString(h[:]), want) {
			return nil, reject("%s does not match its signed hash", name)
		}
	}
	for name := range files {
		if _, listed := sig.Files[name]; !listed {
			return nil, reject("%s is in the package but not in the signature", name)
		}
	}
	return &Package{Files: files, Signature: sig}, nil
}

// signedMessage is the canonical JSON of the signature document without its
// signature member.
func signedMessage(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, reject("%s is not a JSON object", SignatureFile)
	}
	delete(doc, "signature")
	msg, err := signing.CanonicalJSON(doc)
	if err != nil {
		return nil, reject("%s cannot be canonicalized", SignatureFile)
	}
	return msg, nil
}

// cleanName checks an archive member name: relative, no traversal, no
// backslash, no empty components.
func cleanName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\\') || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", reject("the archive has an unsafe path %q", name)
	}
	clean := path.Clean(name)
	if clean != strings.TrimSuffix(name, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", reject("the archive has an unsafe path %q", name)
	}
	return clean, nil
}

type unpacker struct {
	files map[string][]byte
	total int64
}

func (u *unpacker) add(name string, r io.Reader, size int64) error {
	clean, err := cleanName(name)
	if err != nil {
		return err
	}
	if _, dup := u.files[clean]; dup {
		return reject("the archive lists %s twice", clean)
	}
	if len(u.files) >= maxFiles || size > maxFileBytes {
		return reject("the archive is too large")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxFileBytes+1))
	if err != nil {
		return reject("cannot read %s: %v", clean, err)
	}
	u.total += int64(len(data))
	if len(data) > maxFileBytes || u.total > maxTotalBytes {
		return reject("the archive is too large")
	}
	u.files[clean] = data
	return nil
}

func unpack(archive []byte, ext string) (map[string][]byte, error) {
	u := &unpacker{files: map[string][]byte{}}
	switch ext {
	case ".zip":
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, reject("not a zip archive")
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			if !f.Mode().IsRegular() {
				return nil, reject("%s is not a regular file", f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, reject("cannot read %s", f.Name)
			}
			err = u.add(f.Name, rc, int64(f.UncompressedSize64))
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
		}
	case ".tgz":
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return nil, reject("not a gzip archive")
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, reject("not a tar archive")
			}
			switch h.Typeflag {
			case tar.TypeDir:
				continue
			case tar.TypeReg:
			default:
				return nil, reject("%s is not a regular file", h.Name)
			}
			// npm packs everything under package/.
			if err := u.add(strings.TrimPrefix(h.Name, "package/"), tr, h.Size); err != nil {
				return nil, err
			}
		}
	default:
		return nil, reject("unsupported archive type %q", ext)
	}
	return u.files, nil
}

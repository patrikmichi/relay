// Package drop prepares files and folders for publishing as a site: it
// canonicalizes paths and builds the manifest the gateway checks upload by
// upload. The rules mirror the gateway's, and testdata/paths.json holds the
// vectors both sides must pass.
package drop

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	maxPathBytes      = 240
	maxPathComponents = 32
)

// Reasons a path is refused. Callers compare with errors.Is.
var (
	ErrEmpty          = errors.New("path is empty")
	ErrAbsolute       = errors.New("path is absolute")
	ErrTraversal      = errors.New("path has a . or .. component")
	ErrEmptyComponent = errors.New("path has an empty component")
	ErrControl        = errors.New("path has a control character")
	ErrBackslash      = errors.New("path has a backslash")
	ErrDrivePrefix    = errors.New("path starts with a drive letter")
	ErrReserved       = errors.New("path is under a reserved prefix")
	ErrTooLong        = errors.New("path is too long")
	ErrTooDeep        = errors.New("path has too many components")
	ErrInvalidUTF8    = errors.New("path is not valid UTF-8")
)

// reservedTop are first path components the platform serves itself.
var reservedTop = map[string]bool{"_drop": true, "api": true}

// Canonicalize validates a relative path and returns it in canonical form:
// NFC, forward slashes, no trailing slash.
func Canonicalize(p string) (string, error) {
	if p == "" {
		return "", ErrEmpty
	}
	if !utf8.ValidString(p) {
		return "", ErrInvalidUTF8
	}
	if strings.ContainsRune(p, '\\') {
		return "", ErrBackslash
	}
	for _, r := range p {
		if r == 0 || unicode.IsControl(r) {
			return "", ErrControl
		}
	}
	p = norm.NFC.String(p)
	if strings.HasPrefix(p, "/") {
		return "", ErrAbsolute
	}
	if len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') {
		return "", ErrDrivePrefix
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", ErrEmpty
	}
	parts := strings.Split(p, "/")
	if len(parts) > maxPathComponents {
		return "", ErrTooDeep
	}
	for _, part := range parts {
		switch part {
		case "":
			return "", ErrEmptyComponent
		case ".", "..":
			return "", ErrTraversal
		}
	}
	if reservedTop[strings.ToLower(parts[0])] {
		return "", fmt.Errorf("%w: /%s/", ErrReserved, parts[0])
	}
	if len(p) > maxPathBytes {
		return "", ErrTooLong
	}
	return p, nil
}

// CheckSet reports a collision between canonical paths: a duplicate, a path
// that is both a file and a directory, or two paths that differ only in case
// or Unicode case folding.
func CheckSet(paths []string) error {
	files := make(map[string]string, len(paths))
	dirs := map[string]string{}
	for _, p := range paths {
		fold := strings.ToLower(norm.NFC.String(p))
		if prior, ok := files[fold]; ok {
			if prior == p {
				return fmt.Errorf("duplicate path %q", p)
			}
			return fmt.Errorf("paths %q and %q differ only in case", prior, p)
		}
		files[fold] = p
		for i := 0; i < len(p); i++ {
			if p[i] == '/' {
				dirs[strings.ToLower(p[:i])] = p[:i]
			}
		}
	}
	for fold, p := range files {
		if d, ok := dirs[fold]; ok {
			return fmt.Errorf("%q is both a file and a directory (%q)", p, d)
		}
	}
	return nil
}

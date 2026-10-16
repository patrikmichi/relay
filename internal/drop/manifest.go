package drop

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// File is one file of a release.
type File struct {
	Path   string // canonical, slash separated
	Abs    string // where it is on disk
	Size   int64
	SHA256 string
}

// Manifest is what a publish uploads.
type Manifest struct {
	Files    []File
	Warnings []string
}

// TotalBytes is the sum of the file sizes.
func (m Manifest) TotalBytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// IgnoreFile is the file at the root of a folder that lists what not to publish.
const IgnoreFile = ".dropignore"

// excluded names are left out with a warning: they are never part of a site.
var excludedDirs = map[string]bool{"node_modules": true, "__macosx": true, ".spotlight-v100": true, ".trashes": true}
var excludedFiles = map[string]bool{".ds_store": true, "thumbs.db": true, "desktop.ini": true}

var deniedDirs = map[string]bool{".git": true, ".ssh": true, ".aws": true, ".gnupg": true}

// Denied reports why a path must not be published: it looks like a credential
// or a repository's internals.
func Denied(p string) (string, bool) {
	parts := strings.Split(strings.ToLower(p), "/")
	for _, dir := range parts[:len(parts)-1] {
		if deniedDirs[dir] {
			return "inside " + dir + "/", true
		}
	}
	base := parts[len(parts)-1]
	if deniedDirs[base] {
		return base, true
	}
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return "environment file", true
	case strings.HasPrefix(base, "id_rsa"), strings.HasPrefix(base, "id_dsa"), strings.HasPrefix(base, "id_ecdsa"), strings.HasPrefix(base, "id_ed25519"):
		return "private key", true
	case strings.HasSuffix(base, ".pem"), strings.HasSuffix(base, ".p12"), strings.HasSuffix(base, ".pfx"), strings.HasSuffix(base, ".keystore"):
		return "key or certificate store", true
	case base == ".npmrc", base == ".pypirc", base == ".netrc", base == ".pgpass", base == "credentials", base == "credentials.json":
		return "credential file", true
	}
	return "", false
}

// Build lists, checks and hashes what a publish would upload. root is a file
// or a folder. Symlinks are never followed; they and the names that never
// belong in a site are skipped with a warning, and a credential-looking file
// fails the whole build so it cannot be published by accident.
func Build(root string) (Manifest, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return m, fmt.Errorf("%s is not a regular file", root)
		}
		name := filepath.Base(root)
		canon, err := Canonicalize(name)
		if err != nil {
			return m, fmt.Errorf("%s: %w", name, err)
		}
		if why, bad := Denied(canon); bad {
			return m, fmt.Errorf("refusing to publish %s: %s", name, why)
		}
		m.Files = []File{{Path: canon, Abs: root, Size: info.Size()}}
		return m, hashAll(&m)
	}

	ignore := loadIgnore(root)
	var denied []string
	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		lower := strings.ToLower(d.Name())
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			m.Warnings = append(m.Warnings, "skipped symlink "+rel)
			return nil
		case ignore.matches(rel, d.IsDir()):
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case d.IsDir() && excludedDirs[lower]:
			m.Warnings = append(m.Warnings, "left out "+rel+"/")
			return filepath.SkipDir
		case !d.IsDir() && excludedFiles[lower]:
			m.Warnings = append(m.Warnings, "left out "+rel)
			return nil
		case d.IsDir():
			if deniedDirs[lower] {
				denied = append(denied, rel+"/ (inside "+lower+"/)")
				return filepath.SkipDir
			}
			return nil
		case !d.Type().IsRegular():
			m.Warnings = append(m.Warnings, "skipped "+rel+" (not a regular file)")
			return nil
		}
		if why, bad := Denied(rel); bad {
			denied = append(denied, rel+" ("+why+")")
			return nil
		}
		canon, cerr := Canonicalize(rel)
		if cerr != nil {
			return fmt.Errorf("%s: %w", rel, cerr)
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		m.Files = append(m.Files, File{Path: canon, Abs: abs, Size: fi.Size()})
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		return Manifest{}, fmt.Errorf("refusing to publish files that look like credentials or repository internals:\n  %s\nremove them, or list them in %s to leave them out", strings.Join(denied, "\n  "), IgnoreFile)
	}
	if len(m.Files) == 0 {
		return Manifest{}, errors.New("nothing to publish: the folder has no files")
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	paths := make([]string, len(m.Files))
	for i, f := range m.Files {
		paths[i] = f.Path
	}
	if err := CheckSet(paths); err != nil {
		return Manifest{}, err
	}
	return m, hashAll(&m)
}

func hashAll(m *Manifest) error {
	for i := range m.Files {
		f, err := os.Open(m.Files[i].Abs)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", m.Files[i].Path, err)
		}
		m.Files[i].Size = n
		m.Files[i].SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	return nil
}

// ignoreList holds the patterns of a .dropignore: one per line, # starts a
// comment, a trailing / matches directories only, a pattern with a / matches
// the path from the folder root, any other pattern matches a name anywhere.
type ignoreList []string

func loadIgnore(root string) ignoreList {
	f, err := os.Open(filepath.Join(root, IgnoreFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var list ignoreList
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			list = append(list, line)
		}
	}
	return list
}

func (l ignoreList) matches(rel string, isDir bool) bool {
	for _, pat := range l {
		dirOnly := strings.HasSuffix(pat, "/")
		pat = strings.TrimPrefix(strings.TrimSuffix(pat, "/"), "/")
		if dirOnly && !isDir {
			continue
		}
		target := path.Base(rel)
		if strings.Contains(pat, "/") {
			target = rel
		}
		if ok, _ := path.Match(pat, target); ok {
			return true
		}
	}
	return false
}

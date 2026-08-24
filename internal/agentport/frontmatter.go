package agentport

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// splitFrontmatter splits a SKILL.md's raw content into the YAML frontmatter
// block (raw bytes, without the "---" delimiters) and the markdown body.
// Returns an error if content does not start with a "---" frontmatter block.
func splitFrontmatter(content []byte) (fm []byte, body string, err error) {
	text := string(content)
	if !strings.HasPrefix(text, "---") {
		return nil, "", fmt.Errorf("missing frontmatter: file must start with \"---\"")
	}
	rest := text[3:]
	rest = strings.TrimPrefix(rest, "\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, "", fmt.Errorf("missing closing frontmatter delimiter \"---\"")
	}
	fmBlock := rest[:idx]
	after := rest[idx+len("\n---"):]
	after = strings.TrimPrefix(after, "\n")
	return []byte(fmBlock), after, nil
}

// joinFrontmatter serializes frontmatter YAML bytes + a markdown body into a
// full SKILL.md file's content.
func joinFrontmatter(fm []byte, body string) []byte {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(fm)
	if len(fm) > 0 && !strings.HasSuffix(string(fm), "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("---\n\n")
	sb.WriteString(strings.TrimLeft(body, "\n"))
	if body != "" && !strings.HasSuffix(body, "\n") {
		sb.WriteString("\n")
	}
	return []byte(sb.String())
}

// flexStringList unmarshals from either a YAML scalar (comma- or
// space-separated string) or a YAML sequence of strings, normalizing to a
// []string. Used for fields providers allow in either form (Claude
// allowed-tools, Cursor paths).
type flexStringList []string

func (f *flexStringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*f = splitFlexList(s)
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		*f = flexStringList(items)
	case 0:
		// Zero value (field absent) — leave *f as nil.
	default:
		return fmt.Errorf("expected a scalar or sequence, got %v", node.Kind)
	}
	return nil
}

func splitFlexList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var parts []string
	if strings.Contains(s, ",") {
		parts = strings.Split(s, ",")
	} else {
		parts = strings.Fields(s)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadResources walks skillDir and reads every regular file except those in
// exclude (paths relative to skillDir, forward-slash separated) into a
// relative-path -> bytes map. Returns an empty (non-nil) map if skillDir has
// no resource files.
//
// When resourceDirs is non-empty, the walk is scoped to only those top-level
// subdirectories (e.g. ["scripts", "references", "assets"], from
// ProviderConfig.ResourceDirs) — any other top-level directory is skipped
// entirely (filepath.SkipDir), and any loose file directly in skillDir's
// root (other than the excluded skill file/sidecar, which are matched via
// exclude before this check) is excluded too. An empty/nil resourceDirs
// preserves the original unscoped behavior (walk everything) — used by
// LoadGenericSkill, which has no ProviderConfig to scope by.
//
// Symlinks are NEVER followed: a malicious `relay skill install <path>`
// source package could contain e.g. a resource file that's actually a
// symlink to ~/.ssh/id_rsa, and Project()/Write() would then happily copy
// that target's content into every provider's directory. Each walked entry
// is Lstat'd before being read; a symlink (or any other non-regular-file
// entry — device files, sockets, etc.) is skipped rather than read, and its
// relative path is collected in the returned skipped slice so the caller can
// surface a warning instead of the file silently vanishing without a trace.
func loadResources(skillDir string, exclude map[string]bool, resourceDirs []string) (resources map[string][]byte, skipped []string, err error) {
	resources = map[string][]byte{}
	allowedDirs := make(map[string]bool, len(resourceDirs))
	for _, d := range resourceDirs {
		allowedDirs[d] = true
	}
	walkErr := filepath.WalkDir(skillDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(skillDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		topLevel := rel
		if idx := strings.Index(rel, "/"); idx >= 0 {
			topLevel = rel[:idx]
		}
		if len(allowedDirs) > 0 && rel != "." && !allowedDirs[topLevel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}
		if exclude[rel] {
			return nil
		}

		info, lstatErr := os.Lstat(path)
		if lstatErr != nil {
			return lstatErr
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			skipped = append(skipped, rel)
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		resources[rel] = data
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	return resources, skipped, nil
}

// warnSkippedResources prints a warning to stderr listing resource files
// loadResources skipped (symlinks or other non-regular files) so a
// suspicious or malformed skill package doesn't silently lose files without
// the user noticing.
func warnSkippedResources(skillDir string, skipped []string) {
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "agentport: skipped %d symlinked/non-regular resource(s) in %s: %v\n", len(skipped), skillDir, skipped)
}

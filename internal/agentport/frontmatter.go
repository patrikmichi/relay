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
// relative-path -> ResourceFile map. Returns an empty (non-nil) map if
// skillDir has no resource files.
//
// R10 fix: every regular file under the root is preserved regardless of
// which directory it lives in (or whether it lives in one at all) —
// recommended resource-directory names (scripts/, references/, assets/)
// are documentation, not an exhaustive schema, so a stray top-level helper
// script or a custom subdirectory is no longer silently dropped. The only
// files ever excluded are exclude's exact paths (the skill's own manifest
// file, a provider sidecar) and controlMetadataNames (generated/VCS state —
// see its doc comment); every such exclusion is reported in the returned
// excluded slice, never silent.
//
// skillDir itself is resolved through exactly one level of symlink first
// (resolveSkillRoot) — R12: an explicitly-selected skill root that happens
// to be a symlink (e.g. a dotfiles-managed skill directory) must still have
// its resources discovered; filepath.WalkDir does not descend into a
// symlinked root on its own. A symlink found ANYWHERE NESTED inside that
// resolved root, by contrast, is never followed: a malicious `relay skill
// install <path>` source package could contain e.g. a resource file that's
// actually a symlink to ~/.ssh/id_rsa, and Project()/Write() would then
// happily copy that target's content into every provider's directory. Each
// walked entry is Lstat'd before being read; a symlink (or any other
// non-regular-file entry — device files, sockets, etc.) is skipped rather
// than read, and recorded in excluded so the caller can surface it — and so
// a fidelity/loss report can name it — instead of the file silently
// vanishing without a trace.
func loadResources(skillDir string, exclude map[string]bool) (resources map[string]ResourceFile, excluded []ExcludedResource, err error) {
	root, resolveErr := resolveSkillRoot(skillDir)
	if resolveErr != nil {
		return nil, nil, resolveErr
	}

	resources = map[string]ResourceFile{}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if controlMetadataNames[d.Name()] {
			excluded = append(excluded, ExcludedResource{Path: rel, Reason: "generated/control metadata (" + d.Name() + ")"})
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
			excluded = append(excluded, ExcludedResource{Path: rel, Reason: "symlinked or non-regular file, refused (untrusted resource link)"})
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		resources[rel] = ResourceFile{Data: data, Mode: safeMode(info.Mode())}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	return resources, excluded, nil
}

// warnExcludedResources prints a warning to stderr listing resource files
// loadResources excluded (symlinks, non-regular files, or recognized
// control/generated metadata) so a suspicious or unusual skill package
// doesn't silently lose files without the user noticing at load time — the
// same information is also available structurally via
// Skill.ExcludedResources / a Project() loss report.
func warnExcludedResources(skillDir string, excluded []ExcludedResource) {
	if len(excluded) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "agentport: excluded %d resource(s) in %s:\n", len(excluded), skillDir)
	for _, e := range excluded {
		fmt.Fprintf(os.Stderr, "  %s: %s\n", e.Path, e.Reason)
	}
}

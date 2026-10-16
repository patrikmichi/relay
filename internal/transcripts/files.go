package transcripts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/patrikmichi/zuano/internal/nofollow"
)

// ReadFile reads a transcript the person named. It refuses a symlink, a
// device or a pipe, and a file larger than max.
func ReadFile(p string, max int64) ([]byte, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.OpenFile(p, nofollow.ReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readCapped(f, max)
}

func readCapped(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("the transcript is larger than %d bytes", max)
	}
	return data, nil
}

// Locations says where each agent keeps its sessions on this machine.
type Locations struct {
	Home    string
	GOOS    string
	AppData string
}

// Dir is the data directory of a source, or "" when the source has none.
type Dir struct{ Source, Path string }

// Dirs lists the session directories the CLI knows.
func (l Locations) Dirs() []Dir {
	dirs := []Dir{
		{SourceClaudeCode, filepath.Join(l.Home, ".claude", "projects")},
		{SourceCodex, filepath.Join(l.Home, ".codex", "sessions")},
		{SourceGemini, filepath.Join(l.Home, ".gemini", "tmp")},
	}
	var support string
	switch l.GOOS {
	case "darwin":
		support = filepath.Join(l.Home, "Library", "Application Support")
	case "windows":
		support = l.AppData
	default:
		support = filepath.Join(l.Home, ".config")
	}
	if support != "" {
		for _, editor := range []string{"Code", "Cursor", "VSCodium"} {
			dirs = append(dirs, Dir{SourceCline, filepath.Join(support, editor, "User", "globalStorage", "saoudrizwan.claude-dev", "tasks")})
		}
	}
	return dirs
}

// Found is a session file on this machine.
type Found struct {
	Source  string
	Root    string
	Rel     string
	Mod     time.Time
	Project string
	Preview string
}

// Path is the file's location.
func (f Found) Path() string { return filepath.Join(f.Root, filepath.FromSlash(f.Rel)) }

// Open reads the file through its data directory, so a link that leaves the
// directory is refused.
func (f Found) Open(max int64) ([]byte, error) {
	root, err := os.OpenRoot(f.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(f.Rel, nofollow.ReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readCapped(file, max)
}

// matches says whether a path below a source's directory is a session.
func matches(source, rel string) bool {
	parts := strings.Split(rel, "/")
	base := parts[len(parts)-1]
	switch source {
	case SourceClaudeCode:
		return len(parts) == 2 && strings.HasSuffix(base, ".jsonl")
	case SourceCodex:
		return len(parts) == 4 && strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
	case SourceGemini:
		return len(parts) == 3 && parts[1] == "chats" && strings.HasPrefix(base, "session-") && strings.HasSuffix(base, ".json")
	case SourceCline:
		return len(parts) == 2 && base == "api_conversation_history.json"
	}
	return false
}

const maxDiscovered = 5000

// Discover lists the newest sessions on this machine, up to limit. It looks
// only inside each agent's data directory and never follows a link out of it.
// The first user line of each is read here, to be shown here.
func Discover(l Locations, limit int, maxBytes int64) []Found {
	var all []Found
	for _, d := range l.Dirs() {
		root, err := os.OpenRoot(d.Path)
		if err != nil {
			continue
		}
		seen := 0
		_ = fs.WalkDir(root.FS(), ".", func(p string, e fs.DirEntry, err error) error {
			if err != nil || seen >= maxDiscovered {
				return fs.SkipDir
			}
			if e.Type().IsRegular() && matches(d.Source, p) {
				if info, err := e.Info(); err == nil {
					all = append(all, Found{Source: d.Source, Root: d.Path, Rel: p, Mod: info.ModTime()})
					seen++
				}
			}
			return nil
		})
		root.Close()
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Mod.After(all[j].Mod) })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	for i := range all {
		f := &all[i]
		f.Project = path.Base(path.Dir(f.Rel))
		if data, err := f.Open(maxBytes); err == nil {
			if s, err := Read(f.Source, data, ""); err == nil {
				f.Preview = firstUserLine(s)
				if s.CwdHint != "" {
					f.Project = s.CwdHint
				}
			}
		}
	}
	return all
}

func firstUserLine(s *Session) string {
	for _, t := range s.Turns {
		if t.Role == "user" && t.Text != "" {
			line, _, _ := strings.Cut(t.Text, "\n")
			if r := []rune(line); len(r) > 100 {
				line = string(r[:100])
			}
			return line
		}
	}
	return ""
}

// Readable reports which sources have a session directory this process can
// list, without reading any session.
func (l Locations) Readable() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range l.Dirs() {
		if seen[d.Source] {
			continue
		}
		if _, err := os.ReadDir(d.Path); err == nil {
			seen[d.Source] = true
			out = append(out, d.Source)
		}
	}
	return out
}

// runOpenCode runs `opencode export`; a variable so tests need no program.
var runOpenCode = func(ctx context.Context, id string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "opencode", "export", id)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("opencode export %s failed: %s", id, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("run opencode: %w", err)
	}
	return out, nil
}

// ExportOpenCode returns the export of an OpenCode session. The id is passed
// as one argument, never through a shell.
func ExportOpenCode(ctx context.Context, id string) ([]byte, error) {
	if id == "" || strings.HasPrefix(id, "-") || strings.ContainsAny(id, " \t\r\n") {
		return nil, fmt.Errorf("%q is not an OpenCode session id", id)
	}
	return runOpenCode(ctx, id)
}

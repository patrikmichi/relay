package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/patrikmichi/zuano/internal/textsafe"
	"github.com/patrikmichi/zuano/internal/transcripts"
)

const (
	maxTranscriptBytes = 256 << 20
	pickListSize       = 15
)

// transcriptLocations is where this machine's agents keep sessions; a
// variable so tests can point it at a temporary home.
var transcriptLocations = func() transcripts.Locations {
	home, _ := os.UserHomeDir()
	return transcripts.Locations{Home: home, GOOS: runtime.GOOS, AppData: os.Getenv("APPDATA")}
}

// discoverSessions lists recent sessions, optionally of one source.
func discoverSessions(source string) []transcripts.Found {
	found := transcripts.Discover(transcriptLocations(), 200, maxTranscriptBytes)
	if source == "" {
		return found
	}
	var out []transcripts.Found
	for _, f := range found {
		if f.Source == source {
			out = append(out, f)
		}
	}
	return out
}

// newestSession is the newest session of a source, preferring one from the
// current project.
func newestSession(source string) (transcripts.Found, bool) {
	found := discoverSessions(source)
	if len(found) == 0 {
		return transcripts.Found{}, false
	}
	if cwd, err := os.Getwd(); err == nil {
		for _, f := range found {
			if f.Project == filepath.Base(cwd) {
				return f, true
			}
		}
	}
	return found[0], true
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// pickSession lists recent local sessions and asks for one. The first line
// of each is shown here and goes nowhere else.
func pickSession(in io.Reader, out io.Writer, source string) (transcripts.Found, error) {
	found := discoverSessions(source)
	if len(found) > pickListSize {
		found = found[:pickListSize]
	}
	if len(found) == 0 {
		return transcripts.Found{}, errors.New("no sessions were found on this machine; name a file")
	}
	for i, f := range found {
		fmt.Fprintf(out, "%2d  %-11s %-16s %-9s %s\n", i+1, textsafe.Clean(f.Source), textsafe.Clean(f.Project), ago(f.Mod), textsafe.Clean(f.Preview))
	}
	fmt.Fprintf(out, "Session number (1-%d): ", len(found))
	line, _ := bufio.NewReader(in).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(found) {
		return transcripts.Found{}, errors.New("no session chosen")
	}
	return found[n-1], nil
}

// chooseConversation asks which conversation of an export to use.
func chooseConversation(in io.Reader, out io.Writer, source string, data []byte) (string, error) {
	convs, err := transcripts.Conversations(source, data)
	if err != nil {
		return "", err
	}
	if len(convs) <= 1 {
		return "", nil
	}
	for i, c := range convs {
		fmt.Fprintf(out, "%3d  %s\n", i+1, textsafe.Clean(c.Title))
	}
	fmt.Fprintf(out, "Conversation number (1-%d): ", len(convs))
	line, _ := bufio.NewReader(in).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(convs) {
		return "", errors.New("no conversation chosen")
	}
	return convs[n-1].ID, nil
}

// doctorTranscripts lists the agents whose session directories can be read,
// without reading any session.
func doctorTranscripts(add func(string, bool, string)) {
	readable := transcriptLocations().Readable()
	if len(readable) == 0 {
		add("transcripts", true, "no agent session directories found")
		return
	}
	add("transcripts", true, "session directories readable for: "+strings.Join(readable, ", "))
}

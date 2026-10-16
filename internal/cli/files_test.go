package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fixtureFileHash = "2c8b3b4d5d0a7a5b0a3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192"
	fileTestID      = "fil_abc123def456"
)

var fileTestBody = []byte("hello files")

func fileTestHash() string {
	sum := sha256.Sum256(fileTestBody)
	return hex.EncodeToString(sum[:])
}

// fileGateway serves the file routes and the object store behind them.
type fileGateway struct {
	mu          sync.Mutex
	srv         *httptest.Server
	created     map[string]any
	uploaded    []byte
	putHeaders  http.Header
	putAuth     string
	gets        int
	states      []string // states GET /api/files/{id} walks through
	downloadSHA string
	deleted     []string
	tamper      bool
}

func newFileGateway(t *testing.T) *fileGateway {
	t.Helper()
	g := &fileGateway{states: []string{"scanning", "ready"}, downloadSHA: fileTestHash()}
	fill := func(s string) string {
		return strings.NewReplacer("https://store.example", g.srv.URL+"/store", fixtureFileHash, g.downloadSHA, "https://gateway.example", g.srv.URL).Replace(s)
	}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/files":
			_ = json.NewDecoder(r.Body).Decode(&g.created)
			_, _ = io.WriteString(w, fill(contractFixture(t, "file-create.json")))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/store/files/"):
			g.uploaded, _ = io.ReadAll(r.Body)
			g.putHeaders, g.putAuth = r.Header.Clone(), r.Header.Get("Authorization")
		case r.Method == http.MethodPost && r.URL.Path == "/api/files/"+fileTestID+"/complete":
			_, _ = io.WriteString(w, `{"file":{"id":"`+fileTestID+`","state":"uploaded"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/files/"+fileTestID:
			doc := fill(contractFixture(t, "file-meta.json"))
			if len(g.states) > 0 {
				state := g.states[min(g.gets, len(g.states)-1)]
				g.gets++
				doc = strings.Replace(doc, `"state":"ready"`, `"state":"`+state+`"`, 1)
			}
			_, _ = io.WriteString(w, doc)
		case r.Method == http.MethodGet && r.URL.Path == "/api/files/"+fileTestID+"/download":
			if r.Header.Get("Authorization") == "" {
				t.Error("the download request carried no credentials")
			}
			http.Redirect(w, r, g.srv.URL+"/store/dl/"+fileTestID+"?sig=1", http.StatusFound)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/store/dl/"):
			if r.Header.Get("Authorization") != "" {
				t.Error("the object store was sent the gateway credential")
			}
			if g.tamper {
				_, _ = w.Write([]byte("HELLO FILES"))
				return
			}
			_, _ = w.Write(fileTestBody)
		case r.Method == http.MethodGet && r.URL.Path == "/api/files":
			_, _ = io.WriteString(w, fill(contractFixture(t, "files-list.json")))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/files/"):
			g.deleted = append(g.deleted, strings.TrimPrefix(r.URL.Path, "/api/files/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	withAPIKeyGateway(t, g.srv)
	t.Setenv("ZUANO_AGENT", "")
	filePollInterval = time.Millisecond
	t.Cleanup(func() { filePollInterval = time.Second })
	return g
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFilesUpload_SendsTheBytesToTheStoreAndWaitsUntilReady(t *testing.T) {
	g := newFileGateway(t)
	path := writeTemp(t, "report.txt", fileTestBody)
	out, err := runCommand(t, FilesCmd(), "upload", path, "--share-org")
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, fileTestID+"  ready") {
		t.Errorf("output = %q", out)
	}
	if string(g.uploaded) != string(fileTestBody) || g.putAuth != "" || g.putHeaders.Get("x-ms-blob-type") != "BlockBlob" {
		t.Errorf("store got %q, auth %q, headers %v", g.uploaded, g.putAuth, g.putHeaders)
	}
	if g.created["name"] != "report.txt" || g.created["sha256"] != fileTestHash() || g.created["size"] != float64(11) || g.created["share_with_org"] != true || g.created["content_type"] != "text/plain; charset=utf-8" {
		t.Errorf("create body = %v", g.created)
	}
	if g.gets != 2 {
		t.Errorf("polled %d times, want scanning then ready", g.gets)
	}
}

func TestFilesUpload_NoWaitDoesNotPoll(t *testing.T) {
	g := newFileGateway(t)
	out, err := runCommand(t, FilesCmd(), "upload", writeTemp(t, "a.txt", fileTestBody), "--no-wait", "--json")
	if err != nil || g.gets != 0 || !strings.Contains(out, `"id":"`+fileTestID+`"`) {
		t.Fatalf("polls %d, err %v\n%s", g.gets, err, out)
	}
}

func TestFilesUpload_AQuarantinedFileIsAnErrorAndAnUnsafeStoreAddressIsRefused(t *testing.T) {
	g := newFileGateway(t)
	g.states = []string{"quarantined"}
	out, err := runCommand(t, FilesCmd(), "upload", writeTemp(t, "a.txt", fileTestBody))
	if err == nil || !strings.Contains(out, "quarantined") {
		t.Fatalf("err %v\n%s", err, out)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"file_id":"`+fileTestID+`","upload":{"url":"http://store.example.com/x","method":"PUT"}}`)
	}))
	defer srv.Close()
	withAPIKeyGateway(t, srv)
	if _, err := runCommand(t, FilesCmd(), "upload", writeTemp(t, "b.txt", fileTestBody)); err == nil {
		t.Error("an http storage address on a public host was used")
	}
	if _, err := runCommand(t, FilesCmd(), "upload", t.TempDir()); err == nil {
		t.Error("a directory was uploaded")
	}
	if _, err := runCommand(t, FilesCmd(), "upload", "a", "b", "--name", "x"); err == nil {
		t.Error("--name with two files was accepted")
	}
}

func TestFilesDownload_FollowsTheRedirectWithoutTheCredentialAndChecksTheHash(t *testing.T) {
	g := newFileGateway(t)
	dir := t.TempDir()
	out, err := runCommand(t, FilesCmd(), "download", "file:"+fileTestID, "-o", dir)
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "report.txt"))
	if err != nil || string(got) != string(fileTestBody) {
		t.Fatalf("file = %q, %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "report.txt")); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	if _, err := runCommand(t, FilesCmd(), "download", g.srv.URL+"/api/files/"+fileTestID, "-o", dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("overwrite err = %v", err)
	}
	if _, err := runCommand(t, FilesCmd(), "download", fileTestID, "-o", dir, "--force"); err != nil {
		t.Errorf("--force err = %v", err)
	}

	g.tamper = true
	other := filepath.Join(t.TempDir(), "out.bin")
	if _, err := runCommand(t, FilesCmd(), "download", fileTestID, "-o", other); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("tampered err = %v", err)
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("a download that failed its hash was saved")
	}
	entries, _ := os.ReadDir(filepath.Dir(other))
	if len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestFilesDownload_NeverWritesOutsideItsDirectory(t *testing.T) {
	meta := fileDoc{ID: fileTestID, Name: "../../etc/passwd"}
	dir := t.TempDir()
	if got, _ := downloadTarget(filepath.Join(dir, "new.bin"), meta); got != filepath.Join(dir, "new.bin") {
		t.Errorf("a destination that is not a directory = %q", got)
	}
	got, err := downloadTarget(dir, meta)
	if err != nil || filepath.Dir(got) != dir || filepath.Base(got) != "passwd" {
		t.Errorf("target = %q, %v", got, err)
	}
	meta.Name = ".."
	if got, _ := downloadTarget(dir, meta); filepath.Base(got) != fileTestID {
		t.Errorf("target = %q", got)
	}
}

func TestFilesListAndRm(t *testing.T) {
	g := newFileGateway(t)
	out, err := runCommand(t, FilesCmd(), "list")
	if err != nil || !strings.Contains(out, fileTestID) || !strings.Contains(out, "report.txt") || !strings.Contains(out, "clean") {
		t.Fatalf("list: %q, %v", out, err)
	}
	out, err = runCommand(t, FilesCmd(), "rm", fileTestID)
	if err != nil || len(g.deleted) != 1 || g.deleted[0] != fileTestID || !strings.Contains(out, "Deleted") {
		t.Fatalf("rm: %q, %v, %v", out, err, g.deleted)
	}
	if _, err := runCommand(t, FilesCmd(), "rm", "not-a-file"); err == nil {
		t.Error("a bad id was accepted")
	}
}

func TestParseFileRef(t *testing.T) {
	for in, want := range map[string]string{
		fileTestID:           fileTestID,
		"file:" + fileTestID: fileTestID,
		"https://gw.example/api/files/" + fileTestID:               fileTestID,
		"https://gw.example/api/files/" + fileTestID + "/download": fileTestID,
	} {
		if got, err := parseFileRef(in); err != nil || got != want {
			t.Errorf("parseFileRef(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "fil_", "abc", "file:../x", "https://gw.example/"} {
		if _, err := parseFileRef(bad); err == nil {
			t.Errorf("parseFileRef(%q) accepted", bad)
		}
	}
}

func TestCall_FileUploadsFirstAndPassesAReference(t *testing.T) {
	g := newFileGateway(t)
	var gotArgs map[string]any
	inner := g.srv.Config.Handler
	g.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/drive/mcp" {
			var rpc struct {
				Method string `json:"method"`
				Params struct {
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&rpc)
			if rpc.Method != "tools/call" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			gotArgs = rpc.Params.Arguments
			_, _ = io.WriteString(w, strings.NewReplacer("https://gateway.example", g.srv.URL, "https://store.example", g.srv.URL+"/store").Replace(contractFixture(t, "mcp-file-result.json")))
			return
		}
		inner.ServeHTTP(w, r)
	})
	path := writeTemp(t, "report.txt", fileTestBody)
	saveDir := filepath.Join(t.TempDir(), "saved")
	out, err := runCommand(t, CallCmd(), "drive", "upload_file", "--arg", "folder=root", "--file", "file="+"@"+path, "--save-files", saveDir)
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	if gotArgs["file"] != "file:"+fileTestID || gotArgs["folder"] != "root" {
		t.Errorf("arguments = %v", gotArgs)
	}
	if !strings.Contains(out, "File: report.txt "+g.srv.URL+"/api/files/"+fileTestID+" (text/plain, 11 bytes)") {
		t.Errorf("resource link not rendered:\n%s", out)
	}
	saved, err := os.ReadFile(filepath.Join(saveDir, "report.txt"))
	if err != nil || string(saved) != string(fileTestBody) || !strings.Contains(out, "Saved "+fileTestID) {
		t.Errorf("saved %q, %v\n%s", saved, err, out)
	}
	if _, err := runCommand(t, CallCmd(), "drive", "upload_file", "--arg", "file=x", "--file", "file=@"+path); err == nil || !strings.Contains(err.Error(), "given twice") {
		t.Errorf("duplicate err = %v", err)
	}
	if _, err := runCommand(t, CallCmd(), "drive", "upload_file", "--file", "nopath"); err == nil || !strings.Contains(err.Error(), "param=@path") {
		t.Errorf("bad --file err = %v", err)
	}
}

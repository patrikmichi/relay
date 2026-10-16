package evidence

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/signing"
	"github.com/patrikmichi/zuano/internal/signing/signingtest"
)

// bundle describes a bundle to build; each field can be spoiled by a test.
type bundle struct {
	t        *testing.T
	evidence *signingtest.Signer
	audit    *signingtest.Signer
	files    map[string][]byte
	listed   map[string]bool
	events   []map[string]any
	cps      []checkpoint
	manifest Manifest
	head     string
	phantom  []FileEntry
}

func canon(t *testing.T, v any) []byte {
	t.Helper()
	b, err := signing.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newBundle builds events 5 to 9 chained after a start checkpoint at 4, with
// checkpoints at 4, 7 and 9.
func newBundle(t *testing.T) *bundle {
	b := &bundle{t: t, evidence: signingtest.New("ev1"), audit: signingtest.New("au1"), files: map[string][]byte{}, listed: map[string]bool{}}
	prev := hex.EncodeToString(make([]byte, 32))
	heads := map[int64]string{4: prev}
	for seq := int64(5); seq <= 9; seq++ {
		ev := map[string]any{"seq": seq, "org_id": "org_1", "action": fmt.Sprintf("tool.call.%d", seq), "ts": "2026-10-01T10:00:00Z"}
		prevBytes, _ := hex.DecodeString(prev)
		sum := sha256.Sum256(append(prevBytes, canon(t, ev)...))
		h := hex.EncodeToString(sum[:])
		ev["prev_hash"], ev["hash"] = prev, h
		b.events = append(b.events, ev)
		heads[seq], prev = h, h
	}
	b.head = prev
	for _, c := range []struct {
		seq  int64
		date string
	}{{4, "2026-09-30"}, {7, "2026-10-01"}, {9, "2026-10-02"}} {
		cp := checkpoint{OrgID: "org_1", Date: c.date, LastSeq: c.seq, HeadHash: heads[c.seq], KID: "au1"}
		body := canon(t, map[string]any{"org_id": cp.OrgID, "date": cp.Date, "last_seq": cp.LastSeq, "head_hash": cp.HeadHash})
		cp.Signature = base64.StdEncoding.EncodeToString(b.audit.SignMessage(body))
		b.cps = append(b.cps, cp)
	}
	b.manifest = Manifest{Pack: "soc2", PackVersion: "1", OrgID: "org_1", OrgSlug: "acme", EvidenceKeyID: "ev1"}
	b.manifest.Range.From, b.manifest.Range.To = "2026-10-01", "2026-10-03"
	return b
}

func (b *bundle) jsonl(v any) []byte {
	var sb strings.Builder
	switch t := v.(type) {
	case []map[string]any:
		for _, x := range t {
			raw, _ := json.Marshal(x)
			sb.Write(raw)
			sb.WriteString("\n")
		}
	case []checkpoint:
		for _, x := range t {
			raw, _ := json.Marshal(x)
			sb.Write(raw)
			sb.WriteString("\n")
		}
	}
	return []byte(sb.String())
}

// write assembles the archive and returns its path.
func (b *bundle) write() string {
	t := b.t
	if _, ok := b.files["chain/audit_chain.jsonl"]; !ok {
		b.files["chain/audit_chain.jsonl"] = b.jsonl(b.events)
	}
	if _, ok := b.files["chain/checkpoints.jsonl"]; !ok {
		b.files["chain/checkpoints.jsonl"] = b.jsonl(b.cps)
	}
	if _, ok := b.files["chain/verification.json"]; !ok {
		b.files["chain/verification.json"] = []byte(fmt.Sprintf(`{"ok":true,"from_seq":5,"to_seq":9,"recomputed_head":%q}`, b.head))
	}
	if _, ok := b.files["controls.csv"]; !ok {
		b.files["controls.csv"] = []byte("control_id,status\nCC6.1,covered\n")
	}
	b.manifest.Files = nil
	names := make([]string, 0, len(b.files))
	for n := range b.files {
		names = append(names, n)
	}
	for _, n := range names {
		if b.listed[n] {
			continue
		}
		sum := sha256.Sum256(b.files[n])
		b.manifest.Files = append(b.manifest.Files, FileEntry{Path: n, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b.files[n]))})
	}
	b.manifest.Files = append(b.manifest.Files, b.phantom...)
	manifest, _ := json.Marshal(b.manifest)
	sig := base64.StdEncoding.EncodeToString(b.evidence.SignMessage(manifest))
	path := filepath.Join(t.TempDir(), "bundle.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	put := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	put(ManifestFile, manifest)
	put(SignatureFile, []byte(sig+"\n"))
	for n, data := range b.files {
		put(n, data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return path
}

func (b *bundle) keys() Keys {
	return Keys{Evidence: []signing.Key{b.evidence.Key}, Audit: []signing.Key{b.audit.Key}}
}

func verify(t *testing.T, b *bundle) *Result {
	t.Helper()
	res, err := Verify(b.write(), b.keys())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func failures(r *Result) string {
	var parts []string
	for _, f := range r.Failures {
		parts = append(parts, f.String())
	}
	return strings.Join(parts, "\n")
}

func TestAValidBundleVerifies(t *testing.T) {
	r := verify(t, newBundle(t))
	if !r.OK() || r.Events != 5 || r.Checkpoints != 3 || r.Files != 4 || len(r.Warnings) != 0 || r.Manifest.Pack != "soc2" {
		t.Fatalf("result = %+v\n%s", r, failures(r))
	}
}

func TestATamperedFileIsNamed(t *testing.T) {
	b := newBundle(t)
	b.write()
	b.files["controls.csv"] = []byte("control_id,status\nCC6.1,gap\n")
	good := b.files["controls.csv"]
	// Build with the original, then replace the bytes.
	b.files["controls.csv"] = []byte("control_id,status\nCC6.1,covered\n")
	path := b.write()
	rewritten := rewrite(t, path, "controls.csv", good)
	res, err := Verify(rewritten, b.keys())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() || !strings.Contains(failures(res), "controls.csv") {
		t.Errorf("failures = %q", failures(res))
	}
}

// rewrite copies an archive with one file's bytes replaced.
func rewrite(t *testing.T, src, name string, data []byte) string {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	dst := filepath.Join(t.TempDir(), "tampered.zip")
	f, _ := os.Create(dst)
	zw := zip.NewWriter(f)
	for _, zf := range zr.File {
		w, _ := zw.Create(zf.Name)
		if zf.Name == name {
			_, _ = w.Write(data)
			continue
		}
		rc, _ := zf.Open()
		buf := make([]byte, zf.UncompressedSize64)
		n, _ := rc.Read(buf)
		for n < len(buf) {
			m, err := rc.Read(buf[n:])
			n += m
			if err != nil {
				break
			}
		}
		_ = rc.Close()
		_, _ = w.Write(buf)
	}
	_ = zw.Close()
	_ = f.Close()
	return dst
}

func TestATamperedAuditRowIsNamedBySequence(t *testing.T) {
	b := newBundle(t)
	b.events[2]["action"] = "tool.call.EDITED"
	b.files["chain/audit_chain.jsonl"] = b.jsonl(b.events)
	r := verify(t, b)
	got := failures(r)
	if r.OK() || !strings.Contains(got, "event 7: the event's hash does not match its content") || !strings.Contains(got, "event 8: prev_hash is not the hash of event 7") {
		t.Errorf("failures = %q", got)
	}
	if !strings.Contains(got, "chain recomputed to a different head than the checkpoint of 2026-10-01") {
		t.Errorf("the checkpoint at 7 did not notice: %q", got)
	}
}

func TestAMissingEventBreaksTheSequence(t *testing.T) {
	b := newBundle(t)
	b.events = append(b.events[:2:2], b.events[3:]...)
	b.files["chain/audit_chain.jsonl"] = b.jsonl(b.events)
	if got := failures(verify(t, b)); !strings.Contains(got, "event 8: the sequence jumps from 6") {
		t.Errorf("failures = %q", got)
	}
}

func TestChainMustStartWhereTheStartCheckpointEnds(t *testing.T) {
	b := newBundle(t)
	b.cps[0].HeadHash = strings.Repeat("ab", 32)
	body := canon(t, map[string]any{"org_id": "org_1", "date": b.cps[0].Date, "last_seq": b.cps[0].LastSeq, "head_hash": b.cps[0].HeadHash})
	b.cps[0].Signature = base64.StdEncoding.EncodeToString(b.audit.SignMessage(body))
	if got := failures(verify(t, b)); !strings.Contains(got, "does not continue from the start checkpoint") {
		t.Errorf("failures = %q", got)
	}
	b = newBundle(t)
	b.cps = b.cps[1:]
	if got := failures(verify(t, b)); !strings.Contains(got, "no checkpoint anchors the start of the chain") {
		t.Errorf("no anchor: %q", got)
	}
}

func TestSignaturesMustVerifyWithTheGivenKeys(t *testing.T) {
	b := newBundle(t)
	path := b.write()
	other := signingtest.New("ev1")
	res, err := Verify(path, Keys{Evidence: []signing.Key{other.Key}, Audit: []signing.Key{b.audit.Key}})
	if err != nil || !strings.Contains(failures(res), "manifest.json: the signature does not verify") {
		t.Errorf("wrong evidence key: %v %q", err, failures(res))
	}
	res, err = Verify(path, Keys{Evidence: []signing.Key{b.evidence.Key}})
	if err != nil || !strings.Contains(failures(res), "checkpoint of 2026-10-01 does not verify") {
		t.Errorf("no audit keys: %v %q", err, failures(res))
	}
	b.cps[1].Signature = "!!"
	delete(b.files, "chain/checkpoints.jsonl")
	if got := failures(verify(t, b)); !strings.Contains(got, "the checkpoint signature cannot be read") {
		t.Errorf("garbled checkpoint signature: %q", got)
	}
}

func TestFilesOutsideTheManifestAndMissingFilesAreFailures(t *testing.T) {
	b := newBundle(t)
	b.files["extra.txt"] = []byte("x")
	b.listed["extra.txt"] = true
	b.phantom = []FileEntry{{Path: "data/missing.jsonl", SHA256: "00", Size: 1}}
	got := failures(verify(t, b))
	for _, want := range []string{"extra.txt: in the archive but not listed in the signed manifest", "data/missing.jsonl: listed in the manifest but missing"} {
		if !strings.Contains(got, want) {
			t.Errorf("no failure %q in:\n%s", want, got)
		}
	}
}

func TestFileSizeIsChecked(t *testing.T) {
	b := newBundle(t)
	b.files["controls.csv"] = []byte("abc")
	path := b.write()
	// A manifest that claims fewer bytes than the file holds.
	res := &Result{}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "controls.csv" {
			checkFile(res, f, FileEntry{Path: "controls.csv", SHA256: "00", Size: 1})
		}
	}
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0].Msg, "is 2 bytes; the manifest says 1") {
		t.Errorf("failures = %+v", res.Failures)
	}
}

func TestAnUnsignedChainTailIsAWarningAndTheGatewaysOwnFailureIsNot(t *testing.T) {
	b := newBundle(t)
	b.cps = b.cps[:2]
	r := verify(t, b)
	if !r.OK() || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "events 8 to 9 come after the last signed checkpoint") {
		t.Errorf("tail: %+v", r)
	}
	b = newBundle(t)
	b.files["chain/verification.json"] = []byte(`{"ok":false}`)
	if got := failures(verify(t, b)); !strings.Contains(got, "the gateway's own verification of the chain failed") {
		t.Errorf("gateway failure: %q", got)
	}
	b = newBundle(t)
	b.files["chain/verification.json"] = []byte(`{"ok":true,"to_seq":9,"recomputed_head":"` + strings.Repeat("cd", 32) + `"}`)
	if got := failures(verify(t, b)); !strings.Contains(got, "recorded head differs from the recomputed one") {
		t.Errorf("head: %q", got)
	}
	b = newBundle(t)
	b.files["chain/verification.json"] = []byte(`nope`)
	if got := failures(verify(t, b)); !strings.Contains(got, "chain/verification.json: cannot be read") {
		t.Errorf("garbled verification: %q", got)
	}
}

func TestGarbledChainFilesAreFailures(t *testing.T) {
	for name, files := range map[string]map[string][]byte{
		"bad event":      {"chain/audit_chain.jsonl": []byte("{nope\n")},
		"no seq":         {"chain/audit_chain.jsonl": []byte(`{"hash":"00","prev_hash":"00"}` + "\n")},
		"bad prev hash":  {"chain/audit_chain.jsonl": []byte(`{"seq":5,"hash":"00","prev_hash":"zz"}` + "\n")},
		"bad checkpoint": {"chain/checkpoints.jsonl": []byte("{nope\n")},
	} {
		b := newBundle(t)
		for k, v := range files {
			b.files[k] = v
		}
		if r := verify(t, b); r.OK() {
			t.Errorf("%s verified", name)
		}
	}
	b := newBundle(t)
	b.files["chain/audit_chain.jsonl"] = []byte("")
	if r := verify(t, b); r.Events != 0 {
		t.Errorf("empty chain: %+v", r)
	}
}

func TestCheckpointsWithoutAChainAreAFailure(t *testing.T) {
	b := newBundle(t)
	b.files["chain/audit_chain.jsonl"] = []byte{}
	path := b.write()
	// Rebuild the archive without the (empty) chain file.
	rewritten := dropFile(t, path, "chain/audit_chain.jsonl")
	res, err := Verify(rewritten, b.keys())
	if err != nil || !strings.Contains(failures(res), "the bundle has checkpoints but no audit chain") {
		t.Errorf("%v %q", err, failures(res))
	}
}

func dropFile(t *testing.T, src, name string) string {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	dst := filepath.Join(t.TempDir(), "dropped.zip")
	f, _ := os.Create(dst)
	zw := zip.NewWriter(f)
	for _, zf := range zr.File {
		if zf.Name == name {
			continue
		}
		w, _ := zw.Create(zf.Name)
		rc, _ := zf.Open()
		buf := make([]byte, zf.UncompressedSize64)
		n := 0
		for n < len(buf) {
			m, err := rc.Read(buf[n:])
			n += m
			if err != nil {
				break
			}
		}
		_ = rc.Close()
		_, _ = w.Write(buf)
	}
	_ = zw.Close()
	_ = f.Close()
	return dst
}

func TestSplitChainPartsAreReadInOrder(t *testing.T) {
	b := newBundle(t)
	b.files["chain/audit_chain.part-001.jsonl"] = b.jsonl(b.events[:3])
	b.files["chain/audit_chain.part-002.jsonl"] = b.jsonl(b.events[3:])
	// write() recreates the default chain file unless one is supplied; an
	// empty one leaves only the parts to count.
	b.files["chain/audit_chain.jsonl"] = []byte{}
	r := verify(t, b)
	if !r.OK() || r.Events != 5 {
		t.Errorf("parts: %+v\n%s", r, failures(r))
	}
}

func TestAnArchiveThatIsNotABundleIsAnError(t *testing.T) {
	dir := t.TempDir()
	if _, err := Verify(filepath.Join(dir, "none.zip"), Keys{}); err == nil {
		t.Error("a missing file verified")
	}
	bad := filepath.Join(dir, "bad.zip")
	_ = os.WriteFile(bad, []byte("not a zip"), 0o600)
	if _, err := Verify(bad, Keys{}); err == nil {
		t.Error("a non-zip verified")
	}
	empty := filepath.Join(dir, "empty.zip")
	f, _ := os.Create(empty)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("something.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	_ = f.Close()
	if _, err := Verify(empty, Keys{}); err == nil || !strings.Contains(err.Error(), "not a compliance bundle") {
		t.Errorf("no manifest: %v", err)
	}
	garbled := newBundle(t)
	p := rewrite(t, garbled.write(), ManifestFile, []byte("{"))
	if _, err := Verify(p, garbled.keys()); err == nil || !strings.Contains(err.Error(), "manifest.json is not valid") {
		t.Errorf("garbled manifest: %v", err)
	}
	p = rewrite(t, garbled.write(), SignatureFile, []byte("!!"))
	if res, err := Verify(p, garbled.keys()); err != nil || !strings.Contains(failures(res), "the signature is not base64") {
		t.Errorf("garbled signature: %v %q", err, failures(res))
	}
}

func TestUnsafeAndRepeatedNamesAreFailures(t *testing.T) {
	b := newBundle(t)
	path := b.write()
	f, _ := os.Create(filepath.Join(t.TempDir(), "evil.zip"))
	zr, _ := zip.OpenReader(path)
	zw := zip.NewWriter(f)
	for _, zf := range zr.File {
		w, _ := zw.Create(zf.Name)
		rc, _ := zf.Open()
		buf := make([]byte, zf.UncompressedSize64)
		n := 0
		for n < len(buf) {
			m, err := rc.Read(buf[n:])
			n += m
			if err != nil {
				break
			}
		}
		_ = rc.Close()
		_, _ = w.Write(buf)
	}
	for _, name := range []string{"../escape.txt", "/abs.txt", "a/../b.txt", "controls.csv"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	_ = f.Close()
	zr.Close()
	res, err := Verify(f.Name(), b.keys())
	if err != nil {
		t.Fatal(err)
	}
	got := failures(res)
	for _, want := range []string{"../escape.txt: an unsafe file name", "/abs.txt: an unsafe file name", "a/../b.txt: an unsafe file name", "controls.csv: listed twice in the archive"} {
		if !strings.Contains(got, want) {
			t.Errorf("no failure %q in:\n%s", want, got)
		}
	}
}

func TestFailureStrings(t *testing.T) {
	for in, want := range map[Failure]string{
		{File: "f", Seq: 3, Msg: "m"}: "f, event 3: m",
		{File: "f", Msg: "m"}:         "f: m",
		{Msg: "m"}:                    "m",
	} {
		if in.String() != want {
			t.Errorf("%+v = %q", in, in.String())
		}
	}
}

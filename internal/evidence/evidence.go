// Package evidence checks a compliance export bundle on this machine, with no
// session: the manifest signature, every file's hash and the audit hash chain
// with its signed checkpoints.
package evidence

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/patrikmichi/zuano/internal/signing"
)

// Names inside a bundle.
const (
	ManifestFile  = "manifest.json"
	SignatureFile = "manifest.sig"
	checkpoints   = "chain/checkpoints.jsonl"
	verification  = "chain/verification.json"
	chainPrefix   = "evidence/audit_chain"
	maxEntries    = 200000
	maxSmallFile  = 64 << 20
	maxLine       = 16 << 20
	maxFailures   = 50
)

// FileEntry is a file the manifest lists.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the signed description of a bundle.
type Manifest struct {
	Pack           string `json:"pack"`
	PackVersion    string `json:"packVersion"`
	MappingVersion string `json:"mappingVersion"`
	OrgID          string `json:"orgId"`
	OrgSlug        string `json:"orgSlug"`
	Range          struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"range"`
	GeneratedAt   string      `json:"generatedAt"`
	RequestedBy   string      `json:"requestedBy"`
	EvidenceKeyID string      `json:"evidenceKeyId"`
	Files         []FileEntry `json:"files"`
}

// Failure is one thing that does not check out, naming the file or sequence
// number.
type Failure struct {
	File string
	Seq  int64
	Msg  string
}

func (f Failure) String() string {
	switch {
	case f.Seq > 0 && f.File != "":
		return fmt.Sprintf("%s, event %d: %s", f.File, f.Seq, f.Msg)
	case f.File != "":
		return f.File + ": " + f.Msg
	}
	return f.Msg
}

// Result is what verification found.
type Result struct {
	Manifest    Manifest
	Files       int
	Events      int64
	Checkpoints int
	Failures    []Failure
	Warnings    []string
}

// OK reports a bundle with no failures.
func (r *Result) OK() bool { return len(r.Failures) == 0 }

func (r *Result) fail(file string, seq int64, format string, args ...any) {
	if len(r.Failures) < maxFailures {
		r.Failures = append(r.Failures, Failure{file, seq, fmt.Sprintf(format, args...)})
	}
}

// Keys are the public keys a bundle is checked against.
type Keys struct {
	Evidence []signing.Key
	Audit    []signing.Key
}

func readSmall(f *zip.File) ([]byte, error) {
	if f.UncompressedSize64 > maxSmallFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", f.Name, maxSmallFile)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxSmallFile))
}

// Verify checks the bundle at zipPath. It returns an error only when the file
// cannot be read as a bundle at all; everything wrong inside a readable
// bundle is a Failure in the result.
func Verify(zipPath string, keys Keys) (*Result, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open the bundle: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > maxEntries {
		return nil, fmt.Errorf("the bundle has %d entries; the limit is %d", len(zr.File), maxEntries)
	}
	res := &Result{}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || path.Clean(name) != name || strings.HasPrefix(name, "../") {
			res.fail(name, 0, "an unsafe file name")
			continue
		}
		if _, dup := files[name]; dup {
			res.fail(name, 0, "listed twice in the archive")
			continue
		}
		files[name] = f
	}
	mf, sf := files[ManifestFile], files[SignatureFile]
	if mf == nil || sf == nil {
		return nil, errors.New("the archive is not a compliance bundle: manifest.json or manifest.sig is missing")
	}
	manifestBytes, err := readSmall(mf)
	if err != nil {
		return nil, err
	}
	sigBytes, err := readSmall(sf)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(manifestBytes, &res.Manifest); err != nil {
		return nil, fmt.Errorf("manifest.json is not valid: %w", err)
	}
	sig, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigBytes)))
	if derr != nil {
		res.fail(SignatureFile, 0, "the signature is not base64")
	} else if err := signing.VerifyMessage(keys.Evidence, res.Manifest.EvidenceKeyID, manifestBytes, sig); err != nil {
		res.fail(ManifestFile, 0, "the signature does not verify: %v", err)
	}

	listed := map[string]bool{ManifestFile: true, SignatureFile: true}
	for _, e := range res.Manifest.Files {
		listed[e.Path] = true
		f := files[e.Path]
		if f == nil {
			res.fail(e.Path, 0, "listed in the manifest but missing")
			continue
		}
		res.Files++
		checkFile(res, f, e)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !listed[name] {
			res.fail(name, 0, "in the archive but not listed in the signed manifest")
		}
	}
	verifyChain(res, files, names, keys.Audit)
	return res, nil
}

func checkFile(res *Result, f *zip.File, e FileEntry) {
	rc, err := f.Open()
	if err != nil {
		res.fail(e.Path, 0, "cannot be read: %v", err)
		return
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, e.Size+1))
	switch {
	case err != nil:
		res.fail(e.Path, 0, "cannot be read: %v", err)
	case n != e.Size:
		res.fail(e.Path, 0, "is %d bytes; the manifest says %d", n, e.Size)
	case hex.EncodeToString(h.Sum(nil)) != strings.ToLower(e.SHA256):
		res.fail(e.Path, 0, "its hash does not match the manifest")
	}
}

type checkpoint struct {
	OrgID     string `json:"org_id"`
	Date      string `json:"date"`
	LastSeq   int64  `json:"last_seq"`
	HeadHash  string `json:"head_hash"`
	KID       string `json:"kid"`
	Signature string `json:"signature"`
}

func eachLine(f *zip.File, fn func(line []byte) error) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	r := bufio.NewReaderSize(rc, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > maxLine {
			return errors.New("a line is too long")
		}
		if t := bytes.TrimSpace(line); len(t) > 0 {
			if ferr := fn(t); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func verifyChain(res *Result, files map[string]*zip.File, names []string, auditKeys []signing.Key) {
	var cps []checkpoint
	if cf := files[checkpoints]; cf != nil {
		err := eachLine(cf, func(line []byte) error {
			var cp checkpoint
			if err := json.Unmarshal(line, &cp); err != nil {
				return fmt.Errorf("not a checkpoint: %w", err)
			}
			cps = append(cps, cp)
			return nil
		})
		if err != nil {
			res.fail(checkpoints, 0, "%v", err)
		}
	}
	sort.SliceStable(cps, func(i, j int) bool { return cps[i].LastSeq < cps[j].LastSeq })
	res.Checkpoints = len(cps)
	for _, cp := range cps {
		body, err := signing.CanonicalJSON(map[string]any{"org_id": cp.OrgID, "date": cp.Date, "last_seq": cp.LastSeq, "head_hash": cp.HeadHash})
		sig, derr := base64.StdEncoding.DecodeString(cp.Signature)
		if err != nil || derr != nil {
			res.fail(checkpoints, cp.LastSeq, "the checkpoint signature cannot be read")
			continue
		}
		if err := signing.VerifyMessage(auditKeys, cp.KID, body, sig); err != nil {
			res.fail(checkpoints, cp.LastSeq, "the checkpoint of %s does not verify: %v", cp.Date, err)
		}
	}

	var chain []string
	for _, n := range names {
		if strings.HasPrefix(n, chainPrefix) && strings.HasSuffix(n, ".jsonl") {
			chain = append(chain, n)
		}
	}
	if len(chain) == 0 {
		if len(cps) > 0 {
			res.fail("", 0, "the bundle has checkpoints but no audit chain")
		}
		return
	}
	hashAt := map[int64]string{}
	var prevSeq int64
	var prevHash string
	first := true
	var firstSeq, lastSeq int64
	var firstPrev string
	for _, name := range chain {
		err := eachLine(files[name], func(line []byte) error {
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.UseNumber()
			var ev map[string]any
			if err := dec.Decode(&ev); err != nil {
				return fmt.Errorf("not an audit event: %w", err)
			}
			seqNum, _ := ev["seq"].(json.Number)
			seq, err := strconv.ParseInt(string(seqNum), 10, 64)
			if err != nil || seq <= 0 {
				return errors.New("an event has no sequence number")
			}
			hashHex, _ := ev["hash"].(string)
			prevHex, _ := ev["prev_hash"].(string)
			prevBytes, perr := hex.DecodeString(prevHex)
			if perr != nil {
				res.fail(name, seq, "prev_hash is not hex")
				return nil
			}
			delete(ev, "hash")
			delete(ev, "prev_hash")
			canon, err := signing.CanonicalJSON(ev)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(append(prevBytes, canon...))
			computed := hex.EncodeToString(sum[:])
			if computed != strings.ToLower(hashHex) {
				res.fail(name, seq, "the event's hash does not match its content")
			}
			switch {
			case first:
				first, firstSeq, firstPrev = false, seq, prevHex
			case seq != prevSeq+1:
				res.fail(name, seq, "the sequence jumps from %d: events are missing", prevSeq)
			case !strings.EqualFold(prevHex, prevHash):
				res.fail(name, seq, "prev_hash is not the hash of event %d", prevSeq)
			}
			prevSeq, prevHash, lastSeq = seq, computed, seq
			hashAt[seq] = computed
			res.Events++
			return nil
		})
		if err != nil {
			res.fail(name, 0, "%v", err)
		}
	}
	if res.Events == 0 {
		return
	}
	anchored := false
	for _, cp := range cps {
		switch {
		case cp.LastSeq == firstSeq-1:
			anchored = true
			if !strings.EqualFold(cp.HeadHash, firstPrev) {
				res.fail(chain[0], firstSeq, "the first event does not continue from the start checkpoint")
			}
		case cp.LastSeq >= firstSeq && cp.LastSeq <= lastSeq:
			if got := hashAt[cp.LastSeq]; !strings.EqualFold(got, cp.HeadHash) {
				res.fail(checkpoints, cp.LastSeq, "the chain recomputed to a different head than the checkpoint of %s", cp.Date)
			}
		}
	}
	if !anchored && firstSeq > 1 {
		res.fail(chain[0], firstSeq, "no checkpoint anchors the start of the chain")
	}
	if len(cps) > 0 && cps[len(cps)-1].LastSeq < lastSeq {
		res.Warnings = append(res.Warnings, fmt.Sprintf("events %d to %d come after the last signed checkpoint", cps[len(cps)-1].LastSeq+1, lastSeq))
	}
	if vf := files[verification]; vf != nil {
		raw, err := readSmall(vf)
		var v struct {
			OK             bool   `json:"ok"`
			ToSeq          int64  `json:"to_seq"`
			RecomputedHead string `json:"recomputed_head"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &v)
		}
		switch {
		case err != nil:
			res.fail(verification, 0, "cannot be read: %v", err)
		case !v.OK:
			res.fail(verification, 0, "the gateway's own verification of the chain failed")
		case v.RecomputedHead != "" && v.ToSeq > 0 && !strings.EqualFold(hashAt[v.ToSeq], v.RecomputedHead):
			res.fail(verification, v.ToSeq, "the recorded head differs from the recomputed one")
		}
	}
}

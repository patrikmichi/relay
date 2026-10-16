package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// addFileFlags registers --file and --save-files on a command that calls a tool.
func addFileFlags(cmd *cobra.Command, files *[]string, saveDir *string) {
	cmd.Flags().StringArrayVar(files, "file", nil, "Upload a local file and pass it as the tool argument param, as --file param=@path (repeatable)")
	cmd.Flags().StringVar(saveDir, "save-files", "", "Directory to save the files a tool result refers to")
}

// uploadCallFiles uploads each --file param=@path and returns the arguments
// that pass them by reference. The file must be ready before the call, since
// a tool only accepts a file the gateway has checked.
func uploadCallFiles(ctx context.Context, c *client.Client, specs, rawArgs, jsonArgs []string) ([]string, error) {
	used := map[string]bool{}
	for _, a := range append(append([]string(nil), rawArgs...), jsonArgs...) {
		if k, _, ok := strings.Cut(a, "="); ok {
			used[k] = true
		}
	}
	type pair struct{ param, path string }
	var pairs []pair
	for _, spec := range specs {
		param, path, ok := strings.Cut(spec, "=@")
		if !ok || param == "" || path == "" {
			return nil, fmt.Errorf("invalid --file %q: expected param=@path", textsafe.Clean(spec))
		}
		if used[param] {
			return nil, fmt.Errorf("argument %q is given twice", textsafe.Clean(param))
		}
		used[param] = true
		pairs = append(pairs, pair{param, path})
	}
	refs := make([]string, 0, len(pairs))
	var progress io.Writer
	if isInteractiveTerminal(os.Stderr) {
		progress = os.Stderr
	}
	for _, p := range pairs {
		doc, err := uploadFile(ctx, c, p.path, uploadOptions{wait: true, progress: progress})
		if err != nil {
			return nil, fmt.Errorf("upload %s for %s: %w", textsafe.Clean(p.path), textsafe.Clean(p.param), err)
		}
		fmt.Fprintf(os.Stderr, "Uploaded %s as %s\n", textsafe.Clean(p.path), doc.ID)
		refs = append(refs, p.param+"=file:"+doc.ID)
	}
	return refs, nil
}

// resultFile is one entry of a tool result's structuredContent.files.
type resultFile struct {
	FileID string `json:"file_id"`
	Name   string `json:"name"`
}

// saveResultFiles downloads the files a tool result refers to into dir.
func saveResultFiles(ctx context.Context, c *client.Client, r *mcpToolResult, dir string, out io.Writer, jsonOut bool) error {
	var sc struct {
		Files []resultFile `json:"files"`
	}
	if len(r.StructuredContent) == 0 || json.Unmarshal(r.StructuredContent, &sc) != nil || len(sc.Files) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("--save-files: %w", err)
	}
	// With --json standard output carries the result, so progress goes to
	// standard error.
	log := out
	if jsonOut {
		log = os.Stderr
	}
	var failed []string
	for _, f := range sc.Files {
		id, err := parseFileRef(f.FileID)
		if err == nil {
			var path string
			_, path, err = downloadFile(ctx, c, id, dir, false)
			if err == nil {
				fmt.Fprintf(log, "Saved %s to %s\n", id, textsafe.Clean(path))
				continue
			}
		}
		fmt.Fprintf(os.Stderr, "could not save %s: %v\n", textsafe.Clean(f.FileID), err)
		failed = append(failed, textsafe.Clean(f.FileID))
	}
	if len(failed) > 0 {
		return fmt.Errorf("the call succeeded, but %d of its files could not be saved: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

// resourceLinkText renders an MCP resource_link block on one line.
func resourceLinkText(raw json.RawMessage) (string, bool) {
	var b struct {
		Type     string `json:"type"`
		URI      string `json:"uri"`
		Name     string `json:"name"`
		MimeType string `json:"mimeType"`
		Size     *int64 `json:"size"`
	}
	if json.Unmarshal(raw, &b) != nil || b.Type != "resource_link" || b.URI == "" {
		return "", false
	}
	line := "File: " + textsafe.Clean(firstNonEmptyString(b.Name, b.URI)) + " " + textsafe.Clean(b.URI)
	var detail []string
	if b.MimeType != "" {
		detail = append(detail, textsafe.Clean(b.MimeType))
	}
	if b.Size != nil {
		detail = append(detail, fmt.Sprintf("%d bytes", *b.Size))
	}
	if len(detail) > 0 {
		line += " (" + strings.Join(detail, ", ") + ")"
	}
	return line, true
}

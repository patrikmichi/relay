package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// FilesCmd returns `zuano files`, the client for the organisation's file
// store: files move into and out of tools by reference instead of inside the
// request body.
func FilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "files",
		Short: "Upload, download, list and delete files in the organisation's file store",
		Long: `Files go to the gateway's file store and tools take them by id (file:fil_...),
so a tool call is not limited by the size of a request body. 'zuano call
--file param=@path' uploads and passes a file in one step. Files expire; the
list shows when.`,
	}
	cmd.AddCommand(filesUploadCmd(), filesDownloadCmd(), filesListCmd(), filesRmCmd())
	return cmd
}

func filesUploadCmd() *cobra.Command {
	var f approvalFlags
	var name string
	var shareOrg, noWait bool
	cmd := &cobra.Command{
		Use:   "upload <path>...",
		Short: "Upload files and print their ids",
		Long: `Uploads each file straight to the object store with a signed address from the
gateway, tells the gateway it is done and, unless --no-wait, waits until the
gateway has checked the file and it is ready to use. A file the gateway
quarantines or rejects is an error.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name != "" && len(args) > 1 {
				return errors.New("--name names one file; upload several without it")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var docs []fileDoc
			var firstErr error
			for _, path := range args {
				doc, err := uploadFile(cmd.Context(), c, path, uploadOptions{name: name, shareOrg: shareOrg, wait: !noWait, progress: progressWriter(cmd)})
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", textsafe.Clean(path), err)
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", textsafe.Clean(path), err)
					continue
				}
				docs = append(docs, doc)
				if !f.jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s\n", doc.ID, orDash(textsafe.Clean(doc.State)), textsafe.Clean(path))
				}
			}
			if f.jsonOut {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"files": docs}); err != nil {
					return err
				}
			}
			if firstErr != nil {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	f.bind(cmd, "Print the uploaded files as one JSON document")
	cmd.Flags().StringVar(&name, "name", "", "Name to store the file under (default: its file name)")
	cmd.Flags().BoolVar(&shareOrg, "share-org", false, "Let every member of the organisation read the file")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return when the upload is complete, without waiting for the gateway's checks")
	return cmd
}

func filesDownloadCmd() *cobra.Command {
	var f approvalFlags
	var output string
	var force bool
	cmd := &cobra.Command{
		Use:   "download <fil_...|file:fil_...|resource URI>",
		Short: "Download a stored file",
		Long: `Downloads a file by id, or by the address of a resource link from a tool
result. It is saved under -o, or under its own name in the current directory,
and refused if that already exists unless --force. The size and sha256 are
checked against the gateway's record, and nothing is saved if they differ.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseFileRef(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			meta, path, err := downloadFile(cmd.Context(), c, id, output, force)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"file": meta, "path": path})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s (%d bytes) to %s\n", textsafe.Clean(id), meta.SizeBytes, textsafe.Clean(path))
			return nil
		},
	}
	f.bind(cmd, "Print the file record and the path as JSON")
	cmd.Flags().StringVarP(&output, "output", "o", "", "File or directory to save to")
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing file")
	return cmd
}

func filesListCmd() *cobra.Command {
	var f approvalFlags
	var state, via string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored files, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			files, err := fileList(cmd.Context(), c, state, via, 1000)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return json.NewEncoder(out).Encode(map[string]any{"files": files})
			}
			if len(files) == 0 {
				fmt.Fprintln(out, "No files.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSIZE\tSTATE\tSCAN\tEXPIRES")
			for _, d := range files {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", textsafe.Clean(d.ID), textsafe.Clean(d.Name), d.SizeBytes, textsafe.Clean(d.State), orDash(textsafe.Clean(d.Scan.Verdict)), orDash(textsafe.Clean(d.ExpiresAt)))
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the files as JSON")
	cmd.Flags().StringVar(&state, "state", "", "Only files in this state (ready, quarantined, failed, ...)")
	cmd.Flags().StringVar(&via, "created-via", "", "Only files created this way (cli, tool_result, run_code, webhook, ...)")
	return cmd
}

func filesRmCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:     "rm <fil_...>...",
		Aliases: []string{"delete"},
		Short:   "Delete stored files",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]string, 0, len(args))
			for _, a := range args {
				id, err := parseFileRef(a)
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if _, err := gatewayJSON(cmd.Context(), c, http.MethodDelete, "/api/files/"+url.PathEscape(id), nil, nil); err != nil {
					return fmt.Errorf("delete %s: %w", id, err)
				}
				if !f.jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", id)
				}
			}
			if f.jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"deleted": ids})
			}
			return nil
		},
	}
	f.bind(cmd, "Print the deleted ids as JSON")
	return cmd
}

// progressWriter is where upload progress goes: standard error, and only when
// that is a terminal, so scripts and logs stay clean.
func progressWriter(cmd *cobra.Command) io.Writer {
	if f, ok := cmd.ErrOrStderr().(*os.File); ok && isInteractiveTerminal(f) {
		return f
	}
	return nil
}

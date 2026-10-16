package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/evidence"
	"github.com/patrikmichi/zuano/internal/signing"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// Exit statuses of `zuano compliance verify`.
const (
	exitBundleInvalid = 40
	exitKeysMissing   = 41
)

func complianceVerifyCmd() *cobra.Command {
	var gatewayURL, keysFile string
	cmd := &cobra.Command{
		Use:   "verify <bundle.zip> [--keys keys.json]",
		Short: "Check an export bundle offline: signature, file hashes and the audit hash chain",
		Long: `Needs no login. Checks the manifest signature, that every file matches the
hash the signed manifest lists and that nothing else is in the archive, then
recomputes the audit hash chain event by event and checks the signed daily
checkpoints. A bundle that does not check out exits 40 and each file or event
that failed is named.

The public keys come from --keys (a file you obtained from the gateway's
operator by another route, the stricter way) or from the gateway's published key
documents. --keys holds {"keys": [...]} or {"evidence": [...], "audit": [...]}.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, err := verifyKeys(cmd, gatewayURL, keysFile)
			if err != nil {
				return &ExitError{Code: exitKeysMissing, Msg: fmt.Sprintf("the public keys could not be obtained: %v", err)}
			}
			res, err := evidence.Verify(args[0], keys)
			if err != nil {
				return &ExitError{Code: exitBundleInvalid, Msg: err.Error()}
			}
			out := cmd.OutOrStdout()
			for _, w := range res.Warnings {
				fmt.Fprintf(out, "warning: %s\n", textsafe.Clean(w))
			}
			if !res.OK() {
				for _, f := range res.Failures {
					fmt.Fprintf(out, "  %s\n", textsafe.Clean(f.String()))
				}
				return &ExitError{Code: exitBundleInvalid, Msg: fmt.Sprintf("the bundle is not valid (%d problem(s))", len(res.Failures))}
			}
			m := res.Manifest
			fmt.Fprintf(out, "Valid: %s pack for %s, %s to %s: %d files, %d audit events, %d checkpoints.\n", textsafe.Clean(m.Pack), textsafe.Clean(m.OrgSlug), textsafe.Clean(m.Range.From), textsafe.Clean(m.Range.To), res.Files, res.Events, res.Checkpoints)
			return nil
		},
	}
	cmd.Flags().StringVar(&keysFile, "keys", "", "A file of public keys to check against, instead of the gateway's published ones")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL for its published keys (default: $ZUANO_URL, then the config file)")
	return cmd
}

func verifyKeys(cmd *cobra.Command, gatewayURL, keysFile string) (evidence.Keys, error) {
	if keysFile != "" {
		raw, err := os.ReadFile(keysFile)
		if err != nil {
			return evidence.Keys{}, err
		}
		var doc struct {
			Keys     []signing.Key `json:"keys"`
			Evidence []signing.Key `json:"evidence"`
			Audit    []signing.Key `json:"audit"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return evidence.Keys{}, fmt.Errorf("%s is not a key document: %w", textsafe.Clean(keysFile), err)
		}
		k := evidence.Keys{Evidence: append(doc.Evidence, doc.Keys...), Audit: append(doc.Audit, doc.Keys...)}
		if len(k.Evidence) == 0 && len(k.Audit) == 0 {
			return k, fmt.Errorf("%s holds no keys", textsafe.Clean(keysFile))
		}
		return k, nil
	}
	gw, err := resolveGatewayURLOrFailClosed(gatewayURL)
	if err != nil {
		return evidence.Keys{}, fmt.Errorf("%v; pass --keys to check offline", err)
	}
	ev, err := signing.FetchKeysAt(cmd.Context(), gw, "/.well-known/"+brand.Name+"-evidence-keys")
	if err != nil {
		return evidence.Keys{}, err
	}
	au, err := signing.FetchKeysAt(cmd.Context(), gw, "/.well-known/"+brand.Name+"-audit-keys")
	if err != nil {
		return evidence.Keys{}, err
	}
	return evidence.Keys{Evidence: ev, Audit: au}, nil
}

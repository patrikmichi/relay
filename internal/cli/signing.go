package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/catalog"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/signing"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// signingPolicy builds the verification policy for a gateway from the keys
// pinned for its origin.
func signingPolicy(gatewayURL string, require bool) (signing.Policy, error) {
	origin, err := config.NormalizeGatewayURL(gatewayURL)
	if err != nil {
		return signing.Policy{}, err
	}
	keys, err := config.PinnedSigningKeys(origin)
	if err != nil {
		return signing.Policy{}, fmt.Errorf("load config: %w", err)
	}
	return signing.Policy{Keys: keys, Require: require}, nil
}

func signingOption(gatewayURL string, require bool) (catalog.Option, error) {
	policy, err := signingPolicy(gatewayURL, require)
	if err != nil {
		return nil, err
	}
	return catalog.WithSigning(policy, os.Stderr), nil
}

// pinSigningKeys records the gateway's signing keys the first time they are
// seen (trust on first use). A gateway that signs nothing yet leaves nothing
// pinned. Keys that differ from the pinned ones are never adopted here: that
// is a decision for `relay config trust-key`.
func pinSigningKeys(ctx context.Context, origin string, out io.Writer) {
	keys, err := signing.FetchKeys(ctx, origin)
	if errors.Is(err, signing.ErrNoKeys) {
		return
	}
	if err != nil {
		fmt.Fprintf(out, "warning: could not read the gateway's signing keys (%v)\n", err)
		return
	}
	pinned, err := config.PinnedSigningKeys(origin)
	if err != nil {
		fmt.Fprintf(out, "warning: could not read the pinned signing keys (%v)\n", err)
		return
	}
	switch {
	case len(pinned) == 0:
		if err := config.PinSigningKeys(origin, keys); err != nil {
			fmt.Fprintf(out, "warning: could not pin the gateway's signing keys (%v)\n", err)
			return
		}
		fmt.Fprintf(out, "Pinned %d signing key(s) for %s on first use:\n", len(keys), origin)
		printKeys(out, keys)
	case !signing.Same(pinned, keys):
		fmt.Fprintf(out, "warning: the signing keys %s publishes differ from the ones pinned. Downloads signed with the new keys are refused until you review them with `relay config trust-key`.\n", origin)
	}
}

func pinLoginKeys(ctx context.Context, gatewayURL string, out io.Writer) {
	if origin, err := config.NormalizeGatewayURL(gatewayURL); err == nil {
		pinSigningKeys(ctx, origin, out)
	}
}

func printKeys(out io.Writer, keys []signing.Key) {
	for _, k := range keys {
		fmt.Fprintf(out, "  %s  %s\n", textsafe.Clean(k.KID), signing.Fingerprint(k))
	}
}

func configTrustKeyCmd() *cobra.Command {
	var gatewayURL string
	var yes bool
	cmd := &cobra.Command{
		Use:   "trust-key",
		Short: "Review and pin the gateway's download-signing keys",
		Long: `Fetch the signing keys the gateway publishes at /.well-known/relay-signing-keys,
show their fingerprints and pin them, replacing the keys pinned before.

relay pins the keys the first time you log in. A later change is refused until
you run this, because a key that changed could mean the gateway was replaced.
Compare the fingerprints with the ones your gateway's operator publishes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseInAgentMode("relay config trust-key"); err != nil {
				return err
			}
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}
			origin, err := config.NormalizeGatewayURL(gURL)
			if err != nil {
				return err
			}
			keys, err := signing.FetchKeys(cmd.Context(), origin)
			if errors.Is(err, signing.ErrNoKeys) {
				return fmt.Errorf("%s publishes no signing keys, so there is nothing to pin", origin)
			}
			if err != nil {
				return err
			}
			pinned, err := config.PinnedSigningKeys(origin)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if signing.Same(pinned, keys) {
				fmt.Fprintf(out, "The keys pinned for %s already match:\n", origin)
				printKeys(out, keys)
				return nil
			}
			if len(pinned) > 0 {
				fmt.Fprintf(out, "Pinned now:\n")
				printKeys(out, pinned)
			}
			fmt.Fprintf(out, "%s publishes:\n", origin)
			printKeys(out, keys)
			if !yes {
				in := cmd.InOrStdin()
				if !canPrompt(in) {
					return errors.New("pinning signing keys needs an interactive terminal, or --yes after you compared the fingerprints")
				}
				if !askNo(bufio.NewReader(in), out, "Trust these keys?") {
					return errors.New("not confirmed; the pinned keys are unchanged")
				}
			}
			if err := config.PinSigningKeys(origin, keys); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Fprintf(out, "Pinned %d key(s) for %s.\n", len(keys), origin)
			return nil
		},
	}
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Pin without asking (refused when ZUANO_AGENT is set)")
	return cmd
}

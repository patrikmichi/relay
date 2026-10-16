package catalog

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/patrikmichi/relay/internal/signing"
)

// Option adjusts how a download is verified.
type Option func(*fetchOptions)

type fetchOptions struct {
	policy signing.Policy
	warn   io.Writer
	check  bool
}

// WithSigning verifies the gateway's signature on the download against the
// pinned keys in policy. An unsigned download is allowed with a warning on
// warn, unless policy.Require is set.
func WithSigning(policy signing.Policy, warn io.Writer) Option {
	return func(o *fetchOptions) { o.policy, o.warn, o.check = policy, warn, true }
}

func newFetchOptions(opts []Option) fetchOptions {
	var o fetchOptions
	for _, f := range opts {
		f(&o)
	}
	return o
}

// VerifyDownload checks the signature headers of any gateway download (a
// plugin bundle, for one) the same way FetchSkill checks a skill. label names
// it in warnings; requestedVersion, when set, must be the signed version.
func VerifyDownload(policy signing.Policy, warn io.Writer, h http.Header, body []byte, label, requestedVersion string) error {
	return fetchOptions{policy: policy, warn: warn, check: true}.verify(h, body, label, requestedVersion)
}

// verify checks the signature headers of a download whose body has already
// matched its content hash. A signed download must also carry the id and
// version it is signed for, and an exact requested version must be the one
// signed, so an older signed release cannot be served in place of it.
func (o fetchOptions) verify(h http.Header, body []byte, id, requestedVersion string) error {
	if !o.check {
		return nil
	}
	art := signing.Artifact{
		SHA256:  signing.ContentSHA256(body),
		ID:      h.Get(headerCatalogID),
		Version: h.Get(headerVersion),
		Verdict: strings.TrimSpace(h.Get(headerScanVerdict)),
	}
	signed, err := o.policy.Check(h, art)
	if err != nil {
		return err
	}
	if !signed {
		if o.warn != nil {
			fmt.Fprintf(o.warn, "warning: the gateway did not sign %s; its integrity rests on the gateway's own checksum (use --require-signature to refuse unsigned downloads)\n", id)
		}
		return nil
	}
	if art.ID == "" || art.Version == "" {
		return fmt.Errorf("signed download %s carries no %s or %s header", id, headerCatalogID, headerVersion)
	}
	if requestedVersion != "" && art.Version != requestedVersion {
		return fmt.Errorf("signed download %s is version %s, not the requested %s", id, art.Version, requestedVersion)
	}
	return nil
}

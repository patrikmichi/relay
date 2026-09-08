package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// ErrServiceAuthorizationUnsupported is returned by `relay authorize
// <service>`. The real per-service grant flow does not exist yet.
// `relay login --device` only creates a generic CLI session — it grants
// no service-specific scopes — so this command must never report success
// for a service it did not actually authorize.
var ErrServiceAuthorizationUnsupported = errors.New("service authorization is not implemented")

// AuthorizeCmd returns the `relay authorize <service>` cobra command. It is
// an explicit stub: it always fails with ErrServiceAuthorizationUnsupported
// rather than reusing the generic device-code login flow and presenting
// that as proof of service authorization.
func AuthorizeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "authorize <service>",
		Short: "Authorize a specific service (not yet implemented)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			return fmt.Errorf("%w: %q — `relay login --device` creates a CLI session but grants no %s-specific scopes; run that command for CLI access", ErrServiceAuthorizationUnsupported, service, service)
		},
	}
	return cmd
}

// Package brand holds the product's names, so user-facing text, file
// locations and the identifiers written into other tools' configs all come
// from one place.
package brand

const (
	// Name is the command a person types and the prefix of every on-disk and
	// config identifier: the config directory, keychain service, sync
	// markers and the MCP server written into agents.
	Name = "zuano"
	// Product is the name shown in prose.
	Product = "Zuano"
	// Company is the organisation that publishes the product.
	Company = "Loopmatic"
)

// Cmd returns a command line as the person types it, such as "zuano login".
func Cmd(args string) string { return Name + " " + args }

// Package brand holds the product's command name, so user-facing text names
// the command from one place.
package brand

// Name is the command a person types.
const Name = "relay"

// Cmd returns a command line as the person types it, such as "relay login".
func Cmd(args string) string { return Name + " " + args }

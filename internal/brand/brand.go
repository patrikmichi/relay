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

const (
	// EvalsProduct is the name of the evaluation product in prose.
	EvalsProduct = Product + " Evals"
	// EvalsConfigFile is the evaluation config a repository carries.
	EvalsConfigFile = Name + ".evals.yaml"
	// EvalsFormat names the case and result documents the evaluation commands
	// read and write.
	EvalsFormat = Name + ".evals"
	// SignatureHeader carries the HMAC of a request to an application's
	// evaluation hook.
	SignatureHeader = Product + "-Signature"
)

// InstallHeader is the non-secret header zuano writes into an agent's gateway
// entry so the gateway can show which machine a session came from.
const InstallHeader = "X-" + Product + "-Install"

// Cmd returns a command line as the person types it, such as "zuano login".
func Cmd(args string) string { return Name + " " + args }

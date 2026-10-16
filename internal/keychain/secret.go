package keychain

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

func secretAccount(server, name string) string { return "mcp-env:" + server + ":" + name }

// WriteSecret stores an environment value for an MCP server, so the agent's
// config does not carry it.
func WriteSecret(server, name, value string) error {
	if err := keyring.Set(serviceName, secretAccount(server, name), value); err != nil {
		return fmt.Errorf("could not store %s for %s in the system keychain: %w", name, server, err)
	}
	return nil
}

// ReadSecret returns a value stored with WriteSecret.
func ReadSecret(server, name string) (string, error) {
	v, err := keyringGet(secretAccount(server, name))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("%s for %s is not in the system keychain: %w", name, server, err)
	}
	return v, err
}

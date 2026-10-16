package client

import (
	"encoding/json"
	"fmt"
	"io"
)

// MaxJSONBytes caps a gateway JSON response zuano will buffer, so a hostile or
// broken gateway cannot exhaust memory.
const MaxJSONBytes = 4 << 20

// DecodeJSON decodes a response body into v, refusing one larger than
// MaxJSONBytes.
func DecodeJSON(body io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(body, MaxJSONBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaxJSONBytes {
		return fmt.Errorf("response exceeds the %d MiB limit", MaxJSONBytes>>20)
	}
	return json.Unmarshal(data, v)
}

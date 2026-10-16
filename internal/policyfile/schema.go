package policyfile

import _ "embed"

//go:embed schema/policy-v1.json
var schemaJSON []byte

// SchemaJSON is the policy JSON Schema as embedded. The gateway publishes the
// same file; `make gen-policy-schema` copies it in from a gateway release.
func SchemaJSON() []byte { return schemaJSON }

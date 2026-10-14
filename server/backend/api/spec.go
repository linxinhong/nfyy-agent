// Package spec owns the wire contract shared by Go and TypeScript.
package spec

import _ "embed"

//go:generate go tool oapi-codegen -config oapi-codegen.yaml -o ../internal/api/types.gen.go openapi.yaml

//go:embed openapi.yaml
var OpenAPI []byte

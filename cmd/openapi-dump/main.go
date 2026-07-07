// Command openapi-dump writes the /v1 OpenAPI document to a file by building
// the huma-registered operations in-process (no apispec, no live deps). The
// spec is emitted directly from the operations themselves, so it cannot drift
// from the handlers.
//
// Usage: openapi-dump [output.yaml]   (default: openapi.yaml)
package main

import (
	"fmt"
	"os"

	"github.com/ggscale/ggscale/internal/httpapi"
)

// specVersion is the info.version stamped into the generated document.
const specVersion = "1.0.0"

func main() {
	out := "openapi.yaml"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	doc := httpapi.OpenAPIDoc(specVersion)
	y, err := doc.YAML()
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-dump: marshal:", err)
		os.Exit(1)
	}
	//nolint:gosec // G703/G304: dev codegen tool; the output path is supplied by the operator.
	if err := os.WriteFile(out, y, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "openapi-dump: write:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", out)
}

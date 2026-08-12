// Command openapigen writes the §6.1 control-plane OpenAPI document (ADR-0008
// part 1) from the orchestrator's own wire types.
//
//	go run ./cmd/openapigen -out docs/spec/openapi.json
//
// A tiny main rather than a Makefile target or a shell script, because the document
// must be regenerable by someone who has only the Go toolchain — the same reason the
// generated wire stubs are committed rather than assumed regenerable.
//
// Forgetting to run it is a TEST FAILURE, not a silent drift:
// internal/orchestrator/openapi_test.go compares the checked-in file against what
// the generator produces.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/TSHOGX/warren/internal/orchestrator"
)

func main() {
	out := flag.String("out", "docs/spec/openapi.json",
		"path to write the document to (relative to the repo root)")
	flag.Parse()

	doc, err := orchestrator.OpenAPIDoc()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapigen: %v\n", err)
		os.Exit(1)
	}
	// 0644 rather than 0600: this is a published artifact, checked in and read by
	// consumers' generators.
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "openapigen: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("openapigen: wrote %s (%d bytes)\n", *out, len(doc))
}

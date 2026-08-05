//go:build !(js && wasm)

// This file exists so the package still builds on the platforms the playground
// is not for. `go build ./...`, `go vet ./...` and the linter all walk every
// package, and a package whose only file is js/wasm-only has no Go files at all
// for them — which they report as an error rather than as "not for you".
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"playground is a WebAssembly build: GOOS=js GOARCH=wasm go build ./cmd/playground")
	os.Exit(1)
}

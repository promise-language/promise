// Command buildhash prints the current flow source hash (srchash.Hash) to stdout.
// ./make runs it via `go run` to obtain the value it bakes into each flow binary
// via ldflags (-X main.sourceHash), so the build-time hash and the binary's
// runtime self-check (srchash.CheckStale) are computed by identical code. It is a
// build-time utility only — never installed to bin/.
package main

import (
	"fmt"
	"os"

	"github.com/p5e-ia/promise-lang/flows/internal/srchash"
)

func main() {
	root, err := srchash.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "buildhash: %v\n", err)
		os.Exit(1)
	}
	hash, err := srchash.Hash(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "buildhash: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(hash)
}

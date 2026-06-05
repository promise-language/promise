// Command release is the forge driver that produces the release artifacts the
// runtime dependency model (T0769/T0770) consumes: dependency blobs, the embedded
// manifest with ranked acquisition sources, the thin/full compiler variants with
// the Promise stub embedded, and the manifest-integrity gate.
//
// Usage:
//
//	bin/release blobs --host <target> --out <dir>
//	bin/release manifest <blobsdir> --host <target> --pack <dir> --out <manifest> [--tag <tag>]
//	bin/release build --variant {thin|full} --manifest <m> --out <bin> [--blobs <dir>]
//	bin/release verify-manifest <manifest>... --against <dir>
//
// See docs/release-automation.md §2 (build-order) and T0773.
package main

import (
	"fmt"
	"os"

	"github.com/promise-language/promise/tools/build/common"
)

var sourceHash = "dev"

func main() {
	common.CheckStale(sourceHash)
	root, err := common.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := common.RunRelease(root, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// Command release is the forge driver that produces the release artifacts the
// runtime dependency model (T0769/T0770) consumes: dependency blobs, the embedded
// manifest with ranked acquisition sources, the thin/full compiler variants with
// the Promise stub embedded, and the manifest-integrity gate.
//
// Usage:
//
//	bin/release blobs --host <target> --out <dir>
//	bin/release manifest <blobsdir> --host <target> --pack <dir> --out <manifest> [--tag <tag>]
//	bin/release manifest --from-catalog --host <target> --out <manifest>
//	bin/release publish-blobs --dependency <dep> --host <target> [--dry-run] [--no-upload]
//	bin/release fetch-blobs --manifest <m> --out <dir> [--keep-compressed]
//	bin/release build --variant {thin|full} --manifest <m> --out <bin> [--blobs <dir>]
//	bin/release verify-manifest <manifest>... --against <dir>
//	bin/release cut next   [--dry-run] [--reason <text>] [--run-ci] [--no-ci-wait] [--notes-file <path>|-] [--notes <text>]
//	bin/release cut stable [--dry-run] [--reason <text>] [--run-ci] [--no-ci-wait] [--confirm-year] [--notes-file <path>|-] [--notes <text>]
//	bin/release ci [platform...] [--no-tests] [--watch] [--ref <branch>] [--force]
//
// See docs/release-automation.md §2 (build-order), §6.3 (gated cut), and T0773,
// T0797, T0943.
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

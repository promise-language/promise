package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/promise-language/promise/tools/build/common"
)

var sourceHash = "dev"

// exitLockTimeout (EX_TEMPFAIL) signals that --lock-timeout elapsed before the
// host verify lock could be acquired. It is distinct from exit code 1 (verify
// ran and failed) so callers can retry for a turn instead of treating the run
// as a verification failure.
const exitLockTimeout = 75

func main() {
	common.CheckStale(sourceHash)
	root, err := common.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := common.RunVerify(root, os.Args[1:]); err != nil {
		// Lock-acquisition timeout is NOT a verification failure: emit a
		// machine-detectable marker and a dedicated exit code so the caller
		// (the tracker runner) retries for a turn rather than failing the item.
		if errors.Is(err, common.ErrLockTimeout) {
			fmt.Fprintf(os.Stderr, "VERIFY_LOCK_TIMEOUT: %v\n", err)
			os.Exit(exitLockTimeout)
		}
		fmt.Fprintf(os.Stderr, "----------------------------------------------------\n")
		fmt.Fprintf(os.Stderr, "❌ Verify FAILED: not safe to commit\n")
		fmt.Fprintf(os.Stderr, "----------------------------------------------------\n")
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

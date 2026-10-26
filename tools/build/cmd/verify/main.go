package main

import (
	"os"

	"github.com/promise-language/promise/tools/build/common"
)

var sourceHash = "dev"

func main() {
	// Before the command line is read, -help and -version included: a stale
	// binary answers nothing from a surface it no longer matches, and the flow
	// reads this refusal as "run ./make and ask once more".
	common.RefuseUnlessFit("verify", sourceHash, os.Args[1:])
	// Stop rather than run on as an orphan if our launcher is killed (T1450).
	common.WatchForOrphan()
	root, err := common.FindRoot()
	if err != nil {
		os.Exit(common.Fail("verify", err))
	}
	os.Exit(common.Fail("verify", common.RunVerify(root, os.Args[1:], os.Stdout)))
}

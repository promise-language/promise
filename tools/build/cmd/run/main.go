package main

import (
	"os"

	"github.com/promise-language/promise/tools/build/common"
)

var sourceHash = "dev"

func main() {
	// Before the command line is read, -help and -version included: a stale
	// binary answers nothing from a surface it no longer matches.
	common.RefuseUnlessFit("run", sourceHash, os.Args[1:], "verdict")
	// Stop rather than run on as an orphan if our launcher is killed (T1450).
	common.WatchForOrphan()
	root, err := common.FindRoot()
	if err != nil {
		os.Exit(common.Fail("run", err))
	}
	os.Exit(common.Fail("run", common.RunRun(root, os.Args[1:], os.Stdin, os.Stdout)))
}

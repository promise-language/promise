package main

import (
	"fmt"
	"os"

	"github.com/promise-language/promise/tools/build/common"
)

var sourceHash = "dev"

func main() {
	common.CheckStale(sourceHash)
	// Stop rather than run on as an orphan if our launcher is killed (T1450).
	common.WatchForOrphan()
	root, err := common.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := common.RunTest(root, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

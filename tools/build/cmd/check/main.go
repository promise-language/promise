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
	fmt.Println("Checking go...")
	if err := common.RunCheck(root); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

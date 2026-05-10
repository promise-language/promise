package main

import (
	"fmt"
	"os"

	"github.com/p5e-ia/promise-lang/tools/build/common"
)

var sourceHash = "dev"

func main() {
	common.CheckStale(sourceHash)
	fmt.Println("coverage: not yet implemented")
	os.Exit(1)
}

package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunCoverage runs test coverage analysis for Go and/or Promise tests.
// Modes: "go" (Go only), "promise" (Promise only), "all" (default).
// Additional args are paths to Go packages or Promise test directories.
func RunCoverage(root string, args []string) error {
	suite := "all"
	var paths []string

	for _, arg := range args {
		switch arg {
		case "go", "promise", "all":
			suite = arg
		default:
			paths = append(paths, arg)
		}
	}

	compilerDir := filepath.Join(root, "compiler")
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// Classify paths
	var goPkgs, promiseTargets []string
	if len(paths) > 0 {
		for _, p := range paths {
			if strings.HasPrefix(p, "./") || strings.HasSuffix(p, "/") {
				goPkgs = append(goPkgs, p)
			} else if strings.HasSuffix(p, ".pr") {
				promiseTargets = append(promiseTargets, p)
			} else {
				if Exists(filepath.Join(root, "compiler", p)) {
					goPkgs = append(goPkgs, p)
				}
				if Exists(filepath.Join(root, p)) {
					promiseTargets = append(promiseTargets, p)
				}
			}
		}
	} else {
		goPkgs = []string{"./..."}
		promiseTargets = []string{"tests/...", "modules/..."}
	}

	// Go coverage
	if suite == "go" || suite == "all" {
		for _, pkg := range goPkgs {
			if err := runGoCoverage(compilerDir, pkg); err != nil {
				fmt.Fprintf(os.Stderr, "warning: go coverage %s: %v\n", pkg, err)
			}
		}
	}

	// Promise coverage
	if suite == "promise" || suite == "all" {
		for _, target := range promiseTargets {
			if err := runPromiseCoverage(root, promiseBin, target); err != nil {
				fmt.Fprintf(os.Stderr, "warning: promise coverage %s: %v\n", target, err)
			}
		}
	}

	return nil
}

func runGoCoverage(compilerDir, pkg string) error {
	coverPkgs, testPkgs, err := goCoverageScope(compilerDir, pkg)
	if err != nil {
		return err
	}
	fmt.Printf("=== Go Coverage: %s (tests: %s) ===\n\n", pkg, testPkgs)

	covFile := filepath.Join(os.TempDir(), "promise_cov.out")
	defer os.Remove(covFile)

	// Run tests with coverage. -coverpkg is what makes the number agree with
	// bin/gate coverage: without it `go test` instruments only the package under
	// test, so a suite living in a sibling package is credited to nothing.
	err = RunIn(compilerDir, "go", "test", testPkgs,
		"-coverpkg="+coverPkgs, "-coverprofile="+covFile, "-count=1")
	if err != nil {
		return err
	}

	// Check if coverage file was generated
	if !Exists(covFile) {
		return nil
	}

	// Show function coverage
	fmt.Println()
	return RunIn(compilerDir, "go", "tool", "cover", "-func="+covFile)
}

// goCoverageScope resolves a requested package into the -coverpkg list (whose
// statements are measured) and the test-package pattern (whose tests are run).
//
// The two differ because T1776 moved codegen's and cmd/promise's suites into
// per-area packages under .../tests/. Asking for internal/codegen's coverage
// has to run those sibling packages to exercise it, and has to name
// internal/codegen in -coverpkg for their execution to be attributed anywhere:
// `go test ./internal/codegen/` alone reports 54.6% where the package actually
// stands at 86.7%.
//
// For the whole tree, this matches bin/gate coverage exactly — every package
// but the generated parser. For a named package, the measurement stays scoped
// to what the caller asked about while the tests come from its whole subtree.
func goCoverageScope(compilerDir, pkg string) (coverPkgs, testPkgs string, err error) {
	if pkg == "./..." {
		pkgs, err := goCoveragePackages(compilerDir)
		if err != nil {
			return "", "", err
		}
		return strings.Join(pkgs, ","), pkg, nil
	}
	named := strings.TrimSuffix(strings.TrimSuffix(pkg, "/..."), "/")
	return named, named + "/...", nil
}

func runPromiseCoverage(root, promiseBin, target string) error {
	fmt.Printf("=== Promise Coverage: %s ===\n\n", target)
	return RunIn(root, promiseBin, "test", "-coverage", "-timeout", "30", target)
}

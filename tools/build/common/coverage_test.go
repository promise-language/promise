package common

import "testing"

// Asking for one package's coverage has to run the suite that exercises it,
// which since T1776 lives in sibling packages beneath it — while the
// measurement stays scoped to the package the caller named.
func TestGoCoverageScopeNamedPackage(t *testing.T) {
	for _, arg := range []string{"./internal/codegen", "./internal/codegen/", "./internal/codegen/..."} {
		coverPkgs, testPkgs, err := goCoverageScope("", arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if coverPkgs != "./internal/codegen" {
			t.Errorf("%s: coverPkgs = %q, want ./internal/codegen", arg, coverPkgs)
		}
		if testPkgs != "./internal/codegen/..." {
			t.Errorf("%s: testPkgs = %q, want ./internal/codegen/...", arg, testPkgs)
		}
	}
}

// A package with no sibling test packages must still measure itself: the
// recursive pattern resolves to just that package, so the extra reach costs
// nothing where there is nothing to reach.
func TestGoCoverageScopeLeafPackage(t *testing.T) {
	coverPkgs, testPkgs, err := goCoverageScope("", "./internal/types")
	if err != nil {
		t.Fatal(err)
	}
	if coverPkgs != "./internal/types" || testPkgs != "./internal/types/..." {
		t.Errorf("got (%q, %q), want (./internal/types, ./internal/types/...)", coverPkgs, testPkgs)
	}
}

package buildrun

import (
	"path/filepath"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// The standing reproduction of T2094. Both tests build their own 8.3 short/long
// pair through clitest.ShortNameDir rather than hoping the host's %TEMP% carries
// one, so the case runs on every Windows bin/verify instead of only on a runner
// whose temp directory happens to be spelled short. See docs/code-style.md
// §"Path comparisons in tests".

// TestSameDirDistinguishesSpellingFromIdentity pins the comparison behind every
// path assertion in this package, in both directions: it must accept two
// spellings of one directory, and still reject two directories. T2094 was the
// first half failing (a raw got compared against a normalized want); a helper
// rewritten to answer true unconditionally would pass the first half and assert
// nothing, which is the second half's job to catch.
func TestSameDirDistinguishesSpellingFromIdentity(t *testing.T) {
	t.Parallel()
	long, short := clitest.ShortNameDir(t)

	// Symmetric on purpose: T2094 normalized one side, so a comparison that is
	// only correct in one direction is exactly the defect.
	for _, c := range []struct{ got, want string }{{short, long}, {long, short}} {
		same, gotErr, wantErr := sameDir(c.got, c.want)
		if gotErr != nil || wantErr != nil {
			t.Fatalf("test setup is broken: sameDir(%q, %q) could not stat (got: %v, want: %v)",
				c.got, c.want, gotErr, wantErr)
		}
		if !same {
			t.Errorf("sameDir(%q, %q) = false, want true - one directory under its long and 8.3 short spellings",
				c.got, c.want)
		}
	}

	other := clitest.TempDir(t)
	if same, _, _ := sameDir(short, other); same {
		t.Errorf("sameDir(%q, %q) = true, want false - two different directories, not two spellings of one",
			short, other)
	}
}

// TestSameDirNamesWhichSideDoesNotStat pins the distinction assertSameDir routes
// its two failure paths on, and which nothing else can observe: a got that does
// not stat is a product failure (Errorf - the run continues and later assertions
// still report), a want that does not stat is a broken fixture (Fatalf - the test
// stops, because every assertion after it would be meaningless). Getting the two
// backwards would turn a real product failure into a halted run, or a typo in a
// fixture into a bug report against the compiler.
func TestSameDirNamesWhichSideDoesNotStat(t *testing.T) {
	t.Parallel()
	exists := clitest.TempDir(t)
	missing := filepath.Join(exists, "no-such-directory")

	if same, gotErr, wantErr := sameDir(missing, exists); same || gotErr == nil || wantErr != nil {
		t.Errorf("sameDir(%q, %q) = (%v, %v, %v), want (false, non-nil, nil) - "+
			"the got side is what did not stat", missing, exists, same, gotErr, wantErr)
	}

	if same, gotErr, wantErr := sameDir(exists, missing); same || gotErr != nil || wantErr == nil {
		t.Errorf("sameDir(%q, %q) = (%v, %v, %v), want (false, nil, non-nil) - "+
			"the want side is what did not stat", exists, missing, same, gotErr, wantErr)
	}
}

// TestWorkingDirFromShortNamedCwd is the end-to-end shape that reddened trunk in
// T2094: a child invoked with its cwd spelled as an 8.3 short name reports that
// spelling back, while the test's own path for the same directory is the long
// one. The assertion has to hold across the two, and nothing about the product
// is wrong here - promise preserves whatever spelling it is handed.
func TestWorkingDirFromShortNamedCwd(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping exec integration test in short mode")
	}
	long, short := clitest.ShortNameDir(t)
	bin := clitest.Bin(t)

	// autoInjectCatalogUses pulls in `use os;` from the bare os.working_dir
	// reference, as in TestExecHasNoSrcDir. exec keys its build cache on the
	// source text, so this costs one compile for the package rather than one per
	// directory.
	out := runProbe(t, short, bin, "exec", `print_line("working_dir=" + os.working_dir?!);`)
	got := probeField(t, out, "working_dir")

	if got == long {
		t.Fatalf("this no longer reproduces T2094: the child was given the cwd %q and reported %q, "+
			"so getcwd no longer answers with the spelling CreateProcess was handed. Re-derive a "+
			"fixture where the two sides differ, or retire this test - as written it now asserts nothing.",
			short, long)
	}
	assertSameDir(t, got, long, "os.working_dir from an 8.3 short-named cwd")
}

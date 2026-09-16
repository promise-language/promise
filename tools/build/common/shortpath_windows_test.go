//go:build windows

package common

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// The Windows half of T1243's coverage. The three reproductions in
// gate_test_json_test.go all skip here, so until now canonPath and relToBase had
// no coverage at all on the platform the bug was reported from: the alias they
// exist to reconcile — an 8.3 short name like RUNNER~1 against the long
// runneradmin — is a Windows filesystem feature, and those tests stand in for it
// with a POSIX symlink. These build the real thing, so the case runs on every
// Windows bin/verify rather than only where a runner's %TEMP% happens to carry a
// short component (T2125). See docs/code-style.md §"Path comparisons in tests".

var procGetShortPathNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetShortPathNameW")

// shortNameDir returns one directory under both spellings Windows gives it: the
// long name it was created with, and the 8.3 short name generated for it. It
// skips the caller where the pair cannot exist — a volume with 8dot3 name
// creation disabled.
//
// This repeats clitest.ShortNameDir in the compiler module deliberately:
// tools/build is a separate Go module that does not require the compiler, so
// there is no import path between them, and the alternative to a dozen lines of
// syscall wrapper is a module dependency in the wrong direction. It reaches
// GetShortPathNameW through syscall rather than golang.org/x/sys/windows for the
// same reason — a test helper is not a reason to give the build tools a new
// direct dependency.
func shortNameDir(t *testing.T) (long, short string) {
	t.Helper()
	// Longer than 8 characters and not itself 8.3-shaped, so the filesystem has
	// to generate an alias rather than reusing the name.
	long = filepath.Join(t.TempDir(), "promise-short-name-fixture")
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", long, err)
	}
	short = shortPathName(t, long)
	if short == long {
		t.Skipf("no 8.3 alias for %s — 8dot3 name creation is disabled on its volume "+
			"(check with `fsutil 8dot3name query %s`), so the short/long pair cannot be built here",
			long, filepath.VolumeName(long))
	}
	return long, short
}

// shortPathName asks Windows for path's 8.3 spelling. It returns path unchanged
// when the path has no alias — GetShortPathNameW reports the long form rather
// than failing, which is how shortNameDir tells a disabled volume from a working
// one.
func shortPathName(t *testing.T, path string) string {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16 %s: %v", path, err)
	}
	buf := make([]uint16, syscall.MAX_PATH)
	n, _, errno := procGetShortPathNameW.Call(
		uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n > uintptr(len(buf)) {
		// Buffer was too small; n is the required size and buf is untouched.
		buf = make([]uint16, n)
		n, _, errno = procGetShortPathNameW.Call(
			uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), n)
	}
	if n == 0 {
		t.Fatalf("GetShortPathNameW %s: %v", path, errno)
	}
	return syscall.UTF16ToString(buf[:n])
}

// TestRelToBaseShortNameFormDiffersFromBase is TestRelToBaseFileFormDiffersFromBase
// on a genuine 8.3 alias rather than a symlink standing in for one: base is the
// long spelling, the file is reported through the short one, and its leaf does
// not exist on disk (the reported test file, or an already-removed worktree src/).
func TestRelToBaseShortNameFormDiffersFromBase(t *testing.T) {
	long, short := shortNameDir(t)

	rel, err := relToBase(long, filepath.Join(short, "tests", "e2e", "basics.pr"))
	if err != nil {
		t.Fatalf("relToBase = %v, want nil (T1243)", err)
	}
	if rel != "tests/e2e/basics.pr" {
		t.Errorf("rel = %q, want tests/e2e/basics.pr", rel)
	}
}

// TestRelToBaseShortNameStillDetectsOutside is the other half: canonicalizing
// both sides must not turn "outside base" into a pass. A sibling of the short
// spelling is still outside it under either spelling.
func TestRelToBaseShortNameStillDetectsOutside(t *testing.T) {
	long, short := shortNameDir(t)

	outside := filepath.Join(filepath.Dir(short), "elsewhere", "outside.pr")
	if _, err := relToBase(long, outside); err == nil {
		t.Fatalf("relToBase(%q, %q) = nil, want an error - the file is outside base "+
			"under either spelling", long, outside)
	}
}

// TestCanonPathShortNameLongestExistingPrefix verifies canonPath expands an 8.3
// prefix and appends the non-existent tail unchanged — the mechanism underpinning
// the T1243 fix, on the alias it was written for.
func TestCanonPathShortNameLongestExistingPrefix(t *testing.T) {
	long, short := shortNameDir(t)

	got := canonPath(filepath.Join(short, "no", "such", "leaf.pr"))
	want := filepath.Join(long, "no", "such", "leaf.pr")
	if got != want {
		t.Errorf("canonPath = %q, want %q", got, want)
	}
}

// TestBuildGateOutputShortNameFormDiffersFromBase drives the fix through
// BuildGateOutput's actual file-grouping call site: base is the long spelling,
// the JSONL records report their files through the 8.3 short one with
// non-existent leaves — the shape Windows CI produces when base is runneradmin
// and the runner reports RUNNER~1.
func TestBuildGateOutputShortNameFormDiffersFromBase(t *testing.T) {
	long, short := shortNameDir(t)

	jsonl := strings.Join([]string{
		jsonlLine(short, "tests/std/bool_test.pr", "test_and", "pass"),
		jsonlLine(short, "tests/e2e/hello.pr", "main", "pass"),
	}, "\n")

	out, err := BuildGateOutput(long, "windows-amd64", "host", "promise-tests", jsonl)
	if err != nil {
		t.Fatalf("BuildGateOutput = %v, want nil (T1243)", err)
	}
	if len(out.Files) != 2 {
		t.Fatalf("want 2 file groups, got %d: %+v", len(out.Files), out.Files)
	}
	if out.Files[0].File != "tests/std/bool_test.pr" {
		t.Errorf("file[0] = %q, want tests/std/bool_test.pr", out.Files[0].File)
	}
	if out.Files[1].File != "tests/e2e/hello.pr" {
		t.Errorf("file[1] = %q, want tests/e2e/hello.pr", out.Files[1].File)
	}
}

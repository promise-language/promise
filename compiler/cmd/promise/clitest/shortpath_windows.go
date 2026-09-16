//go:build windows

package clitest

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// ShortNameDir returns one directory under both spellings Windows gives it: the
// long name it was created with, and the 8.3 short name the filesystem generated
// for it (…\PROMIS~1). Both name the same directory; os.SameFile says so and a
// string comparison does not.
//
// It exists so the suite *manufactures* that pair rather than waiting for a host
// whose %TEMP% happens to carry one. T2094 was a test comparing a child's getcwd
// against a normalized want: it could only fail where a path component had a
// short alias, so it passed on every developer clone and reddened trunk on the
// GitHub runner (C:\Users\RUNNER~1) the first time it ran on Windows at all —
// it landed while the last windows-amd64 CI run was three weeks old, so nothing
// had put it in front of the hazard until a manually dispatched run did. T1243
// was the same hazard at a different call site. Nothing about the pair needs a
// short %TEMP% — only a directory that has an alias, which any Windows host with
// 8dot3 name creation enabled can supply.
//
// tools/build/common/shortpath_windows_test.go carries a deliberate copy of this
// for T1243's own call site; that module cannot import this one. Keep the two in
// step.
//
// The test is skipped where the pair cannot exist: every non-Windows platform,
// and a Windows volume with 8dot3 name creation disabled. That is the absence of
// the hazard, not of the coverage.
func ShortNameDir(t *testing.T) (long, short string) {
	t.Helper()
	// Longer than 8 characters and not itself 8.3-shaped, so the filesystem has
	// to generate an alias rather than reusing the name.
	long = filepath.Join(t.TempDir(), "promise-short-name-fixture")
	if err := os.MkdirAll(long, 0755); err != nil {
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
// when the path has no alias — GetShortPathName reports the long form rather
// than failing, which is how ShortNameDir tells a disabled volume from a working
// one.
func shortPathName(t *testing.T, path string) string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16 %s: %v", path, err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatalf("GetShortPathName %s: %v", path, err)
	}
	if n > uint32(len(buf)) {
		// Buffer was too small; n is the required size and buf is untouched.
		buf = make([]uint16, n)
		if n, err = windows.GetShortPathName(p, &buf[0], n); err != nil {
			t.Fatalf("GetShortPathName %s: %v", path, err)
		}
	}
	return windows.UTF16ToString(buf[:n])
}

//go:build !windows

package clitest

import "testing"

// ShortNameDir skips the caller off Windows: 8.3 short names are a Windows
// filesystem feature, so there is no second spelling to hand out. See the
// Windows build of this file for what the pair is and why the suite builds one.
func ShortNameDir(t *testing.T) (long, short string) {
	t.Helper()
	t.Skip("8.3 short names exist only on Windows")
	return "", ""
}

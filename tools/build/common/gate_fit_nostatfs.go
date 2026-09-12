//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux

package common

import (
	"fmt"
	"runtime"
)

// freeBytes has no standard-library answer on this platform. Saying so makes
// `fit` fail to measure — which the SDK reads as unfit — rather than report a
// number nobody measured.
func freeBytes(path string) (int64, error) {
	return 0, fmt.Errorf("free space at %s is not measurable from the standard library on %s/%s",
		path, runtime.GOOS, runtime.GOARCH)
}

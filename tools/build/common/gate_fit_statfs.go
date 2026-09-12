//go:build aix || darwin || dragonfly || freebsd || linux

package common

import "syscall"

// freeBytes reports the space available to an unprivileged caller on the
// filesystem holding path — Bavail, not Bfree: the blocks reserved for root are
// not space this project's work can use.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

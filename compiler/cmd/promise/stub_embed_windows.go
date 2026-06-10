//go:build windows

package main

import (
	"errors"
	"syscall"
)

// isRetryableRenameError reports whether a rename failure is a transient
// sharing/lock error that MoveFileEx can return under antivirus or search-indexer
// contention, and is therefore worth retrying after a short backoff.
func isRetryableRenameError(err error) bool {
	return errors.Is(err, syscall.Errno(5)) || // ERROR_ACCESS_DENIED
		errors.Is(err, syscall.Errno(32)) // ERROR_SHARING_VIOLATION
}

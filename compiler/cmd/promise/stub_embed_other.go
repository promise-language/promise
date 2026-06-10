//go:build !windows

package main

// isRetryableRenameError always returns false on Unix: rename(2) is atomic, so a
// rename error is real and must propagate immediately rather than be retried.
func isRetryableRenameError(error) bool { return false }

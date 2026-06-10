//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// inodeOf returns the inode number of path (Unix only).
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() is not *syscall.Stat_t for %s", path)
	}
	return uint64(st.Ino)
}

// TestCopyFileFreshInode is the core regression guard for T0722: copyFile must
// place the destination on a FRESH inode (temp + rename), never overwrite the
// existing inode in place. An in-place rewrite would let a freshly installed
// binary inherit poisoned vnode/kernel state attached to the prior inode (the
// macOS amfid-wedge failure).
func TestCopyFileFreshInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing dst with different content — capture its inode.
	if err := os.WriteFile(dst, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	oldIno := inodeOf(t, dst)

	copyFile(src, dst, 0755)

	newIno := inodeOf(t, dst)
	if newIno == oldIno {
		t.Fatalf("copyFile reused inode %d in place; expected a fresh inode (T0722)", oldIno)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-binary" {
		t.Fatalf("expected 'new-binary' content, got %q", string(data))
	}
}

// TestWriteFileAtomicFreshInode guards the fresh-inode invariant on
// writeFileAtomic directly, not just through copyFile. Release-build stub
// installs (writeStubAndSidecar) overwrite ~/.promise/bin/promise via this
// function, so the safe-replace guarantee — a rename onto a brand new inode,
// never an in-place rewrite of the existing one — must hold here too (T0722).
func TestWriteFileAtomicFreshInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub")
	if err := os.WriteFile(path, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	oldIno := inodeOf(t, path)

	if err := writeFileAtomic(path, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}

	if newIno := inodeOf(t, path); newIno == oldIno {
		t.Fatalf("writeFileAtomic reused inode %d in place; expected a fresh inode (T0722)", oldIno)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("expected 'new' content, got %q", string(data))
	}
}

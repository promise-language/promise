package codegen

import "testing"

// T1520: every durability bridge in file_io.go must bracket its PAL call in the
// scheduler's syscall handoff. This is mandatory rather than stylistic: a
// blocking pal_file_lock parks the OS thread inside flock/LockFileEx, and without
// releasing this M's P first it would wedge that P for as long as another process
// holds the lock — which for a declared exclusion is measured in minutes.
//
// The bridges are reached by declaring their externs directly, exactly as
// modules/io/io.pr does; defineFileIOBodies matches on the LLVM symbol name.
func TestDurabilityBridgesUseSyscallHandoff(t *testing.T) {
	for _, tc := range []struct{ name, decl, call, pal string }{
		{
			"rename",
			"_rename(string from, string to) int `extern(\"promise_io_file_rename\");",
			"_rename(\"a\", \"b\");",
			"@pal_file_rename",
		},
		{
			"sync",
			"_sync(int fd) int `extern(\"promise_io_file_sync\");",
			"_sync(1);",
			"@pal_file_sync",
		},
		{
			"dir_sync",
			"_dsync(string path) int `extern(\"promise_io_dir_sync\");",
			"_dsync(\"d\");",
			"@pal_dir_sync",
		},
		{
			"lock",
			"_lock(int fd, int excl, int nb) int `extern(\"promise_io_file_lock\");",
			"_lock(1, 1, 0);",
			"@pal_file_lock",
		},
		{
			"unlock",
			"_unlock(int fd) int `extern(\"promise_io_file_unlock\");",
			"_unlock(1);",
			"@pal_file_unlock",
		},
		{
			"truncate",
			"_trunc(int fd, int len) int `extern(\"promise_io_file_truncate\");",
			"_trunc(1, 0);",
			"@pal_file_truncate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ir := generateIR(t, tc.decl+"\nmain() { "+tc.call+" }")
			assertContainsMatch(t, ir,
				`(?s)call void @promise_sched_enter_syscall\(\)\s*%\d+ = call i32 `+
					tc.pal+`\([^\n]*\n\s*call void @promise_sched_exit_syscall\(\)`)
		})
	}
}

// T1520: the two path-taking bridges free the C strings they allocate. A leak
// here would be invisible to the Promise-level leak checker, which counts
// allocations made by Promise code — not ones the bridge makes on its behalf.
func TestDurabilityBridgesFreePathStrings(t *testing.T) {
	ir := generateIR(t, "_rename(string from, string to) int `extern(\"promise_io_file_rename\");\n"+
		"main() { _rename(\"a\", \"b\"); }")
	assertContainsMatch(t, ir,
		`(?s)define void @promise_io_file_rename\(.*?@pal_free\(.*?@pal_free\(.*?ret void`)

	ir = generateIR(t, "_dsync(string path) int `extern(\"promise_io_dir_sync\");\n"+
		"main() { _dsync(\"d\"); }")
	assertContainsMatch(t, ir,
		`(?s)define void @promise_io_dir_sync\(.*?@pal_free\(.*?ret void`)
}

// T1520: the bridges marshal Promise ints into the PAL's argument types, and a
// wrong width is silent — every value these tests pass is small. A file length
// narrowed to i32 would truncate a reclaimed slot to the wrong size somewhere
// past 2 GiB, and a descriptor widened past i32 would not match the PAL
// signature at all. Both are pinned by shape rather than by behaviour.
func TestDurabilityBridgeArgumentWidths(t *testing.T) {
	ir := generateIR(t, "_trunc(int fd, int len) int `extern(\"promise_io_file_truncate\");\n"+
		"main() { _trunc(1, 0); }")
	assertContainsMatch(t, ir, `call i32 @pal_file_truncate\(i32 %\w+, i64 %\w+\)`)

	ir = generateIR(t, "_lock(int fd, int excl, int nb) int `extern(\"promise_io_file_lock\");\n"+
		"main() { _lock(1, 1, 0); }")
	assertContainsMatch(t, ir, `call i32 @pal_file_lock\(i32 %\w+, i32 %\w+, i32 %\w+\)`)

	ir = generateIR(t, "_sync(int fd) int `extern(\"promise_io_file_sync\");\n"+
		"main() { _sync(1); }")
	assertContainsMatch(t, ir, `call i32 @pal_file_sync\(i32 %\w+\)`)
}

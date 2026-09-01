package codegen

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// T0772: Windows zero-dependency link surface. These tests pin the IR shape of
// the self-contained crt0 entry, the CRT-replacement runtime support symbols
// (_tls_used / __chkstk / _fltused), and the _beginthreadex → CreateThread
// switch, so the compiler keeps emitting a link surface that needs no MSVC /
// Windows SDK files.

const winTarget = "x86_64-pc-windows-msvc"

// TestWindowsCrt0Entry verifies the @__promise_start entry point performs the
// UCRT app-init sequence, reads argc/argv, calls @main, and exits.
func TestWindowsCrt0Entry(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	assertContains(t, ir, "define void @__promise_start()")
	// UCRT narrow app-init (parses command line + populates _environ).
	assertContains(t, ir, "@_configure_narrow_argv")
	assertContains(t, ir, "@_initialize_narrow_environment")
	assertContains(t, ir, "@__p___argc")
	assertContains(t, ir, "@__p___argv")
	// Calls the program entry and exits with its return code.
	assertContains(t, ir, "call i32 @main(")
	// Old MSVC CRT entry must not appear anywhere.
	assertNotContains(t, ir, "mainCRTStartup")
	assertNotContains(t, ir, "__getmainargs")
}

// TestWindowsTLSSupport verifies the loader-visible TLS directory and the
// _fltused FP marker are emitted, so __declspec(thread) globals (scheduler +
// panic flags) get per-thread storage without the MSVC CRT's tlssup.
func TestWindowsTLSSupport(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	assertContains(t, ir, "@_tls_used =")
	assertContains(t, ir, "@_tls_index =")
	assertContains(t, ir, "@_tls_start =")
	assertContains(t, ir, "@_tls_end =")
	assertContains(t, ir, "@_fltused =")
	// Section placement the Windows loader requires.
	assertContains(t, ir, `section ".tls"`)
	assertContains(t, ir, `section ".tls$ZZZ"`)
	assertContains(t, ir, `section ".CRT$XLA"`)
	assertContains(t, ir, `section ".rdata$T"`)
}

// TestWindowsChkstk verifies __chkstk is emitted as a naked inline-asm function
// (compiler-rt's Windows builtins lib does not provide it, and no Windows DLL
// exports it).
func TestWindowsChkstk(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	assertContains(t, ir, "define void @__chkstk()")
	assertContains(t, ir, "naked")
	assertContains(t, ir, "call void asm sideeffect")
}

// TestWindowsThreadCreateUsesCreateThread verifies pal_thread_create launches
// worker threads via kernel32 CreateThread (always-present DLL) rather than the
// UCRT _beginthreadex — safe because panic recovery is TLS-flag-based, not
// setjmp/longjmp (T0146).
func TestWindowsThreadCreateUsesCreateThread(t *testing.T) {
	ir := generateIRForTarget(t, `main() { go { print_line("g"); } }`, winTarget)

	assertContains(t, ir, "@CreateThread")
	assertContains(t, ir, "call i8* @pal_thread_create")
	assertNotContains(t, ir, "_beginthreadex")
}

// TestWindowsRuntimeSupportEmittedOnce guards the idempotency of
// emitWindowsEntry — the support symbols must be defined exactly once to avoid
// duplicate-symbol link errors.
func TestWindowsRuntimeSupportEmittedOnce(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	for _, sym := range []string{"@__promise_start()", "@__chkstk()", "@_tls_used =", "@_fltused ="} {
		if got := strings.Count(ir, sym); got != 1 {
			t.Errorf("expected exactly one definition of %q, got %d", sym, got)
		}
	}
}

// TestEmitWindowsEntryGuard exercises the windowsRuntimeEmitted early-return
// directly: both entry paths (wrapMainWithScheduler and GenerateTestMain) call
// emitWindowsEntry, but only the guard prevents a duplicate-symbol link error if
// both ever run. Calling it twice on the same Compiler must emit the support
// symbols exactly once.
func TestEmitWindowsEntryGuard(t *testing.T) {
	c := &Compiler{module: ir.NewModule()}
	c.palExit = c.module.NewFunc("pal_exit", irtypes.Void, ir.NewParam("code", irtypes.I32))
	mainFn := c.module.NewFunc("main", irtypes.I32,
		ir.NewParam("argc", irtypes.I32),
		ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)))

	c.emitWindowsEntry(mainFn)
	if !c.windowsRuntimeEmitted {
		t.Fatal("windowsRuntimeEmitted should be set after the first call")
	}
	// Second call must hit the guard and return without re-emitting anything.
	c.emitWindowsEntry(mainFn)

	out := c.module.String()
	for _, sym := range []string{"@__promise_start()", "@__chkstk()", "@_tls_used =", "@_fltused ="} {
		if got := strings.Count(out, sym); got != 1 {
			t.Errorf("after double emitWindowsEntry: %q defined %d times, want 1", sym, got)
		}
	}
}

// TestWindowsExternalSymbolsAreExported audits the whole external link surface:
// every `declare` left in a Windows program's IR must be either defined in the
// same module or listed in one of the .def symbol lists that generate the import
// libraries. A hand-written extern nobody exports still links against the
// self-generated libs but fails at *load* time with a missing-entry-point error,
// which is a miserable way to find out — and the SChannel TLS backend (T1598)
// adds ~40 such externs by hand across secur32/crypt32/ncrypt.
func TestWindowsExternalSymbolsAreExported(t *testing.T) {
	exported := winlinkExportedSymbols(t)
	declRe := regexp.MustCompile(`(?m)^declare[^@]*@"?([A-Za-z0-9_.$]+)"?\(`)

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"plain", `main() { print_line("hi"); }`},
		{"tls", tlsAllExternsSrc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, info := parseWithStd(t, tc.src)
			result := Compile(file, info, winTarget)

			defined := make(map[string]bool)
			for _, fn := range result.Module.Funcs {
				if len(fn.Blocks) > 0 {
					defined[fn.Name()] = true
				}
			}
			for _, m := range declRe.FindAllStringSubmatch(result.Module.String(), -1) {
				sym := m[1]
				// llvm.* are intrinsics the backend lowers; they never reach the linker.
				if defined[sym] || exported[sym] || strings.HasPrefix(sym, "llvm.") {
					continue
				}
				t.Errorf("external symbol @%s is declared but exported by no .def in %s "+
					"— add it to the matching import-lib symbol list", sym, winlinkDefDirForTest)
			}
		})
	}
}

// winlinkDefDirForTest is the .def source directory, relative to the repo root.
const winlinkDefDirForTest = "tools/build/winlink/def"

// winlinkExportedSymbols parses every .def symbol list into a set.
func winlinkExportedSymbols(t *testing.T) map[string]bool {
	t.Helper()
	defs, err := filepath.Glob(filepath.Join("..", "..", "..",
		filepath.FromSlash(winlinkDefDirForTest), "*.def"))
	if err != nil || len(defs) == 0 {
		t.Fatalf("no .def symbol lists found (err=%v)", err)
	}
	exported := make(map[string]bool)
	for _, path := range defs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, ";") ||
				strings.HasPrefix(line, "LIBRARY") || line == "EXPORTS" {
				continue
			}
			exported[line] = true
		}
	}
	return exported
}

// TestWindowsFileOpenLinkSurface pins the import-lib side of T1742 — the half
// TestWindowsExternalSymbolsAreExported cannot see.
//
// That test walks declarations → .def, so it catches an extern nobody exports.
// It says nothing about a .def entry with no declaration, and a tidy-up pass that
// drops "unused" symbol-list entries would silently take a whole primitive with
// it — nothing else in the tree would notice until a Windows build failed to
// link, on a host this repo cannot reach from macOS or Linux. Each entry below
// therefore names the consumer that would break.
func TestWindowsFileOpenLinkSurface(t *testing.T) {
	exported := winlinkExportedSymbols(t)

	for _, tc := range []struct{ sym, why string }{
		{"CreateFileA", "opens files with FILE_SHARE_DELETE (T1742)"},
		{"CloseHandle", "releases the HANDLE when _open_osfhandle refuses it"},
		{"GetLastError", "CreateFileA reports failure here, not via errno"},
		{"_open_osfhandle", "wraps the HANDLE in the CRT fd that _read/_write use"},
		{"_get_osfhandle", "recovers the HANDLE for FlushFileBuffers / LockFileEx (T1520)"},
		{"FlushFileBuffers", "pal_file_sync forces contents to stable storage (T1520)"},
		{"LockFileEx", "pal_file_lock takes the whole-file advisory lock (T1520)"},
		{"UnlockFileEx", "pal_file_unlock releases it (T1520)"},
		{"MoveFileExA", "pal_file_rename replaces atomically and write-through (T1520)"},
		{"_chsize_s", "pal_file_truncate empties a reclaimed temporary slot (T1520)"},
	} {
		if !exported[tc.sym] {
			t.Errorf("%s is not exported by any .def in %s — %s",
				tc.sym, winlinkDefDirForTest, tc.why)
		}
	}

	// The migration is complete, not partial: UCRT _open grants no delete
	// sharing, so leaving it on the link surface invites a caller straight back
	// into the behaviour T1742 removed.
	if exported["_open"] {
		t.Error("_open is still exported from the UCRT symbol list — pal_file_open " +
			"no longer uses it, and no CRT open can grant FILE_SHARE_DELETE")
	}
}

// TestWindowsProgramOpensViaCreateFile checks the whole-program IR, not just the
// PAL emitter in isolation: a plain Windows build must reach files through
// CreateFileA and must not smuggle in a second, share-mode-less open path.
func TestWindowsProgramOpensViaCreateFile(t *testing.T) {
	out := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	for _, want := range []string{
		"declare i8* @CreateFileA(",
		"declare i32 @_open_osfhandle(",
		"define i32 @pal_file_open(",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Windows program IR is missing %q", want)
		}
	}
	// i32 7 = FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE, passed as the
	// dwShareMode literal of the single CreateFileA call in pal_file_open.
	if !strings.Contains(out, "@CreateFileA(i8* %path,") {
		t.Error("pal_file_open does not pass its path parameter to CreateFileA")
	}
	if !strings.Contains(out, ", i32 7, i8* null,") {
		t.Error("CreateFileA is called without FILE_SHARE_DELETE in dwShareMode")
	}
	if strings.Contains(out, "@_open(") {
		t.Error("Windows program still references UCRT @_open — pal_file_open was " +
			"migrated to CreateFileA, so any remaining caller opens without delete sharing")
	}
}

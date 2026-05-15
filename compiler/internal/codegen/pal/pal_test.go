package pal

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
)

func TestPosixPALEmitWrite(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitWrite(module)

	out := module.String()

	// libc write declaration
	if !strings.Contains(out, "declare i64 @write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @write declaration")
	}
	// pal_write wrapper definition
	if !strings.Contains(out, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @pal_write definition")
	}
	// wrapper calls through to write
	if !strings.Contains(out, "call i64 @write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing call to @write in pal_write body")
	}
	// verify return
	if fn.Name() != "pal_write" {
		t.Errorf("expected function name pal_write, got %s", fn.Name())
	}
}

func TestPosixPALEmitExit(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitExit(module)

	out := module.String()

	// libc exit declaration with noreturn
	if !strings.Contains(out, "@exit(i32 %code)") {
		t.Error("missing @exit declaration")
	}
	// pal_exit wrapper definition
	if !strings.Contains(out, "define void @pal_exit(i32 %code)") {
		t.Error("missing @pal_exit definition")
	}
	// wrapper calls exit and is unreachable
	if !strings.Contains(out, "call void @exit(i32 %code)") {
		t.Error("missing call to @exit in pal_exit body")
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("missing unreachable in pal_exit body")
	}
	if fn.Name() != "pal_exit" {
		t.Errorf("expected function name pal_exit, got %s", fn.Name())
	}
}

// --- Windows PAL Tests ---

func TestWindowsPALEmitWrite(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitWrite(module)

	out := module.String()

	// Win32 GetStdHandle declaration
	if !strings.Contains(out, "@GetStdHandle(i32 %nStdHandle)") {
		t.Error("missing @GetStdHandle declaration")
	}
	// Win32 WriteFile declaration
	if !strings.Contains(out, "@WriteFile(") {
		t.Error("missing @WriteFile declaration")
	}
	// pal_write wrapper definition
	if !strings.Contains(out, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @pal_write definition")
	}
	// fd-to-handle mapping: sub i32 -10, %fd
	if !strings.Contains(out, "sub i32 -10, %fd") {
		t.Error("missing fd-to-handle mapping (sub i32 -10, fd)")

	}
	// Call to GetStdHandle
	if !strings.Contains(out, "call i8* @GetStdHandle(") {
		t.Error("missing call to @GetStdHandle")
	}
	// Truncation of len to i32
	if !strings.Contains(out, "trunc i64 %len to i32") {
		t.Error("missing trunc of len to i32")
	}
	// Alloca for bytes written
	if !strings.Contains(out, "alloca i32") {
		t.Error("missing alloca i32 for bytes written")
	}
	// Call to WriteFile
	if !strings.Contains(out, "call i32 @WriteFile(") {
		t.Error("missing call to @WriteFile")
	}
	// Null overlapped pointer passed to WriteFile
	if !strings.Contains(out, "null") {
		t.Error("missing null pointer for lpOverlapped")
	}
	// Load written count from alloca before zext
	if !strings.Contains(out, "load i32, i32*") {
		t.Error("missing load of written bytes from alloca")
	}
	// Zero-extend written to i64
	if !strings.Contains(out, "zext i32") {
		t.Error("missing zext of written bytes to i64")
	}
	if fn.Name() != "pal_write" {
		t.Errorf("expected function name pal_write, got %s", fn.Name())
	}
}

func TestWindowsPALEmitExit(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitExit(module)

	out := module.String()

	// Win32 ExitProcess declaration with noreturn
	if !strings.Contains(out, "@ExitProcess(i32 %uExitCode)") {
		t.Error("missing @ExitProcess declaration")
	}
	// noreturn attribute on pal_exit wrapper
	if !strings.Contains(out, "noreturn") {
		t.Error("missing noreturn attribute on pal_exit")
	}
	// pal_exit wrapper definition
	if !strings.Contains(out, "define void @pal_exit(i32 %code)") {
		t.Error("missing @pal_exit definition")
	}
	// wrapper calls ExitProcess and is unreachable
	if !strings.Contains(out, "call void @ExitProcess(i32 %code)") {
		t.Error("missing call to @ExitProcess in pal_exit body")
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("missing unreachable in pal_exit body")
	}
	if fn.Name() != "pal_exit" {
		t.Errorf("expected function name pal_exit, got %s", fn.Name())
	}
}

// --- WASM PAL Tests ---

func TestWasmPALEmitWrite(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}
	fn := p.EmitWrite(module)

	out := module.String()

	// WASI fd_write declaration
	if !strings.Contains(out, "@fd_write(") {
		t.Error("missing @fd_write declaration")
	}
	// pal_write wrapper definition
	if !strings.Contains(out, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)") {
		t.Error("missing @pal_write definition")
	}
	// iovec alloca (anonymous struct {i8*, i32})
	if !strings.Contains(out, "alloca { i8*, i32 }") {
		t.Error("missing iovec alloca")
	}
	// GEP into iovec fields
	if !strings.Contains(out, "getelementptr { i8*, i32 }") {
		t.Error("missing GEP into iovec struct")
	}
	// Truncation of len to i32 for iovec
	if !strings.Contains(out, "trunc i64 %len to i32") {
		t.Error("missing trunc of len to i32 for iovec")
	}
	// At least 2 alloca instructions (iovec + nwritten)
	if strings.Count(out, "alloca") < 2 {
		t.Error("expected at least 2 alloca instructions (iovec + nwritten)")
	}
	// Store instructions for iovec fields (buf pointer + length)
	if strings.Count(out, "store") < 2 {
		t.Error("expected at least 2 store instructions (buf ptr + len into iovec)")
	}
	// Call to fd_write with iovs_len=1
	if !strings.Contains(out, "call i32 @fd_write(") {
		t.Error("missing call to @fd_write")
	}
	// Load nwritten from alloca before zext
	if !strings.Contains(out, "load i32, i32*") {
		t.Error("missing load of nwritten from alloca")
	}
	// Zero-extend nwritten to i64
	if !strings.Contains(out, "zext i32") {
		t.Error("missing zext of nwritten to i64")
	}
	if fn.Name() != "pal_write" {
		t.Errorf("expected function name pal_write, got %s", fn.Name())
	}
}

func TestWasmPALEmitExit(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}
	fn := p.EmitExit(module)

	out := module.String()

	// WASI proc_exit declaration
	if !strings.Contains(out, "@proc_exit(i32 %rval)") {
		t.Error("missing @proc_exit declaration")
	}
	// noreturn attribute on pal_exit wrapper
	if !strings.Contains(out, "noreturn") {
		t.Error("missing noreturn attribute on pal_exit")
	}
	// pal_exit wrapper definition
	if !strings.Contains(out, "define void @pal_exit(i32 %code)") {
		t.Error("missing @pal_exit definition")
	}
	// wrapper calls proc_exit and is unreachable
	if !strings.Contains(out, "call void @proc_exit(i32 %code)") {
		t.Error("missing call to @proc_exit in pal_exit body")
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("missing unreachable in pal_exit body")
	}
	if fn.Name() != "pal_exit" {
		t.Errorf("expected function name pal_exit, got %s", fn.Name())
	}
}

// --- Allocator tests (libc wrappers for Posix/Windows, custom for WASM) ---

func TestEmitAlloc(t *testing.T) {
	// Posix and Windows use libc malloc wrapper
	libcPals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
	}
	for _, tc := range libcPals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitAlloc(module)
			out := module.String()

			if !strings.Contains(out, "@malloc(i64") {
				t.Error("missing @malloc declaration")
			}
			if !strings.Contains(out, "noalias") {
				t.Error("missing noalias attribute")
			}
			if !strings.Contains(out, "noundef") {
				t.Error("missing noundef attribute on malloc size param")
			}
			if !strings.Contains(out, "define noalias i8* @pal_alloc(i64 %size)") {
				t.Error("missing @pal_alloc definition with noalias return")
			}
			if !strings.Contains(out, "call i8* @malloc(i64 %size)") {
				t.Error("missing call to @malloc in pal_alloc body")
			}
			if !strings.Contains(out, "nounwind") {
				t.Error("missing nounwind attribute")
			}
			if !strings.Contains(out, "willreturn") {
				t.Error("missing willreturn attribute")
			}
			if fn.Name() != "pal_alloc" {
				t.Errorf("expected function name pal_alloc, got %s", fn.Name())
			}
		})
	}

	// WASM uses linked C allocator (extern malloc)
	t.Run("Wasm", func(t *testing.T) {
		module := ir.NewModule()
		p := &WasmPAL{}
		fn := p.EmitAlloc(module)
		out := module.String()

		if fn.Name() != "pal_alloc" {
			t.Errorf("expected function name pal_alloc, got %s", fn.Name())
		}
		if !strings.Contains(out, "noalias") {
			t.Error("missing noalias attribute on pal_alloc")
		}
		if !strings.Contains(out, "nounwind") {
			t.Error("missing nounwind attribute on pal_alloc")
		}
		// Should declare extern malloc (i32 for wasm32)
		if !strings.Contains(out, "@malloc(i32") {
			t.Error("missing @malloc(i32) declaration for wasm32")
		}
		// Should trunc i64 to i32 before calling malloc
		if !strings.Contains(out, "trunc i64") {
			t.Error("missing trunc i64 to i32 for wasm32 malloc")
		}
		// Should NOT have bump allocator globals
		if strings.Contains(out, "@__promise_heap_ptr") {
			t.Error("should not have bump allocator globals")
		}
	})
}

func TestEmitFree(t *testing.T) {
	// Posix and Windows use libc free wrapper
	libcPals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
	}
	for _, tc := range libcPals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitFree(module)
			out := module.String()

			if !strings.Contains(out, "@free(i8*") {
				t.Error("missing @free declaration")
			}
			if !strings.Contains(out, "nocapture") {
				t.Error("missing nocapture attribute on @free param")
			}
			if !strings.Contains(out, "noundef") {
				t.Error("missing noundef attribute on @free param")
			}
			if !strings.Contains(out, "define void @pal_free(i8* %ptr)") {
				t.Error("missing @pal_free definition")
			}
			if !strings.Contains(out, "call void @free(i8* %ptr)") {
				t.Error("missing call to @free in pal_free body")
			}
			if !strings.Contains(out, "nounwind") {
				t.Error("missing nounwind attribute on pal_free")
			}
			if !strings.Contains(out, "willreturn") {
				t.Error("missing willreturn attribute on pal_free")
			}
			if fn.Name() != "pal_free" {
				t.Errorf("expected function name pal_free, got %s", fn.Name())
			}
		})
	}

	// WASM free calls linked C allocator's @free
	t.Run("Wasm", func(t *testing.T) {
		module := ir.NewModule()
		p := &WasmPAL{}
		fn := p.EmitFree(module)
		out := module.String()

		if fn.Name() != "pal_free" {
			t.Errorf("expected function name pal_free, got %s", fn.Name())
		}
		if !strings.Contains(out, "nounwind") {
			t.Error("missing nounwind attribute on pal_free")
		}
		if !strings.Contains(out, "willreturn") {
			t.Error("missing willreturn attribute on pal_free")
		}
		// Should call @free (not a no-op)
		if !strings.Contains(out, "@free(i8*") {
			t.Error("WASM pal_free should call @free")
		}
	})
}

// Debug free: pal_free should poison-fill (0xDE) via memset before calling free.
func TestEmitFreeDebug(t *testing.T) {
	// Linux: uses malloc_usable_size
	t.Run("Linux", func(t *testing.T) {
		module := ir.NewModule()
		p := &PosixPAL{target: "x86_64-unknown-linux-gnu", DebugFree: true}
		p.EmitAlloc(module) // needed for allocCount global
		fn := p.EmitFree(module)
		out := module.String()

		if fn.Name() != "pal_free" {
			t.Errorf("expected function name pal_free, got %s", fn.Name())
		}
		if !strings.Contains(out, "@malloc_usable_size(") {
			t.Error("debug pal_free should declare @malloc_usable_size")
		}
		if !strings.Contains(out, "@memset(") {
			t.Error("debug pal_free should call @memset for poison fill")
		}
		if !strings.Contains(out, "call void @free(") {
			t.Error("debug pal_free should still call @free")
		}
	})

	// macOS: uses malloc_size (not malloc_usable_size)
	t.Run("Darwin", func(t *testing.T) {
		module := ir.NewModule()
		p := &PosixPAL{target: "arm64-apple-darwin24.3.0", DebugFree: true}
		p.EmitAlloc(module)
		fn := p.EmitFree(module)
		out := module.String()

		if fn.Name() != "pal_free" {
			t.Errorf("expected function name pal_free, got %s", fn.Name())
		}
		if !strings.Contains(out, "@malloc_size(") {
			t.Error("Darwin debug pal_free should declare @malloc_size")
		}
		if strings.Contains(out, "@malloc_usable_size(") {
			t.Error("Darwin debug pal_free should NOT use @malloc_usable_size")
		}
		if !strings.Contains(out, "@memset(") {
			t.Error("debug pal_free should call @memset for poison fill")
		}
	})

	// Windows: uses _msize
	t.Run("Windows", func(t *testing.T) {
		module := ir.NewModule()
		p := &WindowsPAL{DebugFree: true}
		p.EmitAlloc(module)
		fn := p.EmitFree(module)
		out := module.String()

		if fn.Name() != "pal_free" {
			t.Errorf("expected function name pal_free, got %s", fn.Name())
		}
		if !strings.Contains(out, "@_msize(") {
			t.Error("debug pal_free on Windows should declare @_msize")
		}
		if !strings.Contains(out, "@memset(") {
			t.Error("debug pal_free should call @memset for poison fill")
		}
	})

	// WASM: uses malloc_usable_size (provided by wasm_alloc.c)
	t.Run("Wasm", func(t *testing.T) {
		module := ir.NewModule()
		p := &WasmPAL{DebugFree: true}
		p.EmitAlloc(module)
		fn := p.EmitFree(module)
		out := module.String()

		if fn.Name() != "pal_free" {
			t.Errorf("expected function name pal_free, got %s", fn.Name())
		}
		if !strings.Contains(out, "@malloc_usable_size(") {
			t.Error("WASM debug pal_free should declare @malloc_usable_size")
		}
		if !strings.Contains(out, "@memset(") {
			t.Error("WASM debug pal_free should call @memset for poison fill")
		}
	})

	// WasmWeb: propagates DebugFree to inner WasmPAL
	t.Run("WasmWeb", func(t *testing.T) {
		module := ir.NewModule()
		p := &WasmWebPAL{DebugFree: true}
		p.EmitAlloc(module)
		fn := p.EmitFree(module)
		out := module.String()

		if fn.Name() != "pal_free" {
			t.Errorf("expected function name pal_free, got %s", fn.Name())
		}
		if !strings.Contains(out, "@malloc_usable_size(") {
			t.Error("WasmWeb debug pal_free should declare @malloc_usable_size")
		}
	})

	// Non-debug should NOT have malloc_usable_size or _msize
	t.Run("NonDebug", func(t *testing.T) {
		module := ir.NewModule()
		p := &PosixPAL{}
		p.EmitAlloc(module)
		p.EmitFree(module)
		out := module.String()

		if strings.Contains(out, "malloc_usable_size") {
			t.Error("non-debug pal_free should NOT use malloc_usable_size")
		}
		if strings.Contains(out, "@memset(") {
			t.Error("non-debug pal_free should NOT call memset")
		}
	})
}

// T0020: pal_alloc and pal_free track allocations via __promise_alloc_count.
func TestAllocTracking(t *testing.T) {
	// Posix/Windows: atomic tracking
	t.Run("Posix", func(t *testing.T) {
		module := ir.NewModule()
		p := &PosixPAL{}
		p.EmitAlloc(module)
		p.EmitFree(module)
		out := module.String()

		// Global alloc counter
		if !strings.Contains(out, "@__promise_alloc_count = global i64 0") {
			t.Error("missing @__promise_alloc_count global")
		}
		// pal_alloc atomically increments on non-null malloc
		if !strings.Contains(out, "atomicrmw add i64* @__promise_alloc_count") {
			t.Error("pal_alloc should atomically increment alloc count")
		}
		// pal_free atomically decrements on non-null ptr
		if !strings.Contains(out, "atomicrmw sub i64* @__promise_alloc_count") {
			t.Error("pal_free should atomically decrement alloc count")
		}
	})

	// WASM: non-atomic tracking (single-threaded)
	t.Run("Wasm", func(t *testing.T) {
		module := ir.NewModule()
		p := &WasmPAL{}
		p.EmitAlloc(module)
		p.EmitFree(module)
		out := module.String()

		// Global alloc counter (shared with posix)
		if !strings.Contains(out, "@__promise_alloc_count = global i64 0") {
			t.Error("missing @__promise_alloc_count global")
		}
		// WASM uses non-atomic load/add/store instead of atomicrmw
		if strings.Contains(out, "atomicrmw") {
			t.Error("WASM should NOT use atomicrmw (single-threaded)")
		}
		// But should still increment/decrement the counter
		if !strings.Contains(out, "add i64") {
			t.Error("WASM pal_alloc should increment alloc count")
		}
		if !strings.Contains(out, "sub i64") {
			t.Error("WASM pal_free should decrement alloc count")
		}
	})
}

func TestEmitRealloc(t *testing.T) {
	// Posix and Windows use libc realloc wrapper
	libcPals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
	}
	for _, tc := range libcPals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitRealloc(module)
			out := module.String()

			if !strings.Contains(out, "@realloc(i8*") {
				t.Error("missing @realloc declaration")
			}
			if !strings.Contains(out, "noalias") {
				t.Error("missing noalias attribute")
			}
			if !strings.Contains(out, "nocapture") {
				t.Error("missing nocapture attribute on realloc ptr param")
			}
			if !strings.Contains(out, "noundef") {
				t.Error("missing noundef attribute on realloc params")
			}
			if !strings.Contains(out, "define noalias i8* @pal_realloc(i8* %ptr, i64 %size)") {
				t.Error("missing @pal_realloc definition with noalias return")
			}
			if !strings.Contains(out, "call i8* @realloc(i8* %ptr, i64 %size)") {
				t.Error("missing call to @realloc in pal_realloc body")
			}
			if !strings.Contains(out, "nounwind") {
				t.Error("missing nounwind attribute on pal_realloc")
			}
			if !strings.Contains(out, "willreturn") {
				t.Error("missing willreturn attribute on pal_realloc")
			}
			if fn.Name() != "pal_realloc" {
				t.Errorf("expected function name pal_realloc, got %s", fn.Name())
			}
		})
	}

	// WASM realloc calls linked C allocator's @realloc
	t.Run("Wasm", func(t *testing.T) {
		module := ir.NewModule()
		p := &WasmPAL{}
		fn := p.EmitRealloc(module)
		out := module.String()

		if fn.Name() != "pal_realloc" {
			t.Errorf("expected function name pal_realloc, got %s", fn.Name())
		}
		if !strings.Contains(out, "noalias") {
			t.Error("missing noalias attribute on pal_realloc")
		}
		if !strings.Contains(out, "nounwind") {
			t.Error("missing nounwind attribute on pal_realloc")
		}
		// Should declare extern realloc (i32 size for wasm32)
		if !strings.Contains(out, "@realloc(i8*") {
			t.Error("missing @realloc declaration for wasm32")
		}
		// Should trunc size to i32
		if !strings.Contains(out, "trunc i64") {
			t.Error("missing trunc i64 to i32 for wasm32 realloc")
		}
		// Should NOT use __promise_memcpy
		if strings.Contains(out, "@__promise_memcpy") {
			t.Error("WASM pal_realloc should not use __promise_memcpy")
		}
	})
}

// --- ForTarget dispatch tests ---

func TestForTargetReturnsPosixPAL(t *testing.T) {
	triples := []string{
		"arm64-apple-macosx11.0.0",
		"x86_64-apple-macosx10.15.0",
		"aarch64-unknown-linux-gnu",
		"x86_64-unknown-linux-gnu",
		"aarch64-unknown-linux-musl",
		"x86_64-unknown-linux-musl",
	}
	for _, triple := range triples {
		p := ForTarget(triple)
		if _, ok := p.(*PosixPAL); !ok {
			t.Errorf("ForTarget(%q) returned %T, expected *PosixPAL", triple, p)
		}
	}
}

func TestForTargetReturnsWindowsPAL(t *testing.T) {
	triples := []string{
		"x86_64-pc-windows-msvc",
		"aarch64-pc-windows-msvc",
		"x86_64-unknown-windows-gnu",
	}
	for _, triple := range triples {
		p := ForTarget(triple)
		if _, ok := p.(*WindowsPAL); !ok {
			t.Errorf("ForTarget(%q) returned %T, expected *WindowsPAL", triple, p)
		}
	}
}

func TestForTargetReturnsWasmPAL(t *testing.T) {
	triples := []string{
		"wasm32-unknown-wasi",
		"wasm32-unknown-unknown",
		"wasm64-unknown-wasi",
	}
	for _, triple := range triples {
		p := ForTarget(triple)
		if _, ok := p.(*WasmPAL); !ok {
			t.Errorf("ForTarget(%q) returned %T, expected *WasmPAL", triple, p)
		}
	}
}

// --- Threading tests ---

// newModuleWithAlloc creates a module with pal_alloc/pal_free already emitted,
// required by threading PAL functions that allocate handles internally.
func newModuleWithAlloc(pal PAL) *ir.Module {
	module := ir.NewModule()
	pal.EmitAlloc(module)
	pal.EmitFree(module)
	return module
}

func TestEmitThreadCreate(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitThreadCreate(module)
			out := module.String()

			if !strings.Contains(out, "define i8* @pal_thread_create(i8* %fn, i8* %arg)") {
				t.Error("missing @pal_thread_create definition")
			}
			if fn.Name() != "pal_thread_create" {
				t.Errorf("expected function name pal_thread_create, got %s", fn.Name())
			}
			if !strings.Contains(out, "nounwind") {
				t.Error("missing nounwind attribute")
			}
		})
	}
}

func TestPosixThreadCreateDeclaresLibc(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{})
	(&PosixPAL{}).EmitThreadCreate(module)
	out := module.String()

	if !strings.Contains(out, "@pthread_create(") {
		t.Error("missing @pthread_create declaration")
	}
	if !strings.Contains(out, "call i32 @pthread_create(") {
		t.Error("missing call to @pthread_create")
	}
	if !strings.Contains(out, "call i8* @pal_alloc(i64 8)") {
		t.Error("missing handle allocation (8 bytes for pthread_t)")
	}
	// Verify explicit thread stack size setup (musl needs this)
	if !strings.Contains(out, "alloca [64 x i8]") {
		t.Error("missing stack-allocated pthread_attr_t (64 bytes)")
	}
	if !strings.Contains(out, "call i32 @pthread_attr_init(") {
		t.Error("missing call to @pthread_attr_init")
	}
	if !strings.Contains(out, "call i32 @pthread_attr_setstacksize(") {
		t.Error("missing call to @pthread_attr_setstacksize")
	}
	if !strings.Contains(out, "i64 u0x200000") {
		t.Error("missing 2MB stack size constant (0x200000)")
	}
	if !strings.Contains(out, "call i32 @pthread_attr_destroy(") {
		t.Error("missing call to @pthread_attr_destroy")
	}
}

func TestStubThreadCreateCallsSynchronously(t *testing.T) {
	// Only WASM uses stub threading (synchronous, no real threads)
	module := newModuleWithAlloc(&WasmPAL{})
	(&WasmPAL{}).EmitThreadCreate(module)
	out := module.String()

	// Stubs bitcast fn i8* to function pointer and call synchronously
	if !strings.Contains(out, "bitcast i8* %fn to i8* (i8*)*") {
		t.Error("missing bitcast of fn to function pointer")
	}
	// Return null handle (no real thread)
	if !strings.Contains(out, "ret i8* null") {
		t.Error("missing null return (no real thread handle)")
	}
	// Should NOT declare pthread_create
	if strings.Contains(out, "pthread_create") {
		t.Error("stub should not declare pthread_create")
	}
}

func TestWindowsThreadCreateUsesBeginthreadex(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitThreadCreate(module)
	out := module.String()

	// Should declare _beginthreadex (not CreateThread — CRT init required for TLS)
	if !strings.Contains(out, "@_beginthreadex") {
		t.Error("missing _beginthreadex declaration")
	}
	// Should emit trampoline function
	if !strings.Contains(out, "@__pal_thread_trampoline") {
		t.Error("missing thread trampoline function")
	}
	// Should NOT use pthreads
	if strings.Contains(out, "pthread_create") {
		t.Error("Windows should not use pthread_create")
	}
}

func TestWindowsThreadCreateDetails(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitThreadCreate(module)
	out := module.String()

	// 2MB stack size constant passed to _beginthreadex (0x200000 = 2097152)
	// _beginthreadex takes i32 stack_size (not i64 like CreateThread)
	if !strings.Contains(out, "i32 u0x200000") {
		t.Error("missing 2MB stack size constant (i32 u0x200000) in _beginthreadex call")
	}
	// Allocate 16-byte struct to pack fn+arg for trampoline
	if !strings.Contains(out, "call i8* @pal_alloc(i64 16)") {
		t.Error("missing 16-byte allocation for {fn, arg} struct")
	}
	// Bitcast to {i8*, i8*}* for struct access
	if !strings.Contains(out, "bitcast i8*") {
		t.Error("missing bitcast to struct pointer")
	}
	// GEP to store fn pointer (field 0) and arg (field 1)
	if strings.Count(out, "getelementptr { i8*, i8* }") < 2 {
		t.Error("expected at least 2 GEPs into {i8*, i8*} for fn and arg fields")
	}
	// Store fn and arg into packed struct
	if strings.Count(out, "store i8*") < 2 {
		t.Error("expected at least 2 stores for fn and arg into packed struct")
	}
	// Trampoline function frees the packed struct
	if !strings.Contains(out, "call void @pal_free(i8* %packed)") {
		t.Error("trampoline should free packed struct via pal_free")
	}
	// Trampoline returns i32 0 (DWORD success)
	if !strings.Contains(out, "ret i32 0") {
		t.Error("trampoline should return i32 0")
	}
	// Null thread attributes and null thread ID pointer
	if strings.Count(out, "null") < 2 {
		t.Error("expected null for lpThreadAttributes and lpThreadId")
	}
}

func TestWindowsThreadJoinDeclaresWin32(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitThreadJoin(module)
	out := module.String()

	// WaitForSingleObject declaration
	if !strings.Contains(out, "@WaitForSingleObject(") {
		t.Error("missing @WaitForSingleObject declaration")
	}
	// Call with INFINITE timeout (0xFFFFFFFF = -1 as i32)
	if !strings.Contains(out, "call i32 @WaitForSingleObject(i8* %handle, i32 -1)") {
		t.Error("missing call to WaitForSingleObject with INFINITE timeout")
	}
	// CloseHandle declaration and call
	if !strings.Contains(out, "@CloseHandle(") {
		t.Error("missing @CloseHandle declaration")
	}
	if !strings.Contains(out, "call i32 @CloseHandle(i8* %handle)") {
		t.Error("missing call to CloseHandle")
	}
	// Should NOT use pthreads
	if strings.Contains(out, "pthread_join") {
		t.Error("Windows should not use pthread_join")
	}
	// pal_thread_join body should NOT call pal_free (CloseHandle is sufficient,
	// unlike posix which frees the alloc'd pthread_t). Extract just the join function body.
	joinBody := out[strings.Index(out, "@pal_thread_join"):]
	if strings.Contains(joinBody, "call void @pal_free(") {
		t.Error("Windows thread join should not call pal_free (CloseHandle suffices)")
	}
}

func TestEmitThreadJoin(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitThreadJoin(module)
			out := module.String()

			if !strings.Contains(out, "define void @pal_thread_join(i8* %handle)") {
				t.Error("missing @pal_thread_join definition")
			}
			if fn.Name() != "pal_thread_join" {
				t.Errorf("expected function name pal_thread_join, got %s", fn.Name())
			}
		})
	}
}

func TestPosixThreadJoinDeclaresLibc(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{})
	(&PosixPAL{}).EmitThreadJoin(module)
	out := module.String()

	if !strings.Contains(out, "@pthread_join(") {
		t.Error("missing @pthread_join declaration")
	}
	if !strings.Contains(out, "call i32 @pthread_join(") {
		t.Error("missing call to @pthread_join")
	}
	if !strings.Contains(out, "call void @pal_free(") {
		t.Error("missing call to @pal_free for handle cleanup")
	}
}

func TestEmitMutexInit(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitMutexInit(module)
			out := module.String()

			if !strings.Contains(out, "define i8* @pal_mutex_init()") {
				t.Error("missing @pal_mutex_init definition")
			}
			if !strings.Contains(out, "call i8* @pal_alloc(") {
				t.Error("missing allocation in pal_mutex_init")
			}
			if fn.Name() != "pal_mutex_init" {
				t.Errorf("expected function name pal_mutex_init, got %s", fn.Name())
			}
		})
	}
}

func TestPosixMutexInitDeclaresLibc(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{})
	(&PosixPAL{}).EmitMutexInit(module)
	out := module.String()

	if !strings.Contains(out, "@pthread_mutex_init(") {
		t.Error("missing @pthread_mutex_init declaration")
	}
	if !strings.Contains(out, "call i32 @pthread_mutex_init(") {
		t.Error("missing call to @pthread_mutex_init")
	}
	if !strings.Contains(out, "call i8* @pal_alloc(i64 64)") {
		t.Error("missing 64-byte allocation for pthread_mutex_t")
	}
}

func TestWindowsMutexInitDeclaresWin32(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitMutexInit(module)
	out := module.String()

	// 40-byte allocation for CRITICAL_SECTION
	if !strings.Contains(out, "call i8* @pal_alloc(i64 40)") {
		t.Error("missing 40-byte allocation for CRITICAL_SECTION")
	}
	// InitializeCriticalSection declaration and call
	if !strings.Contains(out, "@InitializeCriticalSection(") {
		t.Error("missing @InitializeCriticalSection declaration")
	}
	if !strings.Contains(out, "call void @InitializeCriticalSection(") {
		t.Error("missing call to @InitializeCriticalSection")
	}
	// Should NOT use pthreads
	if strings.Contains(out, "pthread_mutex_init") {
		t.Error("Windows should not use pthread_mutex_init")
	}
}

func TestPosixMutexLockUnlockDestroyDeclaresLibc(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{})
	(&PosixPAL{}).EmitMutexLock(module)
	(&PosixPAL{}).EmitMutexUnlock(module)
	(&PosixPAL{}).EmitMutexDestroy(module)
	out := module.String()

	if !strings.Contains(out, "@pthread_mutex_lock(") {
		t.Error("missing @pthread_mutex_lock declaration")
	}
	if !strings.Contains(out, "call i32 @pthread_mutex_lock(") {
		t.Error("missing call to @pthread_mutex_lock")
	}
	if !strings.Contains(out, "@pthread_mutex_unlock(") {
		t.Error("missing @pthread_mutex_unlock declaration")
	}
	if !strings.Contains(out, "call i32 @pthread_mutex_unlock(") {
		t.Error("missing call to @pthread_mutex_unlock")
	}
	if !strings.Contains(out, "@pthread_mutex_destroy(") {
		t.Error("missing @pthread_mutex_destroy declaration")
	}
	if !strings.Contains(out, "call i32 @pthread_mutex_destroy(") {
		t.Error("missing call to @pthread_mutex_destroy")
	}
	if !strings.Contains(out, "call void @pal_free(") {
		t.Error("missing call to @pal_free in pal_mutex_destroy")
	}
}

func TestWindowsMutexLockUnlockDestroyDeclaresWin32(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitMutexLock(module)
	(&WindowsPAL{}).EmitMutexUnlock(module)
	(&WindowsPAL{}).EmitMutexDestroy(module)
	out := module.String()

	// EnterCriticalSection for lock
	if !strings.Contains(out, "@EnterCriticalSection(") {
		t.Error("missing @EnterCriticalSection declaration")
	}
	if !strings.Contains(out, "call void @EnterCriticalSection(") {
		t.Error("missing call to @EnterCriticalSection")
	}
	// LeaveCriticalSection for unlock
	if !strings.Contains(out, "@LeaveCriticalSection(") {
		t.Error("missing @LeaveCriticalSection declaration")
	}
	if !strings.Contains(out, "call void @LeaveCriticalSection(") {
		t.Error("missing call to @LeaveCriticalSection")
	}
	// DeleteCriticalSection for destroy
	if !strings.Contains(out, "@DeleteCriticalSection(") {
		t.Error("missing @DeleteCriticalSection declaration")
	}
	if !strings.Contains(out, "call void @DeleteCriticalSection(") {
		t.Error("missing call to @DeleteCriticalSection")
	}
	// Free after delete
	if !strings.Contains(out, "call void @pal_free(") {
		t.Error("missing call to @pal_free in pal_mutex_destroy")
	}
	// Should NOT use pthreads
	if strings.Contains(out, "pthread_mutex") {
		t.Error("Windows should not use pthread_mutex functions")
	}
}

func TestEmitMutexLockUnlock(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			lockFn := tc.pal.EmitMutexLock(module)
			unlockFn := tc.pal.EmitMutexUnlock(module)
			out := module.String()

			if !strings.Contains(out, "define void @pal_mutex_lock(i8* %mutex)") {
				t.Error("missing @pal_mutex_lock definition")
			}
			if !strings.Contains(out, "define void @pal_mutex_unlock(i8* %mutex)") {
				t.Error("missing @pal_mutex_unlock definition")
			}
			if lockFn.Name() != "pal_mutex_lock" {
				t.Errorf("expected pal_mutex_lock, got %s", lockFn.Name())
			}
			if unlockFn.Name() != "pal_mutex_unlock" {
				t.Errorf("expected pal_mutex_unlock, got %s", unlockFn.Name())
			}
		})
	}
}

func TestEmitMutexDestroy(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitMutexDestroy(module)
			out := module.String()

			if !strings.Contains(out, "define void @pal_mutex_destroy(i8* %mutex)") {
				t.Error("missing @pal_mutex_destroy definition")
			}
			if !strings.Contains(out, "call void @pal_free(") {
				t.Error("missing call to @pal_free in pal_mutex_destroy")
			}
			if fn.Name() != "pal_mutex_destroy" {
				t.Errorf("expected pal_mutex_destroy, got %s", fn.Name())
			}
		})
	}
}

func TestEmitCondInit(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitCondInit(module)
			out := module.String()

			if !strings.Contains(out, "define i8* @pal_cond_init()") {
				t.Error("missing @pal_cond_init definition")
			}
			if !strings.Contains(out, "call i8* @pal_alloc(") {
				t.Error("missing allocation in pal_cond_init")
			}
			if fn.Name() != "pal_cond_init" {
				t.Errorf("expected pal_cond_init, got %s", fn.Name())
			}
		})
	}
}

func TestEmitCondWaitSignal(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			waitFn := tc.pal.EmitCondWait(module)
			signalFn := tc.pal.EmitCondSignal(module)
			out := module.String()

			if !strings.Contains(out, "define void @pal_cond_wait(i8* %cond, i8* %mutex)") {
				t.Error("missing @pal_cond_wait definition")
			}
			if !strings.Contains(out, "define void @pal_cond_signal(i8* %cond)") {
				t.Error("missing @pal_cond_signal definition")
			}
			if waitFn.Name() != "pal_cond_wait" {
				t.Errorf("expected pal_cond_wait, got %s", waitFn.Name())
			}
			if signalFn.Name() != "pal_cond_signal" {
				t.Errorf("expected pal_cond_signal, got %s", signalFn.Name())
			}
		})
	}
}

func TestEmitCondDestroy(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitCondDestroy(module)
			out := module.String()

			if !strings.Contains(out, "define void @pal_cond_destroy(i8* %cond)") {
				t.Error("missing @pal_cond_destroy definition")
			}
			if !strings.Contains(out, "call void @pal_free(") {
				t.Error("missing call to @pal_free in pal_cond_destroy")
			}
			if fn.Name() != "pal_cond_destroy" {
				t.Errorf("expected pal_cond_destroy, got %s", fn.Name())
			}
		})
	}
}

func TestEmitCondBroadcast(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitCondBroadcast(module)
			out := module.String()

			if !strings.Contains(out, "define void @pal_cond_broadcast(i8* %cond)") {
				t.Error("missing @pal_cond_broadcast definition")
			}
			if fn.Name() != "pal_cond_broadcast" {
				t.Errorf("expected pal_cond_broadcast, got %s", fn.Name())
			}
		})
	}
}

func TestWindowsCondInitDeclaresWin32(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitCondInit(module)
	out := module.String()

	// 8-byte allocation for CONDITION_VARIABLE
	if !strings.Contains(out, "call i8* @pal_alloc(i64 8)") {
		t.Error("missing 8-byte allocation for CONDITION_VARIABLE")
	}
	// InitializeConditionVariable declaration and call
	if !strings.Contains(out, "@InitializeConditionVariable(") {
		t.Error("missing @InitializeConditionVariable declaration")
	}
	if !strings.Contains(out, "call void @InitializeConditionVariable(") {
		t.Error("missing call to @InitializeConditionVariable")
	}
	// Should NOT use pthreads
	if strings.Contains(out, "pthread_cond_init") {
		t.Error("Windows should not use pthread_cond_init")
	}
}

func TestWindowsCondWaitSignalDeclaresWin32(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitCondWait(module)
	(&WindowsPAL{}).EmitCondSignal(module)
	out := module.String()

	// SleepConditionVariableCS declaration and call with INFINITE
	if !strings.Contains(out, "@SleepConditionVariableCS(") {
		t.Error("missing @SleepConditionVariableCS declaration")
	}
	if !strings.Contains(out, "call i32 @SleepConditionVariableCS(i8* %cond, i8* %mutex, i32 -1)") {
		t.Error("missing call to SleepConditionVariableCS with INFINITE timeout")
	}
	// WakeConditionVariable declaration and call
	if !strings.Contains(out, "@WakeConditionVariable(") {
		t.Error("missing @WakeConditionVariable declaration")
	}
	if !strings.Contains(out, "call void @WakeConditionVariable(") {
		t.Error("missing call to @WakeConditionVariable")
	}
	// Should NOT use pthreads
	if strings.Contains(out, "pthread_cond") {
		t.Error("Windows should not use pthread_cond functions")
	}
}

func TestWindowsCondDestroyJustFrees(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitCondDestroy(module)
	out := module.String()

	// Should call pal_free (no Windows API destroy for CONDITION_VARIABLE)
	if !strings.Contains(out, "call void @pal_free(") {
		t.Error("missing call to @pal_free in pal_cond_destroy")
	}
	// Should NOT declare any DeleteConditionVariable (doesn't exist in Win32)
	if strings.Contains(out, "DeleteConditionVariable") {
		t.Error("Windows has no DeleteConditionVariable API")
	}
	// Should NOT use pthreads
	if strings.Contains(out, "pthread_cond_destroy") {
		t.Error("Windows should not use pthread_cond_destroy")
	}
}

func TestPosixCondBroadcastDeclaresLibc(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{})
	(&PosixPAL{}).EmitCondBroadcast(module)
	out := module.String()

	if !strings.Contains(out, "@pthread_cond_broadcast(") {
		t.Error("missing @pthread_cond_broadcast declaration")
	}
	if !strings.Contains(out, "call i32 @pthread_cond_broadcast(") {
		t.Error("missing call to @pthread_cond_broadcast")
	}
}

func TestStubCondBroadcastIsNoOp(t *testing.T) {
	// Only WASM uses stub cond_broadcast (no-op)
	module := newModuleWithAlloc(&WasmPAL{})
	(&WasmPAL{}).EmitCondBroadcast(module)
	out := module.String()

	if strings.Contains(out, "pthread_cond_broadcast") {
		t.Error("stub should not declare pthread_cond_broadcast")
	}
	if !strings.Contains(out, "ret void") {
		t.Error("stub should return void (no-op)")
	}
}

func TestWindowsCondBroadcastUsesWakeAll(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitCondBroadcast(module)
	out := module.String()

	if !strings.Contains(out, "@WakeAllConditionVariable") {
		t.Error("Windows pal_cond_broadcast should use WakeAllConditionVariable")
	}
	if strings.Contains(out, "pthread_cond_broadcast") {
		t.Error("Windows should not use pthread_cond_broadcast")
	}
}

func TestPosixCondDeclaresLibc(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{})
	(&PosixPAL{}).EmitCondInit(module)
	(&PosixPAL{}).EmitCondWait(module)
	(&PosixPAL{}).EmitCondSignal(module)
	(&PosixPAL{}).EmitCondDestroy(module)
	out := module.String()

	if !strings.Contains(out, "@pthread_cond_init(") {
		t.Error("missing @pthread_cond_init declaration")
	}
	if !strings.Contains(out, "@pthread_cond_wait(") {
		t.Error("missing @pthread_cond_wait declaration")
	}
	if !strings.Contains(out, "@pthread_cond_signal(") {
		t.Error("missing @pthread_cond_signal declaration")
	}
	if !strings.Contains(out, "@pthread_cond_destroy(") {
		t.Error("missing @pthread_cond_destroy declaration")
	}
	if !strings.Contains(out, "call i8* @pal_alloc(i64 64)") {
		t.Error("missing 64-byte allocation for pthread_cond_t")
	}
}

// --- NumCPUs tests (Phase 5c) ---

func TestEmitNumCPUs(t *testing.T) {
	tests := []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{target: "arm64-apple-macosx11.0.0"}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			emitLibcAlloc(module) // needed by stub mutex init
			fn := tt.pal.EmitNumCPUs(module)
			if fn.Name() != "pal_num_cpus" {
				t.Errorf("expected pal_num_cpus, got %s", fn.Name())
			}
			out := module.String()
			if !strings.Contains(out, "define i32 @pal_num_cpus()") {
				t.Error("missing @pal_num_cpus definition")
			}
		})
	}
}

func TestPosixNumCPUsDeclaresLibc(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "arm64-apple-macosx11.0.0"}
	p.EmitNumCPUs(module)

	out := module.String()
	if !strings.Contains(out, "@sysconf(") {
		t.Error("missing @sysconf declaration")
	}
	// macOS uses _SC_NPROCESSORS_ONLN = 58
	if !strings.Contains(out, "call i64 @sysconf(i32 58)") {
		t.Error("missing sysconf call with macOS _SC_NPROCESSORS_ONLN (58)")
	}
}

func TestPosixNumCPUsLinuxConstant(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "x86_64-unknown-linux-gnu"}
	p.EmitNumCPUs(module)

	out := module.String()
	// Linux uses _SC_NPROCESSORS_ONLN = 84
	if !strings.Contains(out, "call i64 @sysconf(i32 84)") {
		t.Error("missing sysconf call with Linux _SC_NPROCESSORS_ONLN (84)")
	}
}

func TestStubNumCPUsReturnsOne(t *testing.T) {
	// Only WASM uses stub num_cpus (returns 1)
	module := ir.NewModule()
	(&WasmPAL{}).EmitNumCPUs(module)
	out := module.String()
	if !strings.Contains(out, "ret i32 1") {
		t.Error("stub pal_num_cpus should return 1")
	}
}

func TestWindowsNumCPUsUsesGetSystemInfo(t *testing.T) {
	module := ir.NewModule()
	(&WindowsPAL{}).EmitNumCPUs(module)
	out := module.String()
	if !strings.Contains(out, "@GetSystemInfo") {
		t.Error("Windows pal_num_cpus should use GetSystemInfo")
	}
	// Should NOT return a hardcoded 1
	if strings.Contains(out, "ret i32 1") {
		t.Error("Windows pal_num_cpus should not return hardcoded 1")
	}
}

func TestWindowsNumCPUsStructDetails(t *testing.T) {
	module := ir.NewModule()
	(&WindowsPAL{}).EmitNumCPUs(module)
	out := module.String()

	// 48-byte stack alloca for SYSTEM_INFO struct
	if !strings.Contains(out, "alloca [48 x i8]") {
		t.Error("missing 48-byte alloca for SYSTEM_INFO struct")
	}
	// GEP to byte offset 32 for dwNumberOfProcessors
	if !strings.Contains(out, "getelementptr i8, i8*") {
		t.Error("missing GEP to access dwNumberOfProcessors field")
	}
	if !strings.Contains(out, "i64 32") {
		t.Error("missing offset 32 for dwNumberOfProcessors in SYSTEM_INFO")
	}
	// Bitcast to i32* for loading the DWORD field
	if !strings.Contains(out, "bitcast i8*") {
		t.Error("missing bitcast to i32* for dwNumberOfProcessors")
	}
	// Load i32 from the field
	if !strings.Contains(out, "load i32, i32*") {
		t.Error("missing load i32 from dwNumberOfProcessors")
	}
	// Select for clamping to at least 1
	if !strings.Contains(out, "select i1") {
		t.Error("missing select for clamping numCPUs to >= 1")
	}
	// icmp for the clamp check
	if !strings.Contains(out, "icmp slt") {
		t.Error("missing icmp slt for clamp comparison")
	}
}

// --- File I/O PAL Tests (Phase D) ---

// assertContains is a test helper that checks for a substring in the IR output.
func assertContains(t *testing.T, out, substr, msg string) {
	t.Helper()
	if !strings.Contains(out, substr) {
		t.Errorf("%s: missing %q", msg, substr)
	}
}

func TestFileOpenPosix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		// mode 2 (create) flags: O_RDWR|O_CREAT|O_TRUNC
		createFlags string
	}{
		{"Linux", "x86_64-unknown-linux-gnu", "i32 578"},  // 2|0x40|0x200
		{"macOS", "arm64-apple-darwin23.0.0", "i32 1538"}, // 2|0x200|0x400
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			p := &PosixPAL{target: tc.target}
			fn := p.EmitFileOpen(module)
			out := module.String()

			if fn.Name() != "pal_file_open" {
				t.Errorf("expected pal_file_open, got %s", fn.Name())
			}
			assertContains(t, out, "@open(i8*", "open declaration")
			assertContains(t, out, "define i32 @pal_file_open(i8* %path, i32 %mode)", "pal_file_open definition")
			// Mode-to-flags select chain
			assertContains(t, out, "icmp eq i32 %mode, 1", "read mode check")
			assertContains(t, out, "icmp eq i32 %mode, 2", "create mode check")
			assertContains(t, out, "icmp eq i32 %mode, 3", "append mode check")
			// Platform-specific create flags
			assertContains(t, out, tc.createFlags, "platform-specific create flags")
			// Permission mode 0644 = 420
			assertContains(t, out, "i32 420", "0644 permission mode")
		})
	}
}

func TestFileOpenWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitFileOpen(module)
	out := module.String()

	if fn.Name() != "pal_file_open" {
		t.Errorf("expected pal_file_open, got %s", fn.Name())
	}
	assertContains(t, out, "@_open(i8*", "_open declaration")
	assertContains(t, out, "define i32 @pal_file_open(", "pal_file_open definition")
	// _O_BINARY=0x8000 flag must be included (llir renders as u0x8000)
	assertContains(t, out, "0x8000", "_O_BINARY flag in mode mapping")
	// _open called with permission mode argument
	assertContains(t, out, "call i32 @_open(", "_open called")
}

func TestFileOpenWasm(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}
	fn := p.EmitFileOpen(module)
	out := module.String()

	if fn.Name() != "pal_file_open" {
		t.Errorf("expected pal_file_open, got %s", fn.Name())
	}
	assertContains(t, out, "ret i32 -1", "WASM stub returns -1")
}

func TestFileReadAllPlatforms(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
		decl string // expected libc function declaration
	}{
		{"POSIX", &PosixPAL{}, "@read("},
		{"Windows", &WindowsPAL{}, "@_read("},
		{"WASM", &WasmPAL{}, ""},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitFileRead(module)
			out := module.String()

			if fn.Name() != "pal_file_read" {
				t.Errorf("expected pal_file_read, got %s", fn.Name())
			}
			assertContains(t, out, "define i64 @pal_file_read(i32 %fd, i8* %buf, i64 %len)", "definition")
			if tc.decl != "" {
				assertContains(t, out, tc.decl, "libc declaration")
			}
		})
	}
}

func TestFileWriteAllPlatforms(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
		decl string
	}{
		{"POSIX", &PosixPAL{}, "@write("},
		{"Windows", &WindowsPAL{}, "@_write("},
		{"WASM", &WasmPAL{}, ""},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitFileWrite(module)
			out := module.String()

			if fn.Name() != "pal_file_write" {
				t.Errorf("expected pal_file_write, got %s", fn.Name())
			}
			assertContains(t, out, "define i64 @pal_file_write(i32 %fd, i8* %buf, i64 %len)", "definition")
			if tc.decl != "" {
				assertContains(t, out, tc.decl, "libc declaration")
			}
		})
	}
}

func TestFileCloseAllPlatforms(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
		decl string
	}{
		{"POSIX", &PosixPAL{}, "@close("},
		{"Windows", &WindowsPAL{}, "@_close("},
		{"WASM", &WasmPAL{}, ""},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitFileClose(module)
			out := module.String()

			if fn.Name() != "pal_file_close" {
				t.Errorf("expected pal_file_close, got %s", fn.Name())
			}
			assertContains(t, out, "define i32 @pal_file_close(i32 %fd)", "definition")
			if tc.decl != "" {
				assertContains(t, out, tc.decl, "libc declaration")
			}
		})
	}
}

func TestFileSeekAllPlatforms(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
		decl string
	}{
		{"POSIX", &PosixPAL{}, "@lseek("},
		{"Windows", &WindowsPAL{}, "@_lseeki64("},
		{"WASM", &WasmPAL{}, ""},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitFileSeek(module)
			out := module.String()

			if fn.Name() != "pal_file_seek" {
				t.Errorf("expected pal_file_seek, got %s", fn.Name())
			}
			assertContains(t, out, "define i64 @pal_file_seek(i32 %fd, i64 %offset, i32 %whence)", "definition")
			if tc.decl != "" {
				assertContains(t, out, tc.decl, "libc declaration")
			}
		})
	}
}

func TestFileStatSizePosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitFileOpen(module)
	p.EmitFileClose(module)
	p.EmitFileSeek(module)
	fn := p.EmitFileStatSize(module)
	out := module.String()

	if fn.Name() != "pal_file_stat_size" {
		t.Errorf("expected pal_file_stat_size, got %s", fn.Name())
	}
	assertContains(t, out, "define i64 @pal_file_stat_size(i8* %path)", "definition")
	// Uses open+lseek+close pattern (open is variadic)
	assertContains(t, out, "@open(", "calls open")
	assertContains(t, out, "call i64 @lseek(", "calls lseek for SEEK_END")
	assertContains(t, out, "call i32 @close(", "calls close")
	// SEEK_END = 2
	assertContains(t, out, "i32 2)", "SEEK_END constant")
	// Failure branch returns -errno (via __errno_location)
	assertContains(t, out, "__errno_location", "reads errno on failure")
}

func TestFileStatSizeWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	p.EmitFileOpen(module)
	p.EmitFileClose(module)
	p.EmitFileSeek(module)
	fn := p.EmitFileStatSize(module)
	out := module.String()

	if fn.Name() != "pal_file_stat_size" {
		t.Errorf("expected pal_file_stat_size, got %s", fn.Name())
	}
	assertContains(t, out, "call i32 @_open(", "calls _open")
	assertContains(t, out, "call i64 @_lseeki64(", "calls _lseeki64")
	assertContains(t, out, "call i32 @_close(", "calls _close")
}

// B0027: EmitFileStatSize must work without prior EmitFileOpen/Close/Seek.
func TestFileStatSizeOrderIndependent(t *testing.T) {
	t.Run("POSIX", func(t *testing.T) {
		module := ir.NewModule()
		p := &PosixPAL{}
		// Only EmitErrno needed (for errno_location) — no file open/close/seek
		p.EmitErrno(module)
		fn := p.EmitFileStatSize(module)
		out := module.String()

		if fn.Name() != "pal_file_stat_size" {
			t.Errorf("expected pal_file_stat_size, got %s", fn.Name())
		}
		assertContains(t, out, "@open(", "declares open via getOrDeclareFunc")
		assertContains(t, out, "@close(", "declares close via getOrDeclareFunc")
		assertContains(t, out, "@lseek(", "declares lseek via getOrDeclareFunc")
	})
	t.Run("Windows", func(t *testing.T) {
		module := ir.NewModule()
		p := &WindowsPAL{}
		p.EmitErrno(module)
		fn := p.EmitFileStatSize(module)
		out := module.String()

		if fn.Name() != "pal_file_stat_size" {
			t.Errorf("expected pal_file_stat_size, got %s", fn.Name())
		}
		assertContains(t, out, "@_open(", "declares _open via getOrDeclareFunc")
		assertContains(t, out, "@_lseeki64(", "declares _lseeki64 via getOrDeclareFunc")
		assertContains(t, out, "@_close(", "declares _close via getOrDeclareFunc")
	})
}

func TestFileStatPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitErrno(module)
	fn := p.EmitFileStat(module)
	out := module.String()

	if fn.Name() != "pal_file_stat" {
		t.Errorf("expected pal_file_stat, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_file_stat(i8* %path, i64* %out, i32 %follow)", "definition")
	assertContains(t, out, "@stat(", "calls stat")
	assertContains(t, out, "@lstat(", "calls lstat")
	// Branches on follow flag
	assertContains(t, out, "icmp ne i32 %follow", "branches on follow flag")
	// Errno on failure
	assertContains(t, out, "__errno_location", "reads errno on failure")
	// Extracts fields from struct stat (GEP into i8* buffer)
	assertContains(t, out, "getelementptr i8, i8*", "extracts struct stat fields")
	// Permission bits mask: 0o7777 = 4095
	assertContains(t, out, "and i64", "masks permission bits")
	// File type detection: select-chain for reg/dir/lnk/other
	assertContains(t, out, "select i1", "file type select chain")
}

func TestFileStatWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	p.EmitErrno(module)
	fn := p.EmitFileStat(module)
	out := module.String()

	if fn.Name() != "pal_file_stat" {
		t.Errorf("expected pal_file_stat, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_file_stat(", "definition")
	assertContains(t, out, "@_stat64(", "calls _stat64")
}

func TestFileStatWasm(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}
	fn := p.EmitFileStat(module)
	out := module.String()

	if fn.Name() != "pal_file_stat" {
		t.Errorf("expected pal_file_stat, got %s", fn.Name())
	}
	// Stub returns -1
	assertContains(t, out, "ret i32 -1", "stub returns -1")
	_ = out
}

func TestFileRemoveAllPlatforms(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
		decl string
	}{
		{"POSIX", &PosixPAL{}, "@unlink("},
		{"Windows", &WindowsPAL{}, "@_unlink("},
		{"WASM", &WasmPAL{}, ""},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitFileRemove(module)
			out := module.String()

			if fn.Name() != "pal_file_remove" {
				t.Errorf("expected pal_file_remove, got %s", fn.Name())
			}
			assertContains(t, out, "define i32 @pal_file_remove(i8* %path)", "definition")
			if tc.decl != "" {
				assertContains(t, out, tc.decl, "libc declaration")
			}
		})
	}
}

func TestFileExistsPosix(t *testing.T) {
	module := ir.NewModule()
	fn := (&PosixPAL{}).EmitFileExists(module)
	out := module.String()

	if fn.Name() != "pal_file_exists" {
		t.Errorf("expected pal_file_exists, got %s", fn.Name())
	}
	assertContains(t, out, "@access(", "access declaration")
	// F_OK = 0
	assertContains(t, out, "call i32 @access(i8* %path, i32 0)", "access(path, F_OK)")
	// Returns 1 for exists, 0 for not found
	assertContains(t, out, "select i1", "select for return value mapping")
}

func TestFileExistsWindows(t *testing.T) {
	module := ir.NewModule()
	fn := (&WindowsPAL{}).EmitFileExists(module)
	out := module.String()

	if fn.Name() != "pal_file_exists" {
		t.Errorf("expected pal_file_exists, got %s", fn.Name())
	}
	assertContains(t, out, "@_access(", "_access declaration")
}

func TestFileExistsWasm(t *testing.T) {
	module := ir.NewModule()
	fn := (&WasmPAL{}).EmitFileExists(module)
	out := module.String()

	if fn.Name() != "pal_file_exists" {
		t.Errorf("expected pal_file_exists, got %s", fn.Name())
	}
	assertContains(t, out, "ret i32 0", "WASM stub returns 0")
}

func TestFileMkdirPosix(t *testing.T) {
	module := ir.NewModule()
	fn := (&PosixPAL{}).EmitFileMkdir(module)
	out := module.String()

	if fn.Name() != "pal_file_mkdir" {
		t.Errorf("expected pal_file_mkdir, got %s", fn.Name())
	}
	assertContains(t, out, "@mkdir(", "mkdir declaration")
	// 0755 = 493
	assertContains(t, out, "i32 493", "0755 mode")
}

func TestFileMkdirWindows(t *testing.T) {
	module := ir.NewModule()
	fn := (&WindowsPAL{}).EmitFileMkdir(module)
	out := module.String()

	if fn.Name() != "pal_file_mkdir" {
		t.Errorf("expected pal_file_mkdir, got %s", fn.Name())
	}
	assertContains(t, out, "@_mkdir(", "_mkdir declaration")
}

func TestDirRemoveAllPlatforms(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
		decl string
	}{
		{"POSIX", &PosixPAL{}, "@rmdir("},
		{"Windows", &WindowsPAL{}, "@_rmdir("},
		{"WASM", &WasmPAL{}, ""},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitDirRemove(module)
			out := module.String()

			if fn.Name() != "pal_dir_remove" {
				t.Errorf("expected pal_dir_remove, got %s", fn.Name())
			}
			assertContains(t, out, "define i32 @pal_dir_remove(i8* %path)", "definition")
			if tc.decl != "" {
				assertContains(t, out, tc.decl, "libc declaration")
			}
		})
	}
}

func TestDirExistsPosix(t *testing.T) {
	module := ir.NewModule()
	fn := (&PosixPAL{}).EmitDirExists(module)
	out := module.String()

	if fn.Name() != "pal_dir_exists" {
		t.Errorf("expected pal_dir_exists, got %s", fn.Name())
	}
	assertContains(t, out, "@opendir(", "opendir declaration")
	assertContains(t, out, "@closedir(", "closedir declaration")
	// Checks for null (directory doesn't exist)
	assertContains(t, out, "icmp eq", "null check for opendir result")
	assertContains(t, out, "ret i32 1", "returns 1 for exists")
	assertContains(t, out, "ret i32 0", "returns 0 for not found")
}

func TestDirExistsWindows(t *testing.T) {
	module := ir.NewModule()
	fn := (&WindowsPAL{}).EmitDirExists(module)
	out := module.String()

	if fn.Name() != "pal_dir_exists" {
		t.Errorf("expected pal_dir_exists, got %s", fn.Name())
	}
	assertContains(t, out, "@GetFileAttributesA(", "GetFileAttributesA declaration")
	// FILE_ATTRIBUTE_DIRECTORY = 0x10 = 16 (used in and instruction)
	assertContains(t, out, ", 16", "FILE_ATTRIBUTE_DIRECTORY")
}

func TestDirExistsWasm(t *testing.T) {
	module := ir.NewModule()
	fn := (&WasmPAL{}).EmitDirExists(module)
	out := module.String()

	if fn.Name() != "pal_dir_exists" {
		t.Errorf("expected pal_dir_exists, got %s", fn.Name())
	}
	assertContains(t, out, "ret i32 0", "WASM stub returns 0")
}

func TestErrnoPosix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		fnName string
	}{
		{"Linux", "x86_64-unknown-linux-gnu", "__errno_location"},
		{"macOS", "arm64-apple-darwin23.0.0", "__error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := (&PosixPAL{target: tc.target}).EmitErrno(module)
			out := module.String()

			if fn.Name() != "pal_errno" {
				t.Errorf("expected pal_errno, got %s", fn.Name())
			}
			assertContains(t, out, "@"+tc.fnName+"()", "errno location function")
			assertContains(t, out, "load i32, i32*", "loads errno value from pointer")
		})
	}
}

func TestErrnoWindows(t *testing.T) {
	module := ir.NewModule()
	fn := (&WindowsPAL{}).EmitErrno(module)
	out := module.String()

	if fn.Name() != "pal_errno" {
		t.Errorf("expected pal_errno, got %s", fn.Name())
	}
	assertContains(t, out, "@_errno()", "_errno declaration")
	assertContains(t, out, "load i32, i32*", "loads errno value from pointer")
}

func TestErrnoWasm(t *testing.T) {
	module := ir.NewModule()
	fn := (&WasmPAL{}).EmitErrno(module)
	out := module.String()

	if fn.Name() != "pal_errno" {
		t.Errorf("expected pal_errno, got %s", fn.Name())
	}
	assertContains(t, out, "ret i32 0", "WASM stub returns 0")
}

func TestForTargetFileIOInterface(t *testing.T) {
	// Verify that all PAL implementations satisfy the interface with file I/O methods
	triples := []string{
		"x86_64-unknown-linux-gnu",
		"arm64-apple-darwin23.0.0",
		"x86_64-pc-windows-msvc",
		"wasm32-unknown-wasi",
	}
	for _, triple := range triples {
		t.Run(triple, func(t *testing.T) {
			p := ForTarget(triple)
			module := ir.NewModule()
			// Must be able to call all file I/O methods without panic
			p.EmitAlloc(module)
			p.EmitFree(module)
			p.EmitFileOpen(module)
			p.EmitFileRead(module)
			p.EmitFileWrite(module)
			p.EmitFileClose(module)
			p.EmitFileSeek(module)
			p.EmitFileStatSize(module)
			p.EmitFileStat(module)
			p.EmitFileRemove(module)
			p.EmitFileExists(module)
			p.EmitFileMkdir(module)
			p.EmitDirRemove(module)
			p.EmitDirExists(module)
			p.EmitErrno(module)
		})
	}
}

func TestWindowsFileReadWriteTruncation(t *testing.T) {
	// Windows UCRT _read/_write take i32 count, PAL takes i64 — verify truncation + sign extension
	module := ir.NewModule()
	p := &WindowsPAL{}
	p.EmitFileRead(module)
	out := module.String()

	assertContains(t, out, "trunc i64 %len to i32", "truncates i64 len to i32")
	assertContains(t, out, "sext i32", "sign-extends i32 result to i64")
}

func TestPosixFileWriteReusesWriteDecl(t *testing.T) {
	// When EmitWrite was already called, EmitFileWrite should reuse the @write declaration
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitWrite(module)
	p.EmitFileWrite(module)
	out := module.String()

	// Should have exactly one @write declaration (not two)
	count := strings.Count(out, "declare i64 @write(")
	if count != 1 {
		t.Errorf("expected 1 @write declaration, got %d", count)
	}
}

// --- B0010: Stack overflow detection tests ---

func TestPosixGuardPageInThreadCreate(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{})
	(&PosixPAL{}).EmitThreadCreate(module)
	out := module.String()

	if !strings.Contains(out, "call i32 @pthread_attr_setguardsize(") {
		t.Error("missing call to @pthread_attr_setguardsize")
	}
	// 4096 = 0x1000; LLVM may render as decimal or hex
	if !strings.Contains(out, "i64 4096") && !strings.Contains(out, "i64 u0x1000") {
		t.Error("missing 4096-byte guard page size constant")
	}
}

func TestEmitStackOverflowInit(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix/Linux", &PosixPAL{target: "x86_64-unknown-linux-gnu"}},
		{"Posix/macOS", &PosixPAL{target: "arm64-apple-darwin23.0.0"}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitWrite(module)
			fn := tc.pal.EmitStackOverflowInit(module)
			out := module.String()

			if fn.Name() != "pal_stack_overflow_init" {
				t.Errorf("expected function name pal_stack_overflow_init, got %s", fn.Name())
			}
			if !strings.Contains(out, "define void @pal_stack_overflow_init()") {
				t.Error("missing @pal_stack_overflow_init definition")
			}
		})
	}
}

func TestWindowsStackOverflowUsesVEH(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitWrite(module)
	(&WindowsPAL{}).EmitExit(module)
	(&WindowsPAL{}).EmitStackOverflowInit(module)
	out := module.String()

	// Should register a VEH handler
	if !strings.Contains(out, "@AddVectoredExceptionHandler") {
		t.Error("missing AddVectoredExceptionHandler declaration")
	}
	// Should define the VEH handler function
	if !strings.Contains(out, "@__promise_veh_handler") {
		t.Error("missing VEH handler function")
	}
	// Handler should check STATUS_STACK_OVERFLOW (0xC00000FD = 3221225725)
	// LLVM IR may render as unsigned hex or signed decimal depending on the library
	if !strings.Contains(out, "3221225725") && !strings.Contains(out, "0xC00000FD") && !strings.Contains(out, "-1073741571") {
		t.Error("missing STATUS_STACK_OVERFLOW constant (0xC00000FD)")
	}
	// Handler should call ExitProcess
	if !strings.Contains(out, "@ExitProcess") {
		t.Error("missing ExitProcess call in handler")
	}
	// Handler should write error message
	if !strings.Contains(out, "fatal: stack overflow") {
		t.Error("missing 'fatal: stack overflow' message")
	}
	// Should NOT use pthreads or signal
	if strings.Contains(out, "sigaction") || strings.Contains(out, "signal(") {
		t.Error("Windows should not use POSIX signal handling")
	}
}

func TestEmitStackOverflowThreadInit(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Posix/Linux", &PosixPAL{target: "x86_64-unknown-linux-gnu"}},
		{"Posix/macOS", &PosixPAL{target: "arm64-apple-darwin23.0.0"}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitStackOverflowThreadInit(module)
			out := module.String()

			if fn.Name() != "pal_stack_overflow_thread_init" {
				t.Errorf("expected function name pal_stack_overflow_thread_init, got %s", fn.Name())
			}
			if !strings.Contains(out, "define void @pal_stack_overflow_thread_init()") {
				t.Error("missing @pal_stack_overflow_thread_init definition")
			}
		})
	}
}

func TestDarwinStackOverflowUseSigaction(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{target: "arm64-apple-darwin23.0.0"})
	(&PosixPAL{target: "arm64-apple-darwin23.0.0"}).EmitWrite(module)
	(&PosixPAL{target: "arm64-apple-darwin23.0.0"}).EmitStackOverflowInit(module)
	out := module.String()

	// macOS must use sigaction + sigaltstack, NOT signal()
	if !strings.Contains(out, "call i32 @sigaction(") {
		t.Error("macOS should use sigaction, not signal")
	}
	if !strings.Contains(out, "call i32 @sigaltstack(") {
		t.Error("macOS should set up sigaltstack")
	}
	// SA_ONSTACK(0x0001) | SA_RESETHAND(0x0004) = 0x0005
	if !strings.Contains(out, "i32 5") {
		t.Error("missing SA_ONSTACK|SA_RESETHAND flags (0x0005)")
	}
	// Should register for both SIGSEGV(11) and SIGBUS(10)
	if strings.Count(out, "call i32 @sigaction(i32 11,") < 1 {
		t.Error("missing sigaction for SIGSEGV (signal 11)")
	}
	if strings.Count(out, "call i32 @sigaction(i32 10,") < 1 {
		t.Error("missing sigaction for SIGBUS (signal 10)")
	}
	// Should NOT use signal() on macOS
	if strings.Contains(out, "call i8* @signal(") {
		t.Error("macOS should not use signal() — must use sigaction with SA_ONSTACK")
	}
}

func TestDarwinThreadInitSetsSigaltstack(t *testing.T) {
	p := &PosixPAL{target: "arm64-apple-darwin23.0.0"}
	module := newModuleWithAlloc(p)
	p.EmitStackOverflowThreadInit(module)
	out := module.String()

	if !strings.Contains(out, "call i32 @sigaltstack(") {
		t.Error("macOS thread init should call sigaltstack")
	}
	// Allocate 65536 bytes for alternate stack
	if !strings.Contains(out, "call i8* @pal_alloc(i64 65536)") {
		t.Error("missing 64KB alternate stack allocation")
	}
}

func TestLinuxStackOverflowUsesSigaction(t *testing.T) {
	// B0128: Linux now uses sigaction(SA_SIGINFO) instead of signal()
	module := newModuleWithAlloc(&PosixPAL{target: "x86_64-unknown-linux-gnu"})
	(&PosixPAL{target: "x86_64-unknown-linux-gnu"}).EmitWrite(module)
	(&PosixPAL{target: "x86_64-unknown-linux-gnu"}).EmitStackOverflowInit(module)
	out := module.String()

	// Linux now uses sigaction with SA_SIGINFO
	if !strings.Contains(out, "call i32 @sigaction(i32 11,") {
		t.Error("Linux should use sigaction for SIGSEGV (signal 11)")
	}
	// Should NOT use signal() on Linux anymore
	if strings.Contains(out, "call i8* @signal(i32 11,") {
		t.Error("Linux should not use signal() — must use sigaction with SA_SIGINFO")
	}
}

func TestLinuxStackOverflowGlibcLayout(t *testing.T) {
	// Glibc sigaction struct: 152 bytes, flags at offset 136
	module := newModuleWithAlloc(&PosixPAL{target: "x86_64-unknown-linux-gnu"})
	(&PosixPAL{target: "x86_64-unknown-linux-gnu"}).EmitWrite(module)
	(&PosixPAL{target: "x86_64-unknown-linux-gnu"}).EmitStackOverflowInit(module)
	out := module.String()

	// Glibc: 152-byte struct alloca
	if !strings.Contains(out, "[152 x i8]") {
		t.Error("glibc sigaction struct should be 152 bytes")
	}
	// memset to zero the struct
	if !strings.Contains(out, "call i8* @memset(") {
		t.Error("should memset the sigaction struct to zero")
	}
}

func TestLinuxStackOverflowMuslLayout(t *testing.T) {
	// Musl sigaction struct: 40 bytes, flags at offset 24
	module := newModuleWithAlloc(&PosixPAL{target: "x86_64-unknown-linux-musl"})
	(&PosixPAL{target: "x86_64-unknown-linux-musl"}).EmitWrite(module)
	(&PosixPAL{target: "x86_64-unknown-linux-musl"}).EmitStackOverflowInit(module)
	out := module.String()

	// Musl: 40-byte struct alloca
	if !strings.Contains(out, "[40 x i8]") {
		t.Error("musl sigaction struct should be 40 bytes")
	}
	if !strings.Contains(out, "call i32 @sigaction(i32 11,") {
		t.Error("musl should also use sigaction for SIGSEGV")
	}
}

func TestLinuxHandlerSASiginfo(t *testing.T) {
	// B0128: handler should have 3 parameters for SA_SIGINFO
	module := newModuleWithAlloc(&PosixPAL{target: "x86_64-unknown-linux-gnu"})
	(&PosixPAL{target: "x86_64-unknown-linux-gnu"}).EmitWrite(module)
	(&PosixPAL{target: "x86_64-unknown-linux-gnu"}).EmitStackOverflowInit(module)
	out := module.String()

	// Handler must have 3 parameters: (i32 sig, i8* info, i8* ucontext)
	if !strings.Contains(out, "define void @__promise_sigsegv_handler(i32 %sig, i8* %info, i8* %ucontext)") {
		t.Error("Linux handler should have 3 SA_SIGINFO parameters")
	}
	// Must have noreturn attribute
	handlerBody := out[strings.Index(out, "@__promise_sigsegv_handler"):]
	if !strings.Contains(handlerBody, "noreturn") {
		t.Error("handler should be noreturn")
	}
	// Must load si_addr from siginfo at offset 16
	if !strings.Contains(handlerBody, "getelementptr i8, i8* %info, i64 16") {
		t.Error("handler should load si_addr from siginfo at offset 16")
	}
	// Must have hex digit table
	if !strings.Contains(out, "@__promise_hex_digits") {
		t.Error("missing hex digit lookup table")
	}
	// Must have segfault message prefix
	if !strings.Contains(out, "@__promise_segfault_prefix") {
		t.Error("missing segfault message prefix")
	}
	// Must write to stderr (fd 2)
	if !strings.Contains(handlerBody, "call i64 @write(i32 2,") {
		t.Error("handler should write to stderr (fd 2)")
	}
	// Must call _exit(2)
	if !strings.Contains(handlerBody, "call void @_exit(i32 2)") {
		t.Error("handler should call _exit(2)")
	}
	// Must end with unreachable
	if !strings.Contains(handlerBody, "unreachable") {
		t.Error("handler should end with unreachable")
	}
}

func TestLinuxThreadInitSetsSigaltstack(t *testing.T) {
	// B0128: Linux now sets up per-thread sigaltstack for reliable delivery
	p := &PosixPAL{target: "x86_64-unknown-linux-gnu"}
	module := newModuleWithAlloc(p)
	p.EmitStackOverflowThreadInit(module)
	out := module.String()

	// Linux thread init should call sigaltstack
	if !strings.Contains(out, "call i32 @sigaltstack(") {
		t.Error("Linux thread init should call sigaltstack")
	}
	// Allocate 65536 bytes for alternate stack
	if !strings.Contains(out, "call i8* @pal_alloc(i64 65536)") {
		t.Error("missing 64KB alternate stack allocation")
	}
	// Linux stack_t has ss_flags as i32 at field 1 (different from macOS)
	if !strings.Contains(out, "{ i8*, i32, i64 }") {
		t.Error("Linux stack_t should be {i8*, i32, i64} (ss_sp, ss_flags, ss_size)")
	}
}

func TestStackOverflowHandlerBody(t *testing.T) {
	module := newModuleWithAlloc(&PosixPAL{target: "arm64-apple-darwin23.0.0"})
	(&PosixPAL{target: "arm64-apple-darwin23.0.0"}).EmitWrite(module)
	(&PosixPAL{target: "arm64-apple-darwin23.0.0"}).EmitStackOverflowInit(module)
	out := module.String()

	// Handler function must exist with correct name
	if !strings.Contains(out, "define void @__promise_sigsegv_handler(i32 %sig)") {
		t.Error("missing @__promise_sigsegv_handler definition")
	}
	// Must have noreturn attribute (handler never returns)
	handlerBody := out[strings.Index(out, "@__promise_sigsegv_handler"):]
	if !strings.Contains(handlerBody, "noreturn") {
		t.Error("handler should be noreturn")
	}
	// Must write to stderr (fd 2)
	if !strings.Contains(handlerBody, "call i64 @write(i32 2,") {
		t.Error("handler should write to stderr (fd 2)")
	}
	// Must call _exit(2), not exit(2)
	if !strings.Contains(handlerBody, "call void @_exit(i32 2)") {
		t.Error("handler should call _exit(2), not exit")
	}
	// Must end with unreachable (after noreturn _exit)
	if !strings.Contains(handlerBody, "unreachable") {
		t.Error("handler should end with unreachable")
	}
	// Error message global
	if !strings.Contains(out, `@__promise_stack_overflow_msg = constant`) {
		t.Error("missing error message global")
	}
}

func TestLinuxSigactionNoRedefinition(t *testing.T) {
	// Regression: on Linux, EmitStackOverflowInit declares sigaction() via
	// getOrDeclareFunc, and EmitSignalRegister declares signal().
	// Both must use getOrDeclareFunc to avoid "invalid redefinition".
	p := &PosixPAL{target: "x86_64-unknown-linux-gnu"}
	module := newModuleWithAlloc(p)
	p.EmitWrite(module)
	p.EmitStackOverflowInit(module)
	p.EmitSignalInit(module)
	p.EmitSignalRegister(module)
	out := module.String()

	// signal should be declared exactly once (only from EmitSignalRegister now)
	count := strings.Count(out, "define i8* @signal(")
	declCount := strings.Count(out, "declare i8* @signal(")
	total := count + declCount
	if total != 1 {
		t.Errorf("expected exactly 1 signal declaration, got %d (define=%d, declare=%d)", total, count, declCount)
	}
	// sigaction should be declared exactly once (from EmitStackOverflowInit)
	saCount := strings.Count(out, "define i32 @sigaction(")
	saDeclCount := strings.Count(out, "declare i32 @sigaction(")
	saTotal := saCount + saDeclCount
	if saTotal != 1 {
		t.Errorf("expected exactly 1 sigaction declaration, got %d (define=%d, declare=%d)", saTotal, saCount, saDeclCount)
	}
	// Both pal_stack_overflow_init and pal_signal_register should exist
	if !strings.Contains(out, "@pal_stack_overflow_init") {
		t.Error("missing pal_stack_overflow_init")
	}
	if !strings.Contains(out, "@pal_signal_register") {
		t.Error("missing pal_signal_register")
	}
}

// --- Signal PAL Tests ---

func TestEmitSignalInit(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitWrite(module)
			fn := tc.pal.EmitSignalInit(module)
			if fn.Name() != "pal_signal_init" {
				t.Errorf("expected pal_signal_init, got %s", fn.Name())
			}
		})
	}
}

func TestEmitSignalInitPosix(t *testing.T) {
	p := &PosixPAL{}
	module := newModuleWithAlloc(p)
	p.EmitWrite(module)
	p.EmitSignalInit(module)
	out := module.String()

	// Should create a pipe for signal delivery
	assertContains(t, out, "@pipe(", "declares pipe()")
	// Should define the signal handler
	assertContains(t, out, "define void @promise_signal_handler(i32 %signum)", "defines signal handler")
	// Handler truncates signal number to i8 and writes to pipe
	assertContains(t, out, "trunc i32 %signum to i8", "truncates signal to byte")
	// Write fd stored in global
	assertContains(t, out, "@__promise_signal_pipe_wr", "defines write fd global")
	// pal_signal_init returns i32 (read fd)
	assertContains(t, out, "define i32 @pal_signal_init()", "defines pal_signal_init")
}

func TestEmitSignalRegister(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitWrite(module)
			tc.pal.EmitSignalInit(module)
			fn := tc.pal.EmitSignalRegister(module)
			if fn.Name() != "pal_signal_register" {
				t.Errorf("expected pal_signal_register, got %s", fn.Name())
			}
		})
	}
}

func TestEmitSignalRegisterPosix(t *testing.T) {
	p := &PosixPAL{}
	module := newModuleWithAlloc(p)
	p.EmitWrite(module)
	p.EmitSignalInit(module)
	p.EmitSignalRegister(module)
	out := module.String()

	// Uses signal(2) to register the handler
	assertContains(t, out, "@signal(", "declares signal()")
	// pal_signal_register takes signum parameter
	assertContains(t, out, "define i32 @pal_signal_register(i32 %signum)", "defines pal_signal_register")
	// Checks SIG_ERR return (inttoptr -1)
	assertContains(t, out, "inttoptr", "checks SIG_ERR via inttoptr")
	// Returns 0 on success, -1 on error
	assertContains(t, out, "ret i32 0", "returns 0 on success")
	assertContains(t, out, "ret i32 -1", "returns -1 on error")
}

func TestEmitSignalInitWindows(t *testing.T) {
	p := &WindowsPAL{}
	module := newModuleWithAlloc(p)
	p.EmitWrite(module)
	p.EmitSignalInit(module)
	out := module.String()

	// Should use UCRT _pipe for signal delivery
	assertContains(t, out, "@_pipe(", "declares _pipe()")
	// Should use UCRT _write in the handler
	assertContains(t, out, "@_write(", "declares _write()")
	// Should register SetConsoleCtrlHandler
	assertContains(t, out, "@SetConsoleCtrlHandler(", "declares SetConsoleCtrlHandler")
	// Should define the console control handler
	assertContains(t, out, "define i32 @promise_console_ctrl_handler(i32 %dwCtrlType)", "defines console ctrl handler")
	// Write fd stored in global
	assertContains(t, out, "@__promise_signal_pipe_wr", "defines write fd global")
	// Per-signal enable flags
	assertContains(t, out, "@__promise_signal_int_enabled", "defines SIGINT enable flag")
	assertContains(t, out, "@__promise_signal_term_enabled", "defines SIGTERM enable flag")
	// pal_signal_init returns i32 (read fd)
	assertContains(t, out, "define i32 @pal_signal_init()", "defines pal_signal_init")
}

func TestEmitSignalRegisterWindows(t *testing.T) {
	p := &WindowsPAL{}
	module := newModuleWithAlloc(p)
	p.EmitWrite(module)
	p.EmitSignalInit(module)
	p.EmitSignalRegister(module)
	out := module.String()

	// pal_signal_register takes signum parameter
	assertContains(t, out, "define i32 @pal_signal_register(i32 %signum)", "defines pal_signal_register")
	// Returns 0 for supported signals
	assertContains(t, out, "ret i32 0", "returns 0 on success")
	// Returns -1 for unsupported signals
	assertContains(t, out, "ret i32 -1", "returns -1 for unsupported")
	// Checks for SIGINT (2) and SIGTERM (15)
	if !strings.Contains(out, "icmp eq i32 %signum, 2") {
		t.Error("should check for SIGINT (signal 2)")
	}
	if !strings.Contains(out, "icmp eq i32 %signum, 15") {
		t.Error("should check for SIGTERM (signal 15)")
	}
}

func TestStubStackOverflowIsNoop(t *testing.T) {
	// Windows and WASM should emit no-op stubs
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitStackOverflowInit(module)
			tc.pal.EmitStackOverflowThreadInit(module)
			out := module.String()

			// Both functions should be trivial (ret void, no signal handling)
			if strings.Contains(out, "@signal(") || strings.Contains(out, "@sigaction(") {
				t.Error("stub should not call signal/sigaction")
			}
			if strings.Contains(out, "@sigaltstack(") {
				t.Error("stub should not call sigaltstack")
			}
			if strings.Contains(out, "__promise_sigsegv_handler") {
				t.Error("stub should not define SIGSEGV handler")
			}
		})
	}
}

// --- Directory Listing PAL Tests ---

func TestEmitDirOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitDirOpen(module)
			if fn.Name() != "pal_dir_open" {
				t.Errorf("expected pal_dir_open, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i8* @pal_dir_open(i8* %path)", "defines pal_dir_open")
		})
	}
}

func TestEmitDirOpenPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitDirOpen(module)
	out := module.String()
	assertContains(t, out, "@opendir(", "declares opendir()")
	assertContains(t, out, "call i8* @opendir(i8* %path)", "calls opendir")
}

func TestEmitDirNextName(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix/Linux", &PosixPAL{target: "x86_64-unknown-linux-gnu"}},
		{"Posix/macOS", &PosixPAL{target: "arm64-apple-darwin23.0.0"}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitErrno(module)
			fn := tc.pal.EmitDirNextName(module)
			if fn.Name() != "pal_dir_next_name" {
				t.Errorf("expected pal_dir_next_name, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i8* @pal_dir_next_name(i8* %handle)", "defines pal_dir_next_name")
		})
	}
}

func TestEmitDirNextNamePosixOffsets(t *testing.T) {
	// Linux and macOS have different d_name offsets in struct dirent
	for _, tc := range []struct {
		name   string
		target string
		offset string // d_name byte offset
	}{
		{"Linux", "x86_64-unknown-linux-gnu", "i64 19"},
		{"macOS", "arm64-apple-darwin23.0.0", "i64 21"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &PosixPAL{target: tc.target}
			module := newModuleWithAlloc(p)
			p.EmitErrno(module)
			p.EmitDirNextName(module)
			out := module.String()
			assertContains(t, out, "@readdir(", "declares readdir()")
			// Clears errno before readdir
			assertContains(t, out, "store i32 0,", "clears errno before readdir")
			// GEP to d_name at platform-specific offset
			assertContains(t, out, tc.offset, "uses correct d_name offset")
		})
	}
}

func TestEmitDirClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitDirClose(module)
			if fn.Name() != "pal_dir_close" {
				t.Errorf("expected pal_dir_close, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define void @pal_dir_close(i8* %handle)", "defines pal_dir_close")
		})
	}
}

func TestEmitDirClosePosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitDirClose(module)
	out := module.String()
	assertContains(t, out, "@closedir(", "declares closedir()")
	assertContains(t, out, "call i32 @closedir(i8* %handle)", "calls closedir")
}

// --- Environment Variable PAL Tests ---

func TestEmitGetEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitGetEnv(module)
			if fn.Name() != "pal_getenv" {
				t.Errorf("expected pal_getenv, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i8* @pal_getenv(i8* %name)", "defines pal_getenv")
		})
	}
}

func TestEmitGetEnvPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitGetEnv(module)
	out := module.String()
	assertContains(t, out, "@getenv(", "declares getenv()")
	assertContains(t, out, "call i8* @getenv(i8* %name)", "calls getenv")
}

func TestEmitSetEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitSetEnv(module)
			if fn.Name() != "pal_setenv" {
				t.Errorf("expected pal_setenv, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i32 @pal_setenv(i8* %name, i8* %value)", "defines pal_setenv")
		})
	}
}

func TestEmitSetEnvPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitSetEnv(module)
	out := module.String()
	assertContains(t, out, "@setenv(", "declares setenv()")
	// overwrite=1 is passed
	assertContains(t, out, "i32 1)", "passes overwrite=1")
}

func TestEmitUnsetEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitUnsetEnv(module)
			if fn.Name() != "pal_unsetenv" {
				t.Errorf("expected pal_unsetenv, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i32 @pal_unsetenv(i8* %name)", "defines pal_unsetenv")
		})
	}
}

func TestEmitUnsetEnvPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitUnsetEnv(module)
	out := module.String()
	assertContains(t, out, "@unsetenv(", "declares unsetenv()")
}

func TestEmitChdir(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitChdir(module)
			if fn.Name() != "pal_chdir" {
				t.Errorf("expected pal_chdir, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i32 @pal_chdir(i8* %path)", "defines pal_chdir")
		})
	}
}

func TestEmitGetCwd(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitGetCwd(module)
			if fn.Name() != "pal_getcwd" {
				t.Errorf("expected pal_getcwd, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i8* @pal_getcwd(i8* %buf, i64 %len)", "defines pal_getcwd")
		})
	}
}

// --- Process Execution PAL Tests ---

func TestEmitSpawn(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			// EmitSpawn on Posix needs close to be declared first
			tc.pal.EmitFileClose(module)
			fn := tc.pal.EmitSpawn(module)
			if fn.Name() != "pal_spawn" {
				t.Errorf("expected pal_spawn, got %s", fn.Name())
			}
		})
	}
}

func TestEmitSpawnPosix(t *testing.T) {
	p := &PosixPAL{}
	module := newModuleWithAlloc(p)
	p.EmitFileClose(module) // Provides @close
	p.EmitSpawn(module)
	out := module.String()

	assertContains(t, out, "define i32 @pal_spawn(i8* %program, i8** %argv,", "defines pal_spawn")
	assertContains(t, out, "@pipe(", "declares pipe()")
	assertContains(t, out, "@fork(", "declares fork()")
	assertContains(t, out, "@dup2(", "declares dup2()")
	assertContains(t, out, "@execvp(", "declares execvp()")
	assertContains(t, out, "@_exit(", "declares _exit()")
}

// newModuleForReadPipe creates a module with all dependencies for EmitReadPipe:
// pal_alloc, pal_free, pal_realloc, read (via EmitFileRead), close (via EmitFileClose).
func newModuleForReadPipe(pal PAL) *ir.Module {
	module := newModuleWithAlloc(pal)
	pal.EmitRealloc(module)
	pal.EmitFileRead(module)
	pal.EmitFileClose(module)
	return module
}

func TestEmitReadPipe(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleForReadPipe(tc.pal)
			fn := tc.pal.EmitReadPipe(module)
			if fn.Name() != "pal_read_pipe" {
				t.Errorf("expected pal_read_pipe, got %s", fn.Name())
			}
		})
	}
}

func TestEmitReadPipePosix(t *testing.T) {
	p := &PosixPAL{}
	module := newModuleForReadPipe(p)
	p.EmitReadPipe(module)
	out := module.String()

	assertContains(t, out, "define void @pal_read_pipe(i32 %fd,", "defines pal_read_pipe")
	// Initial 4096-byte allocation (rendered as hex u0x1000)
	assertContains(t, out, "call i8* @pal_alloc(i64", "initial allocation")
	// Doubles capacity when full
	assertContains(t, out, "mul i64", "doubles capacity")
	assertContains(t, out, "call i8* @pal_realloc(", "reallocs on growth")
	// Closes fd when done
	assertContains(t, out, "call i32 @close(", "closes fd when done")
}

func TestEmitWaitPid(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			fn := tc.pal.EmitWaitPid(module)
			if fn.Name() != "pal_wait_pid" {
				t.Errorf("expected pal_wait_pid, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i32 @pal_wait_pid(i32 %pid)", "defines pal_wait_pid")
		})
	}
}

func TestEmitWaitPidPosix(t *testing.T) {
	p := &PosixPAL{}
	module := newModuleWithAlloc(p)
	p.EmitWaitPid(module)
	out := module.String()

	assertContains(t, out, "@waitpid(", "declares waitpid()")
	// EINTR retry loop
	assertContains(t, out, "icmp eq i32", "checks for EINTR retry")
}

func TestEmitSpawnStreaming(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitFileClose(module)
			fn := tc.pal.EmitSpawnStreaming(module)
			if fn.Name() != "pal_spawn_streaming" {
				t.Errorf("expected pal_spawn_streaming, got %s", fn.Name())
			}
		})
	}
}

func TestEmitSpawnStreamingPosix(t *testing.T) {
	p := &PosixPAL{}
	module := newModuleWithAlloc(p)
	p.EmitFileClose(module)
	p.EmitSpawnStreaming(module)
	out := module.String()

	assertContains(t, out, "define i32 @pal_spawn_streaming(i8* %program, i8** %argv,", "defines pal_spawn_streaming")
	// Three pipes: stdin, stdout, stderr
	if strings.Count(out, "call i32 @pipe(") < 3 {
		t.Error("expected at least 3 pipe() calls for stdin/stdout/stderr")
	}
	assertContains(t, out, "@fork(", "calls fork()")
	assertContains(t, out, "@dup2(", "calls dup2()")
	assertContains(t, out, "@execvp(", "calls execvp()")
}

func TestEmitKill(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitKill(module)
			if fn.Name() != "pal_kill" {
				t.Errorf("expected pal_kill, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i32 @pal_kill(i32 %pid, i32 %signal)", "defines pal_kill")
		})
	}
}

func TestEmitKillPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitKill(module)
	out := module.String()
	assertContains(t, out, "@kill(", "declares kill()")
	assertContains(t, out, "call i32 @kill(i32 %pid, i32 %signal)", "calls kill()")
}

// --- Windows Process Execution Tests ---

func TestEmitSpawnWindows(t *testing.T) {
	p := &WindowsPAL{}
	module := newModuleWithAlloc(p)
	p.EmitFree(module)
	p.EmitSpawn(module)
	out := module.String()

	// Verify command line builder is emitted
	assertContains(t, out, "define i8* @__promise_argv_to_cmdline(i8** %argv)", "emits argv_to_cmdline helper")
	// Verify CreateProcessA and pipe APIs
	assertContains(t, out, "@CreatePipe(", "declares CreatePipe")
	assertContains(t, out, "@CreateProcessA(", "declares CreateProcessA")
	assertContains(t, out, "@SetHandleInformation(", "declares SetHandleInformation")
	assertContains(t, out, "@CloseHandle(", "declares CloseHandle")
	// Verify STARTUPINFOA setup (STARTF_USESTDHANDLES = 0x100 = 256)
	assertContains(t, out, "i32 256", "sets STARTF_USESTDHANDLES flag")
	// Verify handle packing: ptrtoint + trunc
	assertContains(t, out, "ptrtoint i8*", "packs handle to i64")
	assertContains(t, out, "trunc i64", "truncates handle to i32")
}

func TestEmitReadPipeWindows(t *testing.T) {
	p := &WindowsPAL{}
	module := newModuleForReadPipe(p)
	p.EmitReadPipe(module)
	out := module.String()

	assertContains(t, out, "define void @pal_read_pipe(i32 %fd,", "defines pal_read_pipe")
	// Verify handle unpacking: sext + inttoptr
	assertContains(t, out, "sext i32 %fd to i64", "sign-extends fd to i64")
	assertContains(t, out, "inttoptr i64", "converts i64 to handle")
	// Verify ReadFile call
	assertContains(t, out, "call i32 @ReadFile(", "calls ReadFile")
	// Verify buffer management
	assertContains(t, out, "call i8* @pal_alloc(i64", "initial allocation")
	assertContains(t, out, "mul i64", "doubles capacity")
	assertContains(t, out, "call i8* @pal_realloc(", "reallocs on growth")
	// Closes handle when done
	assertContains(t, out, "call i32 @CloseHandle(", "closes handle when done")
}

func TestEmitWaitPidWindows(t *testing.T) {
	p := &WindowsPAL{}
	module := newModuleWithAlloc(p)
	p.EmitWaitPid(module)
	out := module.String()

	assertContains(t, out, "define i32 @pal_wait_pid(i32 %pid)", "defines pal_wait_pid")
	// Handle unpacking
	assertContains(t, out, "sext i32 %pid to i64", "sign-extends pid to i64")
	assertContains(t, out, "inttoptr i64", "converts i64 to handle")
	// Wait and get exit code
	assertContains(t, out, "call i32 @WaitForSingleObject(", "calls WaitForSingleObject")
	assertContains(t, out, "call i32 @GetExitCodeProcess(", "calls GetExitCodeProcess")
	assertContains(t, out, "call i32 @CloseHandle(", "closes handle")
}

func TestEmitSpawnStreamingWindows(t *testing.T) {
	p := &WindowsPAL{}
	module := newModuleWithAlloc(p)
	p.EmitFree(module)
	p.EmitSpawnStreaming(module)
	out := module.String()

	assertContains(t, out, "define i32 @pal_spawn_streaming(i8* %program, i8** %argv,", "defines pal_spawn_streaming")
	// Three pipes: stdin, stdout, stderr
	if strings.Count(out, "call i32 @CreatePipe(") < 3 {
		t.Error("expected at least 3 CreatePipe() calls for stdin/stdout/stderr")
	}
	assertContains(t, out, "@CreateProcessA(", "calls CreateProcessA")
	assertContains(t, out, "@SetHandleInformation(", "marks handles non-inheritable")
}

func TestEmitKillWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	p.EmitKill(module)
	out := module.String()

	assertContains(t, out, "define i32 @pal_kill(i32 %pid, i32 %signal)", "defines pal_kill")
	// Self-signaling via GenerateConsoleCtrlEvent
	assertContains(t, out, "@GetCurrentProcessId()", "declares GetCurrentProcessId")
	assertContains(t, out, "@GenerateConsoleCtrlEvent(", "declares GenerateConsoleCtrlEvent")
	// Checks for SIGINT (2) and SIGTERM (15)
	if !strings.Contains(out, "icmp eq i32 %signal, 2") {
		t.Error("should check for SIGINT (signal 2)")
	}
	if !strings.Contains(out, "icmp eq i32 %signal, 15") {
		t.Error("should check for SIGTERM (signal 15)")
	}
	// Non-self: TerminateProcess fallback
	assertContains(t, out, "@TerminateProcess(", "declares TerminateProcess")
	assertContains(t, out, "sext i32 %pid to i64", "sign-extends pid to i64")
	assertContains(t, out, "inttoptr i64", "converts i64 to handle")
}

func TestArgvToCmdline(t *testing.T) {
	p := &WindowsPAL{}
	module := newModuleWithAlloc(p)
	p.EmitFree(module)
	emitArgvToCmdline(module)
	out := module.String()

	assertContains(t, out, "define i8* @__promise_argv_to_cmdline(i8** %argv)", "defines argv_to_cmdline")
	// Two-pass: size calculation then fill
	assertContains(t, out, "call i8* @pal_alloc(", "allocates buffer")
	// Quote handling: stores double-quote character (34 = 0x22)
	if !strings.Contains(out, "i8 34") {
		t.Error("expected i8 34 (double quote) in output")
	}
	// Backslash escape for internal quotes (92 = 0x5C)
	if !strings.Contains(out, "i8 92") {
		t.Error("expected i8 92 (backslash) for quote escaping")
	}
}

// --- OS Info PAL Tests ---

func TestEmitGetEnviron(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitGetEnviron(module)
			if fn.Name() != "pal_get_environ" {
				t.Errorf("expected pal_get_environ, got %s", fn.Name())
			}
		})
	}
}

func TestEmitGetEnvironPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitGetEnviron(module)
	out := module.String()

	assertContains(t, out, "define i8** @pal_get_environ()", "defines pal_get_environ")
	// References the C environ global
	assertContains(t, out, "@environ", "references environ global")
	assertContains(t, out, "external", "environ is external")
}

func TestEmitGetUserInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix/Linux", &PosixPAL{target: "x86_64-unknown-linux-gnu"}},
		{"Posix/macOS", &PosixPAL{target: "arm64-apple-darwin23.0.0"}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitGetUserInfo(module)
			if fn.Name() != "pal_get_user_info" {
				t.Errorf("expected pal_get_user_info, got %s", fn.Name())
			}
		})
	}
}

func TestEmitGetUserInfoPosixPasswdLayout(t *testing.T) {
	// Linux and macOS have different struct passwd layouts
	for _, tc := range []struct {
		name     string
		target   string
		dirIndex string // pw_dir GEP index
	}{
		{"Linux", "x86_64-unknown-linux-gnu", "i32 5"},
		{"macOS", "arm64-apple-darwin23.0.0", "i32 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &PosixPAL{target: tc.target}
			module := ir.NewModule()
			p.EmitGetUserInfo(module)
			out := module.String()

			assertContains(t, out, "@getuid(", "declares getuid()")
			assertContains(t, out, "@getpwuid(", "declares getpwuid()")
			assertContains(t, out, "define i32 @pal_get_user_info(", "defines pal_get_user_info")
			// Checks for null return from getpwuid
			assertContains(t, out, "icmp eq", "null-checks getpwuid result")
			// Uses platform-specific pw_dir index
			assertContains(t, out, tc.dirIndex, "uses correct pw_dir index")
		})
	}
}

func TestEmitGetHostname(t *testing.T) {
	for _, tc := range []struct {
		name string
		pal  PAL
	}{
		{"Posix", &PosixPAL{}},
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			fn := tc.pal.EmitGetHostname(module)
			if fn.Name() != "pal_get_hostname" {
				t.Errorf("expected pal_get_hostname, got %s", fn.Name())
			}
			out := module.String()
			assertContains(t, out, "define i8* @pal_get_hostname(i8* %buf, i64 %len)", "defines pal_get_hostname")
		})
	}
}

func TestEmitGetHostnamePosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	p.EmitGetHostname(module)
	out := module.String()

	assertContains(t, out, "@gethostname(", "declares gethostname()")
	// Error check: returns null on failure
	assertContains(t, out, "ret i8* null", "returns null on error")
	// Success: returns buf
	assertContains(t, out, "ret i8* %buf", "returns buf on success")
}

// --- Socket PAL tests (T0069) ---

func TestSocketCreatePosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketCreate(module)
	out := module.String()

	if fn.Name() != "pal_socket_create" {
		t.Errorf("expected pal_socket_create, got %s", fn.Name())
	}
	assertContains(t, out, "@socket(i32", "socket declaration")
	assertContains(t, out, "define i32 @pal_socket_create(i32 %domain, i32 %typ, i32 %protocol)", "definition")
	assertContains(t, out, "call i32 @socket(", "calls socket()")
	assertContains(t, out, "icmp slt i32", "error check")
}

func TestSocketCreateWasm(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}
	fn := p.EmitSocketCreate(module)
	out := module.String()

	if fn.Name() != "pal_socket_create" {
		t.Errorf("expected pal_socket_create, got %s", fn.Name())
	}
	assertContains(t, out, "ret i32 -38", "WASM stub returns -ENOSYS")
}

func TestSocketCreateWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketCreate(module)
	out := module.String()

	if fn.Name() != "pal_socket_create" {
		t.Errorf("expected pal_socket_create, got %s", fn.Name())
	}
	assertContains(t, out, "declare i64 @socket(i32", "Winsock socket() returns SOCKET (i64)")
	assertContains(t, out, "define i32 @pal_socket_create(i32 %domain, i32 %typ, i32 %protocol)", "definition")
	assertContains(t, out, "call i64 @socket(", "calls socket() returning i64")
	assertContains(t, out, "icmp eq i64", "checks INVALID_SOCKET")
	assertContains(t, out, "trunc i64", "truncates SOCKET to i32")
	assertContains(t, out, "@WSAGetLastError()", "calls WSAGetLastError on error")
	assertContains(t, out, "@__pal_wsa_ensure_init()", "calls WSA init")
}

func TestSocketBindWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketBind(module)
	out := module.String()

	if fn.Name() != "pal_socket_bind" {
		t.Errorf("expected pal_socket_bind, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_bind(i32 %fd, i8* %addr, i32 %addrlen)", "definition")
	assertContains(t, out, "zext i32", "zero-extends fd to SOCKET")
	assertContains(t, out, "call i32 @bind(i64", "calls bind() with SOCKET param")
	assertContains(t, out, "@WSAGetLastError()", "calls WSAGetLastError on error")
}

func TestSocketListenWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketListen(module)
	out := module.String()

	if fn.Name() != "pal_socket_listen" {
		t.Errorf("expected pal_socket_listen, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_listen(i32 %fd, i32 %backlog)", "definition")
	assertContains(t, out, "call i32 @listen(i64", "calls listen() with SOCKET param")
}

func TestSocketAcceptWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketAccept(module)
	out := module.String()

	if fn.Name() != "pal_socket_accept" {
		t.Errorf("expected pal_socket_accept, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_accept(i32 %fd, i8* %addr, i32* %addrlen)", "definition")
	assertContains(t, out, "call i64 @accept(i64", "calls accept() returning SOCKET")
	assertContains(t, out, "trunc i64", "truncates SOCKET to i32")
}

func TestSocketConnectWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketConnect(module)
	out := module.String()

	if fn.Name() != "pal_socket_connect" {
		t.Errorf("expected pal_socket_connect, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_connect(i32 %fd, i8* %addr, i32 %addrlen)", "definition")
	assertContains(t, out, "call i32 @connect(i64", "calls connect() with SOCKET param")
}

func TestSocketSendRecvWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	sendFn := p.EmitSocketSend(module)
	recvFn := p.EmitSocketRecv(module)
	out := module.String()

	if sendFn.Name() != "pal_socket_send" {
		t.Errorf("expected pal_socket_send, got %s", sendFn.Name())
	}
	if recvFn.Name() != "pal_socket_recv" {
		t.Errorf("expected pal_socket_recv, got %s", recvFn.Name())
	}
	// Winsock send/recv take i32 len (not i64 like POSIX)
	assertContains(t, out, "define i64 @pal_socket_send(i32 %fd, i8* %buf, i64 %len, i32 %flags)", "send definition")
	assertContains(t, out, "define i64 @pal_socket_recv(i32 %fd, i8* %buf, i64 %len, i32 %flags)", "recv definition")
	assertContains(t, out, "trunc i64 %len to i32", "truncates len for Winsock")
	assertContains(t, out, "sext i32", "sign-extends i32 result to i64")
}

func TestSocketCloseWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketClose(module)
	out := module.String()

	if fn.Name() != "pal_socket_close" {
		t.Errorf("expected pal_socket_close, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_close(i32 %fd)", "definition")
	assertContains(t, out, "call i32 @closesocket(i64", "calls closesocket() not close()")
}

func TestSocketSetOptWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketSetOpt(module)
	out := module.String()

	if fn.Name() != "pal_socket_setopt" {
		t.Errorf("expected pal_socket_setopt, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_setopt(i32 %fd, i32 %level, i32 %opt, i8* %val, i32 %len)", "definition")
	assertContains(t, out, "call i32 @setsockopt(i64", "calls setsockopt() with SOCKET param")
}

func TestSocketShutdownWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketShutdown(module)
	out := module.String()

	if fn.Name() != "pal_socket_shutdown" {
		t.Errorf("expected pal_socket_shutdown, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_shutdown(i32 %fd, i32 %how)", "definition")
	assertContains(t, out, "call i32 @shutdown(i64", "calls shutdown() with SOCKET param")
}

func TestSocketSetNonBlockWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketSetNonBlock(module)
	out := module.String()

	if fn.Name() != "pal_socket_set_nonblock" {
		t.Errorf("expected pal_socket_set_nonblock, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_set_nonblock(i32 %fd)", "definition")
	assertContains(t, out, "@ioctlsocket(", "calls ioctlsocket not fcntl")
	// FIONBIO = 0x8004667e = -2147195266 as signed i32
	assertContains(t, out, "-2147195266", "FIONBIO constant")
}

func TestSocketGetErrorWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitSocketGetError(module)
	out := module.String()

	if fn.Name() != "pal_socket_get_error" {
		t.Errorf("expected pal_socket_get_error, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_get_error(i32 %fd)", "definition")
	assertContains(t, out, "@getsockopt(i64", "calls getsockopt with SOCKET param")
	// Windows SOL_SOCKET = 0xFFFF (LLVM renders as unsigned hex)
	assertContains(t, out, "u0xFFFF", "SOL_SOCKET constant")
	// Windows SO_ERROR = 0x1007 = 4103
	assertContains(t, out, "i32 4103", "SO_ERROR constant")
}

func TestGetAddrInfoWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitGetAddrInfo(module)
	out := module.String()

	if fn.Name() != "pal_getaddrinfo" {
		t.Errorf("expected pal_getaddrinfo, got %s", fn.Name())
	}
	assertContains(t, out, "@getaddrinfo(", "getaddrinfo declaration")
	assertContains(t, out, "define i32 @pal_getaddrinfo(i8* %host, i8* %port, i8* %hints, i8** %result)", "definition")
	assertContains(t, out, "@__pal_wsa_ensure_init()", "calls WSA init")
}

func TestFreeAddrInfoWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fn := p.EmitFreeAddrInfo(module)
	out := module.String()

	if fn.Name() != "pal_freeaddrinfo" {
		t.Errorf("expected pal_freeaddrinfo, got %s", fn.Name())
	}
	assertContains(t, out, "@freeaddrinfo(", "freeaddrinfo declaration")
	assertContains(t, out, "define void @pal_freeaddrinfo(i8* %result)", "definition")
}

func TestWSAEnsureInitWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	// EmitSocketCreate triggers WSA init helper creation
	p.EmitSocketCreate(module)
	out := module.String()

	assertContains(t, out, "define void @__pal_wsa_ensure_init()", "init function defined")
	assertContains(t, out, "@WSAStartup(", "calls WSAStartup")
	assertContains(t, out, "i32 514", "MAKEWORD(2,2) = 0x0202 = 514")
	assertContains(t, out, "@__wsa_init_done", "uses init flag global")
	assertContains(t, out, "@__wsa_data", "uses WSADATA buffer global")

	// Second call on same module reuses the existing init function (covers early-return path)
	p.EmitGetAddrInfo(module)
	out2 := module.String()
	// Should have exactly one definition of __pal_wsa_ensure_init, not two
	if count := strings.Count(out2, "define void @__pal_wsa_ensure_init"); count != 1 {
		t.Errorf("expected exactly 1 definition of __pal_wsa_ensure_init, got %d", count)
	}
}

func TestSocketBindPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketBind(module)
	out := module.String()

	if fn.Name() != "pal_socket_bind" {
		t.Errorf("expected pal_socket_bind, got %s", fn.Name())
	}
	assertContains(t, out, "@bind(i32", "bind declaration")
	assertContains(t, out, "define i32 @pal_socket_bind(i32 %fd, i8* %addr, i32 %addrlen)", "definition")
	assertContains(t, out, "call i32 @bind(", "calls bind()")
}

func TestSocketListenPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketListen(module)
	out := module.String()

	if fn.Name() != "pal_socket_listen" {
		t.Errorf("expected pal_socket_listen, got %s", fn.Name())
	}
	assertContains(t, out, "@listen(i32", "listen declaration")
	assertContains(t, out, "define i32 @pal_socket_listen(i32 %fd, i32 %backlog)", "definition")
}

func TestSocketAcceptPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketAccept(module)
	out := module.String()

	if fn.Name() != "pal_socket_accept" {
		t.Errorf("expected pal_socket_accept, got %s", fn.Name())
	}
	assertContains(t, out, "@accept(i32", "accept declaration")
	assertContains(t, out, "define i32 @pal_socket_accept(i32 %fd, i8* %addr, i32* %addrlen)", "definition")
}

func TestSocketConnectPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketConnect(module)
	out := module.String()

	if fn.Name() != "pal_socket_connect" {
		t.Errorf("expected pal_socket_connect, got %s", fn.Name())
	}
	assertContains(t, out, "@connect(i32", "connect declaration")
	assertContains(t, out, "define i32 @pal_socket_connect(i32 %fd, i8* %addr, i32 %addrlen)", "definition")
}

func TestSocketSendRecvPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	sendFn := p.EmitSocketSend(module)
	recvFn := p.EmitSocketRecv(module)
	out := module.String()

	if sendFn.Name() != "pal_socket_send" {
		t.Errorf("expected pal_socket_send, got %s", sendFn.Name())
	}
	if recvFn.Name() != "pal_socket_recv" {
		t.Errorf("expected pal_socket_recv, got %s", recvFn.Name())
	}
	assertContains(t, out, "@send(i32", "send declaration")
	assertContains(t, out, "@recv(i32", "recv declaration")
	assertContains(t, out, "define i64 @pal_socket_send(i32 %fd, i8* %buf, i64 %len, i32 %flags)", "send definition")
	assertContains(t, out, "define i64 @pal_socket_recv(i32 %fd, i8* %buf, i64 %len, i32 %flags)", "recv definition")
}

func TestSocketClosePosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketClose(module)
	out := module.String()

	if fn.Name() != "pal_socket_close" {
		t.Errorf("expected pal_socket_close, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_socket_close(i32 %fd)", "definition")
	assertContains(t, out, "call i32 @close(", "calls close()")
}

func TestSocketSetOptPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketSetOpt(module)
	out := module.String()

	if fn.Name() != "pal_socket_setopt" {
		t.Errorf("expected pal_socket_setopt, got %s", fn.Name())
	}
	assertContains(t, out, "@setsockopt(i32", "setsockopt declaration")
	assertContains(t, out, "define i32 @pal_socket_setopt(i32 %fd, i32 %level, i32 %opt, i8* %val, i32 %len)", "definition")
}

func TestSocketShutdownPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitSocketShutdown(module)
	out := module.String()

	if fn.Name() != "pal_socket_shutdown" {
		t.Errorf("expected pal_socket_shutdown, got %s", fn.Name())
	}
	assertContains(t, out, "@shutdown(i32", "shutdown declaration")
	assertContains(t, out, "define i32 @pal_socket_shutdown(i32 %fd, i32 %how)", "definition")
}

func TestSocketSetNonBlockPosix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		oNB    string // O_NONBLOCK value in or instruction
	}{
		{"macOS", "aarch64-apple-darwin", "or i32 %"},   // or i32 %flags, 4
		{"Linux", "x86_64-unknown-linux-gnu", ", 2048"}, // or i32 %flags, 2048
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			p := &PosixPAL{target: tc.target}
			fn := p.EmitSocketSetNonBlock(module)
			out := module.String()

			if fn.Name() != "pal_socket_set_nonblock" {
				t.Errorf("expected pal_socket_set_nonblock, got %s", fn.Name())
			}
			assertContains(t, out, "define i32 @pal_socket_set_nonblock(i32 %fd)", "definition")
			assertContains(t, out, "@fcntl(", "fcntl declaration")
			// F_GETFL = 3
			assertContains(t, out, "i32 3)", "F_GETFL constant")
			// F_SETFL = 4
			assertContains(t, out, "i32 4,", "F_SETFL constant")
			// O_NONBLOCK platform-specific (in or instruction)
			assertContains(t, out, tc.oNB, "O_NONBLOCK constant")
		})
	}
}

func TestSocketGetErrorPosix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    string
		solSocket string
		soError   string
	}{
		{"macOS", "aarch64-apple-darwin", "u0xFFFF", "i32 4103"},
		{"Linux", "x86_64-unknown-linux-gnu", "i32 1,", "i32 4,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := ir.NewModule()
			p := &PosixPAL{target: tc.target}
			fn := p.EmitSocketGetError(module)
			out := module.String()

			if fn.Name() != "pal_socket_get_error" {
				t.Errorf("expected pal_socket_get_error, got %s", fn.Name())
			}
			assertContains(t, out, "define i32 @pal_socket_get_error(i32 %fd)", "definition")
			assertContains(t, out, "@getsockopt(", "getsockopt declaration")
			assertContains(t, out, tc.solSocket, "SOL_SOCKET constant")
			assertContains(t, out, tc.soError, "SO_ERROR constant")
		})
	}
}

func TestGetAddrInfoPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitGetAddrInfo(module)
	out := module.String()

	if fn.Name() != "pal_getaddrinfo" {
		t.Errorf("expected pal_getaddrinfo, got %s", fn.Name())
	}
	assertContains(t, out, "@getaddrinfo(", "getaddrinfo declaration")
	assertContains(t, out, "define i32 @pal_getaddrinfo(i8* %host, i8* %port, i8* %hints, i8** %result)", "definition")
}

func TestFreeAddrInfoPosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitFreeAddrInfo(module)
	out := module.String()

	if fn.Name() != "pal_freeaddrinfo" {
		t.Errorf("expected pal_freeaddrinfo, got %s", fn.Name())
	}
	assertContains(t, out, "@freeaddrinfo(", "freeaddrinfo declaration")
	assertContains(t, out, "define void @pal_freeaddrinfo(i8* %result)", "definition")
}

func TestSocketStubsWasm(t *testing.T) {
	// Verify all socket stubs return -ENOSYS on WASM
	module := ir.NewModule()
	p := &WasmPAL{}

	fns := []struct {
		name string
		emit func(*ir.Module) *ir.Func
	}{
		{"pal_socket_create", p.EmitSocketCreate},
		{"pal_socket_bind", p.EmitSocketBind},
		{"pal_socket_listen", p.EmitSocketListen},
		{"pal_socket_accept", p.EmitSocketAccept},
		{"pal_socket_connect", p.EmitSocketConnect},
		{"pal_socket_close", p.EmitSocketClose},
		{"pal_socket_setopt", p.EmitSocketSetOpt},
		{"pal_socket_shutdown", p.EmitSocketShutdown},
		{"pal_socket_set_nonblock", p.EmitSocketSetNonBlock},
		{"pal_socket_get_error", p.EmitSocketGetError},
	}

	for _, tc := range fns {
		fn := tc.emit(module)
		if fn.Name() != tc.name {
			t.Errorf("expected %s, got %s", tc.name, fn.Name())
		}
	}

	// i64 return stubs
	sendFn := p.EmitSocketSend(module)
	if sendFn.Name() != "pal_socket_send" {
		t.Errorf("expected pal_socket_send, got %s", sendFn.Name())
	}
	recvFn := p.EmitSocketRecv(module)
	if recvFn.Name() != "pal_socket_recv" {
		t.Errorf("expected pal_socket_recv, got %s", recvFn.Name())
	}

	// getaddrinfo/freeaddrinfo
	gaiFn := p.EmitGetAddrInfo(module)
	if gaiFn.Name() != "pal_getaddrinfo" {
		t.Errorf("expected pal_getaddrinfo, got %s", gaiFn.Name())
	}
	faiFn := p.EmitFreeAddrInfo(module)
	if faiFn.Name() != "pal_freeaddrinfo" {
		t.Errorf("expected pal_freeaddrinfo, got %s", faiFn.Name())
	}

	out := module.String()
	assertContains(t, out, "ret i32 -38", "i32 stubs return -ENOSYS")
	assertContains(t, out, "ret i64 -38", "i64 stubs return -ENOSYS")
}

// --- Reactor PAL tests (T0070) ---

func TestReactorCreatePosixMacOS(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "aarch64-apple-darwin"}
	fn := p.EmitReactorCreate(module)
	out := module.String()

	if fn.Name() != "pal_reactor_create" {
		t.Errorf("expected pal_reactor_create, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_create()", "definition")
	assertContains(t, out, "@kqueue()", "calls kqueue on macOS")
	assertContains(t, out, "icmp slt i32", "error check")
}

func TestReactorCreatePosixLinux(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "x86_64-unknown-linux-gnu"}
	fn := p.EmitReactorCreate(module)
	out := module.String()

	if fn.Name() != "pal_reactor_create" {
		t.Errorf("expected pal_reactor_create, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_create()", "definition")
	assertContains(t, out, "@epoll_create1(i32 0)", "calls epoll_create1 on Linux")
}

func TestReactorAddPosixMacOS(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "aarch64-apple-darwin"}
	fn := p.EmitReactorAdd(module)
	out := module.String()

	if fn.Name() != "pal_reactor_add" {
		t.Errorf("expected pal_reactor_add, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_add(i32 %rfd, i32 %fd, i8* %userdata)", "definition")
	assertContains(t, out, "@kevent(", "calls kevent on macOS")
	// EVFILT_READ = -1, EVFILT_WRITE = -2
	assertContains(t, out, "i16 -1", "EVFILT_READ filter")
	assertContains(t, out, "i16 -2", "EVFILT_WRITE filter")
}

func TestReactorAddPosixLinux(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "x86_64-unknown-linux-gnu"}
	fn := p.EmitReactorAdd(module)
	out := module.String()

	if fn.Name() != "pal_reactor_add" {
		t.Errorf("expected pal_reactor_add, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_add(i32 %rfd, i32 %fd, i8* %userdata)", "definition")
	assertContains(t, out, "@epoll_ctl(", "calls epoll_ctl on Linux")
}

func TestReactorRemovePosixMacOS(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "aarch64-apple-darwin"}
	fn := p.EmitReactorRemove(module)
	out := module.String()

	if fn.Name() != "pal_reactor_remove" {
		t.Errorf("expected pal_reactor_remove, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_remove(i32 %rfd, i32 %fd)", "definition")
	assertContains(t, out, "@kevent(", "calls kevent on macOS")
}

func TestReactorRemovePosixLinux(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "x86_64-unknown-linux-gnu"}
	fn := p.EmitReactorRemove(module)
	out := module.String()

	if fn.Name() != "pal_reactor_remove" {
		t.Errorf("expected pal_reactor_remove, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_remove(i32 %rfd, i32 %fd)", "definition")
	assertContains(t, out, "@epoll_ctl(", "calls epoll_ctl on Linux")
}

func TestReactorPollPosix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		syscall string
	}{
		{"macOS", "aarch64-apple-darwin", "@kevent("},
		{"Linux", "x86_64-unknown-linux-gnu", "@epoll_wait("},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(&PosixPAL{target: tc.target})
			p := &PosixPAL{target: tc.target}
			fn := p.EmitReactorPoll(module)
			out := module.String()

			if fn.Name() != "pal_reactor_poll" {
				t.Errorf("expected pal_reactor_poll, got %s", fn.Name())
			}
			assertContains(t, out, "define i32 @pal_reactor_poll(i32 %rfd, i8* %events_buf, i32 %max_events, i32 %timeout_ms)", "definition")
			assertContains(t, out, tc.syscall, "platform-specific poll syscall")
			// B0326: verify syscall buffer is stack-allocated (alloca), not heap-allocated (pal_alloc).
			// Heap allocation would be tracked by the alloc counter and race with per-test leak detection.
			assertContains(t, out, "alloca i8,", "syscall buffer uses stack alloca (B0326)")
			if strings.Contains(out, "call noalias i8* @pal_alloc(") {
				t.Error("B0326: pal_reactor_poll must not heap-allocate syscall buffer — use alloca instead")
			}
		})
	}
}

func TestReactorClosePosix(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{}
	fn := p.EmitReactorClose(module)
	out := module.String()

	if fn.Name() != "pal_reactor_close" {
		t.Errorf("expected pal_reactor_close, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_close(i32 %rfd)", "definition")
	assertContains(t, out, "@close(", "calls close")
}

func TestReactorStubsWasm(t *testing.T) {
	module := ir.NewModule()
	p := &WasmPAL{}

	fns := []struct {
		name string
		emit func(*ir.Module) *ir.Func
	}{
		{"pal_reactor_create", p.EmitReactorCreate},
		{"pal_reactor_add", p.EmitReactorAdd},
		{"pal_reactor_remove", p.EmitReactorRemove},
		{"pal_reactor_poll", p.EmitReactorPoll},
		{"pal_reactor_close", p.EmitReactorClose},
	}

	for _, tc := range fns {
		fn := tc.emit(module)
		if fn.Name() != tc.name {
			t.Errorf("expected %s, got %s", tc.name, fn.Name())
		}
	}

	out := module.String()
	assertContains(t, out, "ret i32 -38", "stubs return -ENOSYS")
}

// --- Windows reactor tests (T0070) ---

func newWindowsModuleWithDeps() *ir.Module {
	module := ir.NewModule()
	p := &WindowsPAL{}
	p.EmitAlloc(module)
	p.EmitFree(module)
	p.EmitMutexInit(module)
	p.EmitMutexLock(module)
	p.EmitMutexUnlock(module)
	p.EmitMutexDestroy(module)
	return module
}

func TestReactorCreateWindows(t *testing.T) {
	module := newWindowsModuleWithDeps()
	p := &WindowsPAL{}
	fn := p.EmitReactorCreate(module)
	out := module.String()

	if fn.Name() != "pal_reactor_create" {
		t.Errorf("expected pal_reactor_create, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_create()", "definition")
	assertContains(t, out, "@pal_mutex_init()", "creates mutex")
	assertContains(t, out, "@__reactor_lock", "uses reactor lock global")
	assertContains(t, out, "@__reactor_count", "uses reactor count global")
	assertContains(t, out, "ret i32 0", "returns 0 on success")
}

func TestReactorAddWindows(t *testing.T) {
	module := newWindowsModuleWithDeps()
	p := &WindowsPAL{}
	fn := p.EmitReactorAdd(module)
	out := module.String()

	if fn.Name() != "pal_reactor_add" {
		t.Errorf("expected pal_reactor_add, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_add(i32 %rfd, i32 %fd, i8* %userdata)", "definition")
	assertContains(t, out, "@pal_mutex_lock(", "locks mutex")
	assertContains(t, out, "@pal_mutex_unlock(", "unlocks mutex")
	assertContains(t, out, "@__reactor_pollfds", "stores in pollfds array")
	assertContains(t, out, "@__reactor_userdata", "stores in userdata array")
	// POLLRDNORM|POLLWRNORM = 0x0110 = 272
	assertContains(t, out, "i16 272", "sets POLLRDNORM|POLLWRNORM events")
}

func TestReactorRemoveWindows(t *testing.T) {
	module := newWindowsModuleWithDeps()
	p := &WindowsPAL{}
	fn := p.EmitReactorRemove(module)
	out := module.String()

	if fn.Name() != "pal_reactor_remove" {
		t.Errorf("expected pal_reactor_remove, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_remove(i32 %rfd, i32 %fd)", "definition")
	assertContains(t, out, "@pal_mutex_lock(", "locks mutex")
	assertContains(t, out, "@pal_mutex_unlock(", "unlocks mutex")
	assertContains(t, out, "icmp eq i64", "compares fd values")
}

func TestReactorPollWindows(t *testing.T) {
	module := newWindowsModuleWithDeps()
	p := &WindowsPAL{}
	fn := p.EmitReactorPoll(module)
	out := module.String()

	if fn.Name() != "pal_reactor_poll" {
		t.Errorf("expected pal_reactor_poll, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_poll(i32 %rfd, i8* %events_buf, i32 %max_events, i32 %timeout_ms)", "definition")
	assertContains(t, out, "@WSAPoll(", "calls WSAPoll")
	assertContains(t, out, "@WSAGetLastError()", "calls WSAGetLastError on error")
	assertContains(t, out, "@pal_mutex_lock(", "locks mutex")
	assertContains(t, out, "@pal_mutex_unlock(", "unlocks mutex")
}

func TestReactorCloseWindows(t *testing.T) {
	module := newWindowsModuleWithDeps()
	p := &WindowsPAL{}
	fn := p.EmitReactorClose(module)
	out := module.String()

	if fn.Name() != "pal_reactor_close" {
		t.Errorf("expected pal_reactor_close, got %s", fn.Name())
	}
	assertContains(t, out, "define i32 @pal_reactor_close(i32 %rfd)", "definition")
	assertContains(t, out, "@pal_mutex_destroy(", "destroys mutex")
	assertContains(t, out, "@__reactor_lock", "accesses reactor lock")
}

func TestReactorGlobalsReusedWindows(t *testing.T) {
	// Exercises the "globals already exist" early-return path in getOrCreateReactorGlobals.
	module := newWindowsModuleWithDeps()
	p := &WindowsPAL{}
	p.EmitReactorCreate(module)
	p.EmitReactorAdd(module) // second call reuses existing globals
	out := module.String()

	// Verify only one set of globals exists (not duplicated)
	count := strings.Count(out, "@__reactor_pollfds")
	if count < 2 {
		t.Error("expected @__reactor_pollfds to be referenced in both create and add")
	}
	// Verify globals are defined exactly once (global def appears once)
	defCount := strings.Count(out, "__reactor_pollfds = global")
	if defCount != 1 {
		t.Errorf("expected exactly 1 __reactor_pollfds definition, got %d", defCount)
	}
}

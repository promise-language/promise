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

func TestWindowsThreadCreateUsesCreateThread(t *testing.T) {
	module := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitThreadCreate(module)
	out := module.String()

	// Should declare CreateThread
	if !strings.Contains(out, "@CreateThread") {
		t.Error("missing CreateThread declaration")
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

	// 2MB stack size constant passed to CreateThread (0x200000 = 2097152)
	if !strings.Contains(out, "i64 u0x200000") {
		t.Error("missing 2MB stack size constant (u0x200000) in CreateThread call")
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
	// StatSize needs open, close, lseek already declared
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

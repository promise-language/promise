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

// --- Allocator tests (shared libc wrappers, tested across all PALs) ---

func TestEmitAlloc(t *testing.T) {
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
}

func TestEmitFree(t *testing.T) {
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
}

func TestEmitRealloc(t *testing.T) {
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
}

// --- ForTarget dispatch tests ---

func TestForTargetReturnsPosixPAL(t *testing.T) {
	triples := []string{
		"arm64-apple-macosx11.0.0",
		"x86_64-apple-macosx10.15.0",
		"aarch64-unknown-linux-gnu",
		"x86_64-unknown-linux-gnu",
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
}

func TestStubThreadCreateCallsSynchronously(t *testing.T) {
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitThreadCreate(module)
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
		})
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
	pals := []struct {
		name string
		pal  PAL
	}{
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tc := range pals {
		t.Run(tc.name, func(t *testing.T) {
			module := newModuleWithAlloc(tc.pal)
			tc.pal.EmitCondBroadcast(module)
			out := module.String()

			// Stubs should NOT declare pthread_cond_broadcast
			if strings.Contains(out, "pthread_cond_broadcast") {
				t.Error("stub should not declare pthread_cond_broadcast")
			}
			// Should just return void (no-op)
			if !strings.Contains(out, "ret void") {
				t.Error("stub should return void (no-op)")
			}
		})
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
	tests := []struct {
		name string
		pal  PAL
	}{
		{"Windows", &WindowsPAL{}},
		{"Wasm", &WasmPAL{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.pal.EmitNumCPUs(module)
			out := module.String()
			if !strings.Contains(out, "ret i32 1") {
				t.Error("stub pal_num_cpus should return 1")
			}
		})
	}
}

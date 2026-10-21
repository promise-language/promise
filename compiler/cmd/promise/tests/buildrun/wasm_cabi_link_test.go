package buildrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"github.com/promise-language/promise/compiler/internal/wasmtest"
)

// T1660: `promise bind wit -canonical-abi` output must build for wasm32-wasi
// without a single wasm-ld signature mismatch.
//
// This is the coverage the canonical path never had. bindgen_test.go, wit_test.go
// and bind_test.go all assert on emitted source *strings*, so a helper declared
// with a signature the runtime object cannot match reads as correct forever.
// Nothing built a canonical binding, and wasm-ld reports a mismatch as a warning
// while substituting a trapping stub — so the defect was invisible twice over.
//
// A string result is the exact shape T1660 broke, since `string` is not an
// opaque container type and so took the sret path. The `list<u8>` pair pulls in
// bindgen's per-element cabi_vector_* helpers. A helper the runtime object does
// not define is no link error either: --allow-undefined turns it into a host
// import, which wasm-ld never signature-checks and no host provides, so the
// build succeeds and the module refuses to instantiate (T2128). Hence the
// module's imports are read back as well — a canonical helper is statically
// linked, never imported (docs/wasm-bindings.md). The scalar types are covered
// by TestBindWitCanonicalAbiScalarsBuildForWasm below.
func TestBindWitCanonicalAbiLinksCleanForWasm(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	witPath := filepath.Join(dir, "api.wit")
	wit := `package test:api;

interface greet {
  hello: func(name: string) -> string;
  checksum: func(data: list<u8>) -> u32;
  payload: func() -> list<u8>;
}

world api {
  import greet;
}
`
	if err := os.WriteFile(witPath, []byte(wit), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "wit", "-canonical-abi", "-name", "greetapi", "-o", outDir, witPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind wit -canonical-abi failed: %v\n%s", err, out)
	}

	// Call the wrapper so the helpers survive LTO. Without a caller every cabi_*
	// symbol is dead-stripped before wasm-ld sees it, which is exactly why
	// tests/catalog/wasi_preview_2_test.pr passed against the broken lowering.
	userPath := filepath.Join(outDir, "use_greet.pr")
	user := `main() {
    s := hello("world");
    print_line(s);
    print_line("{checksum([1u8, 2u8, 3u8])}");
    p := payload();
    print_line("{p.len}");
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	wasmPath := filepath.Join(dir, "greet.wasm")
	buildCmd := exec.Command(bin, "build", "-target", "wasm32-wasi", "-o", wasmPath, outDir)
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build -target wasm32-wasi failed (want exit 0): %v\n%s", err, out)
	}
	if strings.Contains(string(out), "signature mismatch") {
		t.Errorf("canonical binding linked with a wasm-ld signature mismatch:\n%s", out)
	}

	module, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	imports, err := wasmtest.Imports(module)
	if err != nil {
		t.Fatalf("%s: %v", wasmPath, err)
	}
	// The interface's own functions are the imports this module should have, so
	// finding them shows the import section was read rather than missed.
	hostFuncs := map[string]bool{}
	for _, imp := range imports {
		if imp.Module == "test:api/greet" {
			hostFuncs[imp.Name] = true
		}
		if strings.HasPrefix(imp.Name, "cabi_") {
			t.Errorf("canonical binding imports %s::%s — a helper wasm_alloc.o does not define, which no host provides", imp.Module, imp.Name)
		}
		// wasm_alloc.o's allocating helpers call the PAL allocator, which the
		// module itself defines (#261); left unresolved it would become a host
		// import no host provides, just like a missing helper.
		if strings.HasPrefix(imp.Name, "pal_") {
			t.Errorf("canonical binding imports %s::%s — a PAL function the module should define, which no host provides", imp.Module, imp.Name)
		}
	}
	for _, name := range []string{"hello", "checksum", "payload"} {
		if !hostFuncs[name] {
			t.Errorf("module does not import test:api/greet::%s (imports: %+v)", name, imports)
		}
	}
}

// T2129: `promise bind wit -canonical-abi` output must COMPILE for every WIT
// scalar, in both directions.
//
// The extern carries the canonical flat type (every integer narrower than 64
// bits is i32, 64-bit is i64, bool and char are i32) while the public wrapper
// carries the WIT type, and Promise has no implicit numeric conversion — so a
// wrapper that returned the extern's result verbatim did not compile. Every
// unsigned result, bool, and char was affected, which is most real WIT: u32/u64
// are the default integer types across the WASI preview-2 interfaces.
//
// Generation-only assertions cannot catch this — a body of `return _c();`
// matches its expected string whatever the declared types are — so the gate has
// to be a build. It is build-only rather than build-and-run: the imports have no
// host, and the conversions themselves are language behaviour covered by
// tests/e2e/scalar_casts_test.pr.
//
// A `result<T, E>` carrying a payload is included: it exceeds one flat result,
// so it takes the retptr path, where the conversion applies to a _cabi_load_*
// helper rather than to the extern's own result. `result<bool, E>` is the case
// that has to narrow to the stored width before testing — see the comment on
// liftScalarFromMemory.
//
// The fixture stays away from an unrelated open defect, so a failure here names
// this one: a payload-less `result` stays on the direct path, where the wrapper
// propagates with `^` instead of `?^` and the extern is declared with a trailing
// bang — neither of which parses (T2146). Resource members stay scalar-only for
// a second reason: a string or list on a resource is marshalled by no one
// (T2144).
func TestBindWitCanonicalAbiScalarsBuildForWasm(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	witPath := filepath.Join(dir, "api.wit")
	wit := `package test:api;

interface nums {
  a: func() -> u8;
  b: func() -> u16;
  c: func() -> u32;
  d: func() -> u64;
  e: func() -> s8;
  f: func() -> s16;
  g: func() -> s32;
  h: func() -> s64;
  i: func() -> f32;
  j: func() -> f64;
  k: func() -> bool;
  l: func() -> char;
  mix: func(p: u8, q: u16, r: u32, s: u64, t: s8, u: s16, v: s32, w: s64, x: f32, y: bool, z: char) -> u32;

  ready: func() -> result<bool, string>;
  total: func() -> result<u64, string>;

  resource counter {
    constructor(start: u32);
    bump: func(by: u8) -> bool;
    value: func() -> u64;
  }
}

world api {
  import nums;
}
`
	if err := os.WriteFile(witPath, []byte(wit), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "wit", "-canonical-abi", "-name", "numsapi", "-o", outDir, witPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind wit -canonical-abi failed: %v\n%s", err, out)
	}

	// Call every wrapper: an uncalled one still type-checks, but keeping the
	// calls means the flat signatures also have to survive to the linker.
	userPath := filepath.Join(outDir, "use_nums.pr")
	user := `main() {
    counter := Counter.create(1u32);
    print_line("{a()} {b()} {c()} {d()} {e()} {f()} {g()} {h()} {i()} {j()} {k()} {l()}");
    print_line("{mix(1u8, 2u16, 3u32, 4u64, 5i8, 6i16, 7i32, 8i64, 9.0f32, true, 'x')}");
    print_line("{counter.bump(1u8)} {counter.value()}");
    print_line("{ready()?!} {total()?!}");
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	buildCmd := exec.Command(bin, "build", "-target", "wasm32-wasi", "-o", filepath.Join(dir, "nums.wasm"), outDir)
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build -target wasm32-wasi failed (want exit 0): %v\n%s", err, out)
	}
	if strings.Contains(string(out), "signature mismatch") {
		t.Errorf("canonical binding linked with a wasm-ld signature mismatch:\n%s", out)
	}
}

// T1660: a signature mismatch must fail the build. wasm-ld only warns and wires
// the call to a stub that executes `unreachable`, so before this the build
// reported success and the module trapped at runtime — with the trap attributed
// to the call site rather than to the declaration that was wrong.
//
// The declaration below names a real wasm_alloc.o helper with the wrong arity,
// which is the cheapest way to manufacture a genuine mismatch.
func TestWasmSignatureMismatchFailsTheBuild(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping wasm link integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	srcPath := filepath.Join(dir, "mismatch.pr")
	src := "_cabi_load_i32(i32 ptr, i32 extra) i32 `extern(\"cabi_load_i32\");\n" +
		"main() { x := _cabi_load_i32(0i32, 0i32); }\n"
	if err := os.WriteFile(srcPath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "mismatch.wasm")
	buildCmd := exec.Command(bin, "build", "-target", "wasm32-wasi", "-o", outPath, srcPath)
	out, err := buildCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("build succeeded despite a signature mismatch (want failure)\n%s", out)
	}
	if !strings.Contains(string(out), "cabi_load_i32") {
		t.Errorf("build failure does not name the mismatched symbol:\n%s", out)
	}
	// wasm-ld exits 0 here and writes the trapping module, so the rejection has
	// to clean it up — otherwise a failed build leaves a runnable artifact that
	// traps, and an incremental build would treat it as up to date.
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("a rejected link left %s behind (stat err = %v)", outPath, err)
	}
}

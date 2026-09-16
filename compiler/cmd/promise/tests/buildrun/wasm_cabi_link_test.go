package buildrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
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
// The fixture is deliberately string-only: `list<T>` would pull in
// cabi_vector_* (missing from the prebuilt wasm_alloc.o — T2128) and an
// unsigned-integer result would not compile at all (T2129). A string result is
// also the exact shape T1660 broke, since `string` is not an opaque container
// type and so took the sret path.
func TestBindWitCanonicalAbiLinksCleanForWasm(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := t.TempDir()
	witPath := filepath.Join(dir, "api.wit")
	wit := `package test:api;

interface greet {
  hello: func(name: string) -> string;
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
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	buildCmd := exec.Command(bin, "build", "-target", "wasm32-wasi", "-o", filepath.Join(dir, "greet.wasm"), outDir)
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

	dir := t.TempDir()
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

package sema

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// T2234 (T1708 phase 1): an `extern binding one of the wasm32-web reactor
// symbols internal/wasmweb registers must carry that symbol's registered
// signature — codegen defines each one in the shape a plain `extern of exactly
// that signature calls — a wasm32-web-only symbol may not survive target
// filtering for any other target, and no declaration may bind a host export.
// Each is reported where the declaration is
// written (docs/web-apps.md "One name one definition", docs/annotations.md
// "extern").

const linuxTriple = "x86_64-unknown-linux-musl"

// mismatch is the diagnostic for a mis-declared registered symbol.
func mismatch(sym string) string {
	want, _ := wasmweb.Lookup(sym)
	return fmt.Sprintf("`extern(%q) must be declared %s", sym, want)
}

func webOnly(sym string) string {
	return "`" + sym + "` exists only on wasm32-web; gate the declaration with `target(web)"
}

// errorAt returns the position of the first error containing substr.
func errorAt(t *testing.T, errs []error, substr string) (line, col int) {
	t.Helper()
	for _, e := range errs {
		var se *Error
		if errors.As(e, &se) && strings.Contains(se.Msg, substr) {
			return se.Pos.Line, se.Pos.Column
		}
	}
	t.Fatalf("expected error containing %q, got %v", substr, errs)
	return 0, 0
}

// A mis-declared extern is rejected at the annotation, not at the declaration.
func TestWasmWebExternSignatureMismatch(t *testing.T) {
	cases := []struct {
		name string
		decl string // the declaration's text before the `extern annotation
	}{
		{"wrong param type", "_sub(i64 capacity, i32 policy) i32"},
		{"too few params", "_sub(i32 capacity) i32"},
		{"too many params", "_sub(i32 capacity, i32 policy, i32 extra) i32"},
		{"wrong result", "_sub(i32 capacity, i32 policy) i64"},
		{"missing result", "_sub(i32 capacity, i32 policy)"},
		{"failable", "_sub!(i32 capacity, i32 policy) i32"},
		{"ref param", "_sub(i32& capacity, i32 policy) i32"},
		{"optional result", "_sub(i32 capacity, i32 policy) i32?"},
		{"variadic", "_sub(i32 capacity, ...i32 policy) i32"},
		{"generic", "_sub[T](i32 capacity, i32 policy) i32"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := fmt.Sprintf("%s `extern(%q);", tc.decl, wasmweb.SubscribeFunc)
			_, errs := checkSourceWithTarget(t, "main() {}\n"+line+"\n", "wasm32-web")
			gotLine, gotCol := errorAt(t, errs, mismatch(wasmweb.SubscribeFunc))
			if wantCol := strings.Index(line, "`extern"); gotLine != 2 || gotCol != wantCol {
				t.Errorf("error at %d:%d, want the annotation at 2:%d", gotLine, gotCol, wantCol)
			}
		})
	}

	// A result where the registry has none is a mismatch too.
	_, errs := checkSourceWithTarget(t, fmt.Sprintf("main() {}\n_unsub(i32 sub_id) i32 `extern(%q);\n",
		wasmweb.UnsubscribeFunc), "wasm32-web")
	expectError(t, errs, mismatch(wasmweb.UnsubscribeFunc))
}

// The symbol a declaration binds is the one codegen binds: a bare `extern
// defaults to "promise_" + name, and so does a body-less declaration with no
// annotation at all. Neither may reach a registered symbol unchecked.
func TestWasmWebExternDefaultedSymbolIsChecked(t *testing.T) {
	name := strings.TrimPrefix(wasmweb.SubscribeFunc, "promise_")

	line := name + "(i64 x) i32 `extern;"
	_, errs := checkSourceWithTarget(t, "main() {}\n"+line+"\n", "wasm32-web")
	gotLine, gotCol := errorAt(t, errs, mismatch(wasmweb.SubscribeFunc))
	if wantCol := strings.Index(line, "`extern"); gotLine != 2 || gotCol != wantCol {
		t.Errorf("bare `extern: error at %d:%d, want the annotation at 2:%d", gotLine, gotCol, wantCol)
	}

	// No annotation: the error points at the declaration.
	_, errs = checkSourceWithTarget(t, "main() {}\n"+name+"(i64 x) i32;\n", "wasm32-web")
	if gotLine, gotCol := errorAt(t, errs, mismatch(wasmweb.SubscribeFunc)); gotLine != 2 || gotCol != 0 {
		t.Errorf("unannotated: error at %d:%d, want the declaration at 2:0", gotLine, gotCol)
	}

	// A function with a body binds no symbol.
	_, errs = checkSourceWithTarget(t, "main() {}\n"+name+"(i64 x) i32 { return 0i32; }\n", "wasm32-web")
	expectNoErrorContaining(t, errs, "must be declared")
}

// Every registered signature, declared exactly as its String() spells it, is
// accepted — which also proves every type spelling in the registry resolves.
// Under the zero target nothing is filtered, so this exercises the signature
// check alone.
func TestWasmWebExternRegisteredSignaturesAccepted(t *testing.T) {
	var b strings.Builder
	b.WriteString("main() {}\n")
	for i, s := range wasmweb.Signatures() {
		fmt.Fprintf(&b, "_w%d%s `extern(%q);\n", i, s, s.Sym)
	}
	_, errs := checkSourceWithTarget(t, b.String(), "wasm32-web")
	if len(errs) > 0 {
		t.Errorf("wasm32-web: registered signatures rejected: %v\nsource:\n%s", errs, b.String())
	}
	_, errs = checkSource(t, b.String())
	if len(errs) > 0 {
		t.Errorf("zero target: registered signatures rejected: %v\nsource:\n%s", errs, b.String())
	}
}

// Parameter names never reach the ABI, so they are not part of the contract;
// and `channel[i32]` is the same type as `Channel[i32]`.
func TestWasmWebExternNamesAndAliasesAreFree(t *testing.T) {
	_, errs := checkSourceWithTarget(t, fmt.Sprintf("main() {}\n_sub(i32 cap, i32 p) i32 `extern(%q);\n",
		wasmweb.SubscribeFunc), "wasm32-web")
	expectNoErrors(t, errs)

	_, errs = checkSourceWithTarget(t, fmt.Sprintf("main() {}\n_ch(i32 id) channel[i32] `extern(%q);\n",
		wasmweb.ChannelFunc), "wasm32-web")
	expectNoErrors(t, errs)

	// A channel of the wrong element type is a different type.
	_, errs = checkSourceWithTarget(t, fmt.Sprintf("main() {}\n_ch(i32 id) Channel[int] `extern(%q);\n",
		wasmweb.ChannelFunc), "wasm32-web")
	expectError(t, errs, mismatch(wasmweb.ChannelFunc))
}

// Codegen defines the registered symbols only for wasm32-web: a declaration of
// one that survives target filtering anywhere else is rejected at the
// annotation.
func TestWasmWebExternIsWebOnly(t *testing.T) {
	sym := wasmweb.UnsubscribeFunc
	decl := func(gate string) string {
		return fmt.Sprintf("main() {}\n_unsub(i32 sub_id) `extern(%q)%s;\n", sym, gate)
	}
	cases := []struct {
		name    string
		gate    string
		triple  string
		rejects bool
	}{
		{"ungated on linux", "", linuxTriple, true},
		{"ungated on wasi", "", "wasm32-wasi", true},
		{"gated wasm on wasi", " `target(wasm)", "wasm32-wasi", true},
		{"gated web on linux is filtered", " `target(web)", linuxTriple, false},
		{"gated web on wasi is filtered", " `target(web)", "wasm32-wasi", false},
		{"gated web on web", " `target(web)", "wasm32-web", false},
		{"ungated on web", "", "wasm32-web", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := decl(tc.gate)
			_, errs := checkSourceWithTarget(t, src, tc.triple)
			if !tc.rejects {
				expectNoErrors(t, errs)
				return
			}
			gotLine, gotCol := errorAt(t, errs, webOnly(sym))
			line := strings.Split(src, "\n")[1]
			if wantCol := strings.Index(line, "`extern"); gotLine != 2 || gotCol != wantCol {
				t.Errorf("error at %d:%d, want the annotation at 2:%d", gotLine, gotCol, wantCol)
			}
		})
	}

	// The zero TargetInfo filters nothing, so "survives filtering on a
	// non-web target" cannot be established and the declaration is not
	// rejected.
	_, errs := checkSource(t, decl(""))
	expectNoErrorContaining(t, errs, webOnly(sym))

	// A mis-typed declaration on a non-web target gets both diagnostics.
	_, errs = checkSourceWithTarget(t, fmt.Sprintf("main() {}\n_sub(i64 c) i32 `extern(%q);\n",
		wasmweb.SubscribeFunc), linuxTriple)
	expectError(t, errs, webOnly(wasmweb.SubscribeFunc))
	expectError(t, errs, mismatch(wasmweb.SubscribeFunc))
}

func exportBound(sym string) string {
	return "`" + sym + "` is a wasm32-web export the host calls; Promise code cannot bind it with `extern"
}

// The exports are defined in the raw shape the host calls, and a plain `extern
// lowers to the bridge shape, so a declaration naming one is rejected on every
// target — even with the export's own documented signature (docs/web-apps.md
// "Layer 1 the exported surface"), even on wasm32-web.
func TestWasmWebExternRejectsExports(t *testing.T) {
	decls := map[string]string{
		wasmweb.ExportInitialize: "_init()",
		wasmweb.ExportPump:       "_pump() i32",
		wasmweb.ExportEnqueue:    "_enq(i32 sub_id, i32 handle) i32",
	}
	if len(decls) != len(wasmweb.Exports()) {
		t.Fatalf("test covers %d exports, wasmweb has %d", len(decls), len(wasmweb.Exports()))
	}
	for _, sym := range wasmweb.Exports() {
		line := fmt.Sprintf("%s `extern(%q);", decls[sym], sym)
		src := "main() {}\n" + line + "\n"
		for _, triple := range []string{"wasm32-web", linuxTriple, ""} {
			t.Run(sym+"/"+triple, func(t *testing.T) {
				var errs []error
				if triple == "" {
					_, errs = checkSource(t, src)
				} else {
					_, errs = checkSourceWithTarget(t, src, triple)
				}
				gotLine, gotCol := errorAt(t, errs, exportBound(sym))
				if wantCol := strings.Index(line, "`extern"); gotLine != 2 || gotCol != wantCol {
					t.Errorf("error at %d:%d, want the annotation at 2:%d", gotLine, gotCol, wantCol)
				}
			})
		}
	}

	// A body-less declaration defaulting onto an export binds it too.
	_, errs := checkSourceWithTarget(t, "main() {}\nweb_pump() i32;\n", "wasm32-web")
	if gotLine, gotCol := errorAt(t, errs, exportBound(wasmweb.ExportPump)); gotLine != 2 || gotCol != 0 {
		t.Errorf("unannotated: error at %d:%d, want the declaration at 2:0", gotLine, gotCol)
	}
}

// The registry is matched by exact symbol, not by the promise_web_ prefix:
// docs/wasm-bindings.md's promise_web_console_log is an unrelated host import.
func TestWasmWebExternPrefixIsNotTheMatcher(t *testing.T) {
	_, errs := checkSourceWithTarget(t,
		"main() {}\n_log(string s, i64 n) `extern(\"promise_web_console_log\");\n", linuxTriple)
	expectNoErrors(t, errs)
}

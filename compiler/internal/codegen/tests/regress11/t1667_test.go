package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1667: codegen's resolveTypeRefToType used to be an independent copy of
// sema.resolveType — the two walked the same AST node set and had to be edited
// in lockstep (T1634 had to make the same two edits in both, and a missed edit
// gives a sema type and a codegen type that disagree on failability: a wrong
// LLVM result shape at an indirect call, silent until codegen crashes). It now
// reads back sema's recorded resolution (sema.Info.TypeRefs). These tests pin
// the IR at the call sites that consume it, so a regression fails here rather
// than as a wrong shape at run time.

// --- lookupLocalType: nested optionals keep their full depth ---

// TestT1667_NestedOptionalDeclKeepsDepth — `int?? b = a;` where a is `int?`.
// The alloca must be the two-deep optional, not the one-deep type the
// initializer alone would give.
func TestT1667_NestedOptionalDeclKeepsDepth(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		probe() {
			int? a = 3;
			int?? b = a;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "probe")
	if !strings.Contains(body, "%b = alloca { i1, { i1, i64 } }") {
		t.Fatalf("expected b to be a two-deep optional alloca, got:\n%s", body)
	}
}

// --- stmt_decl.go: uninitialized `T? x;` uses the declared type ---

// TestT1667_UninitializedOptionalUsesDeclaredType — `Format? f;` has no
// initializer at all, so the declared TypeRef is the only source of the alloca
// type. Zero-init must be the structural optional, not i64.
func TestT1667_UninitializedOptionalUsesDeclaredType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		probe() {
			Format? f;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "probe")
	if !strings.Contains(body, "%f = alloca { i1, { i8*, i8* } }") {
		t.Fatalf("expected f to be an optional structural alloca, got:\n%s", body)
	}
}

// --- stmt_decl.go: structural-interface declared type ---

// TestT1667_StructuralInterfaceDeclAlloca — `Format f = 42;` must allocate the
// `{i8*, i8*}` interface value struct, not the `i64` the initializer implies.
func TestT1667_StructuralInterfaceDeclAlloca(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		probe() {
			Format f = 42;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "probe")
	if !strings.Contains(body, "%f = alloca { i8*, i8* }") {
		t.Fatalf("expected f to be a structural value struct alloca, got:\n%s", body)
	}
}

// --- expr_cast.go: non-Named cast targets (T1884) ---

// TestT1667_CastToSliceTarget — `f as! int[]`, a SliceTypeRef cast target.
// Before T1884 only NamedTypeRef targets resolved; the shared resolver keeps
// every kind working without codegen restating the walk.
func TestT1667_CastToSliceTarget(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		probe() {
			Format f = [1, 2];
			int[] v = f as! int[];
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, `@"promise_vtable_Vector[int]_as_Format"`)
}

// --- expr_cast.go / stmt_decl.go: module-qualified refs (T1462) ---

// TestT1667_QualifiedModuleTypeDecl — `mod.Iface s = concrete;`. The declared
// type must resolve to the *module's* interface (sema's resolveQualifiedType
// result), so the local is an interface view rather than collapsing to the
// concrete RHS type — which is what made maybeRegisterDrop emit a heap drop
// that double-freed the box the concrete owner also frees.
func TestT1667_QualifiedModuleTypeDecl(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "vmod", `
		type Shower `+"`structural `public"+` {
			show() string `+"`abstract"+`;
		}
	`, `
		use vmod "./vmod";

		type Concrete is vmod.Shower {
			int v;
			show() string { return "c"; }
		}

		probe() {
			Concrete c = Concrete(v: 1);
			vmod.Shower s = c;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "probe")
	if !strings.Contains(body, "%s = alloca { i8*, i8* }") {
		t.Fatalf("expected s to be a structural view alloca, got:\n%s", body)
	}
	// The view must not carry its own drop binding — only the concrete owner
	// frees the box. A drop flag for `s` is the double-free signature.
	if strings.Contains(body, "%s.dropflag") {
		t.Fatalf("the interface view s must not get its own drop binding:\n%s", body)
	}
	if !strings.Contains(body, "%c.dropflag") {
		t.Fatalf("the concrete owner c must keep its drop binding:\n%s", body)
	}
}

// --- The shadowing fix ---

// TestT1667_ShadowedNameResolvesToUserType — a user type whose name collides
// with a std type. The old codegen resolver scanned Info.ScopeOrder in insertion
// order and could take the std hit; here that produced a self-view coercion
// through an empty `promise_vtable_Task_as_Task` vtable. Sema's scope-chain
// lookup picks the user's Task, so no view coercion is emitted at all.
func TestT1667_ShadowedNameResolvesToUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Task {
			string name;
		}
		probe() {
			Task t = Task(name: "x");
		}
		main() { }
	`)
	if strings.Contains(ir, "promise_vtable_Task_as_Task") {
		t.Fatalf("a user Task shadowing std Task[T] must not emit a self-view vtable:\n%s",
			codegentest.FuncBody(t, ir, "probe"))
	}
}

// --- The T1634 regression, at the codegen end ---

// TestT1667_FailableFunctionTypeCallShape — a `!(int) -> int` local. A
// sema/codegen disagreement on the signature's CanError changes the LLVM result
// shape of the indirect call: `{ i1, i64, i8* }` (raised-flag, value, error) vs
// a bare `i64`. Both sides now read the same recorded *types.Signature.
func TestT1667_FailableFunctionTypeCallShape(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		doubler!(int x) int { return x * 2; }
		probe!() int {
			!(int) -> int f = doubler;
			return f(5)?^;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "probe")
	if !strings.Contains(body, "%f = alloca { i8*, i8* }") {
		t.Fatalf("expected f to be a closure fat-pointer alloca, got:\n%s", body)
	}
	if !strings.Contains(body, "call { i1, i64, i8* } %") {
		t.Fatalf("expected the failable indirect-call result shape { i1, i64, i8* }, got:\n%s", body)
	}
}

// TestT1667_VoidFunctionTypeCallShape — the other half of T1634: `void` in
// return position must normalize to a nil result, so the indirect call is
// `call void`, not a call returning a `void` value.
func TestT1667_VoidFunctionTypeCallShape(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		noop(int x) { }
		probe() {
			(int) -> void f = noop;
			f(5);
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "probe")
	if !strings.Contains(body, "call void %") {
		t.Fatalf("expected a void indirect call, got:\n%s", body)
	}
}

// --- New coverage this change enables: Self ---

// TestT1667_SelfDeclaredTypeInGenericBody — `Self x = ...` inside a generic
// type's method. The codegen copy had no `Self` case at all and returned nil,
// so the declared type silently fell back to the initializer's type. It now
// resolves through sema and the ambient mono substitution.
func TestT1667_SelfDeclaredTypeInGenericBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder[T] {
			T v;
			twin() Self {
				Self s = Holder[T](v: this.v);
				return s;
			}
		}
		probe() {
			h := Holder[int](v: 1);
			h2 := h.twin();
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, `define { i8*, i8* } @"Holder[int].twin"`)
}

// --- Cross-Info reads: the map lives in the unit that type-checked the ref ---

// TestT1667_ModuleOwnedDeclReadsModuleInfo — the declared types here are
// written in the *module's* source, so their records live in the module's
// sema.Info, not the user program's. Codegen swaps c.info while compiling a
// module; if the lookup ever ran against the wrong Info the ref would come back
// unrecorded, and for `Wrapper? w;` there is no initializer to fall back on —
// genTypedVarDecl returns without emitting the alloca and the next use dies
// with "undefined variable" (the T1692 failure mode).
func TestT1667_ModuleOwnedDeclReadsModuleInfo(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "vmod", `
		type Wrapper `+"`public"+` {
			int v;
		}
		absent() int `+"`public"+` {
			Wrapper? w;
			if found := w {
				return found.v;
			}
			return -1;
		}
	`, `
		use vmod "./vmod";
		main() { n := vmod.absent(); }
	`)
	body := codegentest.ExtractFunction(ir, "__mod_vmod_absent")
	if body == "" {
		t.Fatal("module function @__mod_vmod_absent not emitted")
	}
	if !strings.Contains(body, "%w = alloca { i1, { i8*, i8* } }") {
		t.Fatalf("the module-owned `Wrapper? w;` alloca was not emitted from the module's own Info:\n%s", body)
	}
}

// TestT1667_ModuleOwnedCastTargetReadsModuleInfo — same swap, at the cast call
// site. genCastExpr panics outright when the target ref is unrecorded, so a
// wrong-Info read here is a hard codegen crash rather than a silent miscompile.
func TestT1667_ModuleOwnedCastTargetReadsModuleInfo(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "cmod", `
		unwrap() int `+"`public"+` {
			Format f = 7;
			return f as! int;
		}
	`, `
		use cmod "./cmod";
		main() { n := cmod.unwrap(); }
	`)
	body := codegentest.ExtractFunction(ir, "__mod_cmod_unwrap")
	if body == "" {
		t.Fatal("module function @__mod_cmod_unwrap not emitted")
	}
	// The downcast compares the value struct's vtable against the int-as-Format
	// view — the shape that only exists once the `int` target ref resolved.
	if !strings.Contains(body, "@promise_vtable_int_as_Format") {
		t.Fatalf("the module-owned `f as! int` did not resolve its target:\n%s", body)
	}
	if !strings.Contains(body, "cast.panic") {
		t.Fatalf("expected the RTTI-checked downcast branches:\n%s", body)
	}
}

// TestT1667_ParamDefaultCastReadsDeclaringInfo — a trailing parameter default
// is emitted at a call site in *another* compilation unit (T1395). Here the
// dispatch is `scan[wmod.Widget]`, whose body was type-checked against the
// abstract `Parse` signature, so the concrete `extra` argument has no AST node
// at the call site and emitTrailingDefaultArgValues splices the module's
// default expression in. useDeclaringInfo swaps c.info to the module that
// declared it first; the cast target inside the default is recorded ONLY there,
// so without the swap resolveTypeRefToType comes up empty and genCastExpr
// panics with "sema recorded no type for cast target".
func TestT1667_ParamDefaultCastReadsDeclaringInfo(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "wmod", `
		type Widget `+"`public"+` {
		  int v;
		  parse!(Reader ~r, int extra = 7 as int) Widget `+"`factory `public"+` {
		    return Widget(v: 100 + extra);
		  }
		}
	`, `
		use wmod "./wmod";
		run() int {
		  wmod.Widget w = scan[wmod.Widget]("ignored")?!;
		  return w.v;
		}
		main() { run(); }
	`)
	// The spliced default resolved and folded to the constant 7 — a missing
	// trailing argument (or a wrong one) is the failure this pins.
	codegentest.AssertContainsMatch(t, ir,
		`call \{ i1, \{ i8\*, i8\* \}, i8\* \} @__mod_wmod_Widget\.parse\(\{ i8\*, i8\* \}\* %\d+, i64 7\)`)
}

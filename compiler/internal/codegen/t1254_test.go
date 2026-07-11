package codegen

import (
	"regexp"
	"testing"
)

// T1254: a capturing closure created inside a *monomorphized* body — a generic
// type's user-defined operator/method (instance-owned) or a module function
// (module-owned) — must have its lambda (and any synthesized `.env_drop` helper)
// named after, and routed into, the same compilation unit as the body that
// creates it. Before the fix the lambda was emitted as a bare top-level
// `.lambda.N` in the main IR while its creating body lived in a per-instance /
// per-module .bc, producing (a) an undefined-symbol link error on a warm cache
// (the instance body is not regenerated, so the main IR no longer defines
// `.lambda.N`), and (b) cross-object number collisions → wrong closure called.
//
// These tests lock in the structural fix: the lambda name is owner-qualified.
// Without the fix the name is a bare `.lambda.N`, so `assertContainsMatch` on the
// owner-qualified form fails.

// Instance-owned: a generic user-defined operator whose body builds a capturing
// closure. The lambda must be named `.lambda.ABox[int].N`, not a bare `.lambda.N`.
func TestT1254GenericOperatorLambdaOwnerQualified(t *testing.T) {
	ir := generateIR(t, `
type ABox[T] {
  T v;
  ^ (this, ABox[T] o)() -> T {
    a := this.v;
    return move || -> a;
  }
}
main() {
  a := ABox[int](v: 11);
  b := ABox[int](v: 22);
  () -> int f = a ^ b;
  x := f();
}
`)
	// Owner-qualified lambda definition (the fix). The number is not asserted.
	assertContainsMatch(t, ir, `define i64 @"\.lambda\.ABox\[int\]\.\d+"`)
}

// Instance-owned + droppable capture: capturing a heap string forces a
// synthesized `.env_drop` helper, which must ALSO be adopted into the instance
// unit and owner-qualified (genEnvDropFunc's adoptEnclosingCompilationUnit site).
// The Copy-typed captures in the operator test above never generate an env_drop.
func TestT1254GenericOperatorEnvDropOwnerQualified(t *testing.T) {
	ir := generateIR(t, `
type SBox[T] {
  T v;
  ^ (this, SBox[T] o)() -> T {
    a := this.v;
    return move || -> a;
  }
}
main() {
  a := SBox[string](v: "hi");
  b := SBox[string](v: "bye");
  () -> string f = a ^ b;
  s := f();
}
`)
	// Both the lambda and its env_drop helper are owner-qualified.
	assertContainsMatch(t, ir, `define i8\* @"\.lambda\.SBox\[string\]\.\d+"`)
	assertContainsMatch(t, ir, `define void @"\.lambda\.SBox\[string\]\.\d+\.env_drop"`)
}

// Instance-owned generic method (non-operator) capturing a droppable vector.
func TestT1254GenericMethodEnvDropOwnerQualified(t *testing.T) {
	ir := generateIR(t, `
type VBox[T] {
  T v;
  make(this)() -> T {
    b := this.v;
    return move || -> b;
  }
}
main() {
  a := VBox[int[]](v: [1, 2, 3]);
  () -> int[] f = a.make();
  v := f();
}
`)
	assertContainsMatch(t, ir, `@"\.lambda\.VBox\[Vector\[int\]\]\.\d+"`)
	assertContainsMatch(t, ir, `define void @"\.lambda\.VBox\[Vector\[int\]\]\.\d+\.env_drop"`)
}

// Module-owned: a plain module function whose body builds a capturing closure.
// The lambda must be named after the module compilation unit (`.lambda.clomod.N`),
// exercising the moduleOwnedFuncs branch of enclosingUnitPrefix / adopt (distinct
// from the instance branch above — std iterator closures only hit the instance one).
func TestT1254ModuleFunctionLambdaOwnerQualified(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "clomod",
		"make_adder(int base) () -> int `public { b := base + 1; return move || -> b; }",
		`
		use clomod;
		main() {
			f := clomod.make_adder(10);
			x := f();
		}
		`,
	)
	assertContainsMatch(t, ir, `define i64 @\.lambda\.clomod\.\d+`)
}

// Module-owned + droppable capture: a module function capturing a heap string —
// the env_drop helper is adopted into the module unit and module-qualified.
func TestT1254ModuleFunctionEnvDropOwnerQualified(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "clomod",
		"make_greeter(string name) () -> string `public { msg := \"hi \" + name; return move || -> msg; }",
		`
		use clomod;
		main() {
			f := clomod.make_greeter("ada");
			s := f();
		}
		`,
	)
	assertContainsMatch(t, ir, `define i8\* @\.lambda\.clomod\.\d+`)
	assertContainsMatch(t, ir, `define void @\.lambda\.clomod\.\d+\.env_drop`)
}

// T1254 (double-free face): a lambda that returns one of its env-owned droppable
// captures directly (`move || -> a` where `a` is a captured heap string) must
// CLONE the value on return, not hand back the raw captured pointer. The env
// struct retains its own copy and the env drop function frees it, so returning
// the alias would double-free (caller frees the returned value + env_drop frees
// the same allocation). The clone shows up as a `promise_string_new` call inside
// the lambda body; before the fix the body was just `load` + `ret` of the raw
// captured pointer. This was a pre-existing bug (independent of the .bc routing
// fix above) surfaced by the droppable-capture regression tests; on WASM the real
// allocator aborts with `fatal: double free`, while the host leak checker (which
// only reports positive alloc deltas) silently tolerated it.
func TestT1254ReturnedCaptureIsCloned(t *testing.T) {
	ir := generateIR(t, `
type SBox[T] {
  T v;
  ^ (this, SBox[T] o)() -> T {
    a := this.v;
    return move || -> a;
  }
}
main() {
  a := SBox[string](v: "hi");
  b := SBox[string](v: "bye");
  () -> string f = a ^ b;
  s := f();
}
`)
	// Isolate the lambda body (not the operator body, which also clones the field).
	re := regexp.MustCompile(`(?s)define i8\* @"\.lambda\.SBox\[string\]\.\d+"\(i8\* %env\) \{.*?\n\}`)
	body := re.FindString(ir)
	if body == "" {
		t.Fatalf("could not locate SBox[string] lambda body in IR")
	}
	if !regexp.MustCompile(`call i8\* @promise_string_new`).MatchString(body) {
		t.Errorf("lambda returning an env-owned string capture must clone it "+
			"(expected promise_string_new in the lambda body); got:\n%s", body)
	}
}

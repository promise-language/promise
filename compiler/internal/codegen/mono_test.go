package codegen

import (
	"sort"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// argTypeNameSet runs the pipeline on src, finds the instance whose mono name
// matches monoNameWanted, and returns the sorted set of user-type names reached
// by CollectArgTypeNames from its type arguments (T1115).
func argTypeNameSet(t *testing.T, src, monoNameWanted string) []string {
	t.Helper()
	_, info := parseWithStd(t, src)
	var inst *types.Instance
	for _, in := range CollectMonoInstances(info) {
		if MonoName(in) == monoNameWanted {
			inst = in
			break
		}
	}
	if inst == nil {
		var have []string
		for _, in := range CollectMonoInstances(info) {
			have = append(have, MonoName(in))
		}
		t.Fatalf("instance %q not found; have %v", monoNameWanted, have)
	}
	var names []string
	for _, tn := range CollectArgTypeNames(inst) {
		names = append(names, tn.Name())
	}
	sort.Strings(names)
	return names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestCollectArgTypeNames verifies the T1115 closure walk reaches every
// user-defined type whose drop/clone/method symbols a container instance's IR
// may reference — directly, transitively through fields, and through nested
// generic instances — without looping on self-referential types.
func TestCollectArgTypeNames(t *testing.T) {
	// Direct enum element: Map[int, CollideName] must reach CollideName.
	t.Run("direct enum element", func(t *testing.T) {
		src := `
			enum CollideName { S(string s, int n) }
			main() {
				m := Map[int, CollideName]();
				m[1] = CollideName.S("hi", 2);
			}
		`
		names := argTypeNameSet(t, src, "Map[int, CollideName]")
		if !containsName(names, "CollideName") {
			t.Errorf("expected CollideName in arg type names, got %v", names)
		}
	})

	// Transitive field: Map[int, Wrap] where Wrap { Inner c; } must reach both
	// Wrap and Inner — Wrap's DeclHash is identical across programs, only Inner's
	// body differs, so the field-walk to Inner is what catches the collision.
	t.Run("transitive field", func(t *testing.T) {
		src := `
			type Inner { string label; }
			type Wrap { Inner c; }
			main() {
				m := Map[int, Wrap]();
				m[1] = Wrap(c: Inner(label: "x"));
			}
		`
		names := argTypeNameSet(t, src, "Map[int, Wrap]")
		if !containsName(names, "Wrap") {
			t.Errorf("expected Wrap in arg type names, got %v", names)
		}
		if !containsName(names, "Inner") {
			t.Errorf("expected Inner (transitive via field) in arg type names, got %v", names)
		}
	})

	// Nested generic instance: Box[Box[int]] reaches Box via the instance origin.
	t.Run("nested generic instance", func(t *testing.T) {
		src := `
			type Box[T] { T value; }
			main() {
				b := Box[Box[int]](value: Box[int](value: 5));
			}
		`
		names := argTypeNameSet(t, src, "Box[Box[int]]")
		if !containsName(names, "Box") {
			t.Errorf("expected Box in arg type names, got %v", names)
		}
	})

	// Concrete user type in a generic body: Box[int] where Box references a
	// concrete Helper type (not a type argument) must still reach Helper — its
	// layout/symbols are baked into Box[int]'s IR, so a same-named/different-bodied
	// Helper in another program must invalidate the cache.
	t.Run("concrete type in generic body", func(t *testing.T) {
		src := `
			type Helper { string s; }
			type Box[T] { T value; Helper h; }
			main() {
				b := Box[int](value: 5, h: Helper(s: "x"));
			}
		`
		names := argTypeNameSet(t, src, "Box[int]")
		if !containsName(names, "Helper") {
			t.Errorf("expected Helper (concrete type in generic body) in arg type names, got %v", names)
		}
	})

	// Cycle safety: a self-referential linked-list node must not loop forever.
	t.Run("self-referential cycle safe", func(t *testing.T) {
		src := `
			type Node { Node? next; int value; }
			main() {
				m := Map[int, Node]();
				m[1] = Node(next: none, value: 7);
			}
		`
		names := argTypeNameSet(t, src, "Map[int, Node]")
		if !containsName(names, "Node") {
			t.Errorf("expected Node in arg type names, got %v", names)
		}
	})

	// Generic enum element: Map[int, Opt[Inner]] must reach BOTH the generic
	// enum origin Opt (its variant-payload drop symbols are baked into the IR)
	// AND Inner reached through the enum variant field under substitution. This
	// is the *enum origin* arm of the Instance case — and the closest analogue
	// to the original CollideName repro, which was an enum value.
	t.Run("generic enum element", func(t *testing.T) {
		src := `
			enum Opt[T] { Some(T v); None }
			type Inner { string label; }
			main() {
				m := Map[int, Opt[Inner]]();
				m[1] = Opt[Inner].Some(Inner(label: "x"));
			}
		`
		names := argTypeNameSet(t, src, "Map[int, Opt[Inner]]")
		if !containsName(names, "Opt") {
			t.Errorf("expected Opt (generic enum origin) in arg type names, got %v", names)
		}
		if !containsName(names, "Inner") {
			t.Errorf("expected Inner (via enum variant payload subst) in arg type names, got %v", names)
		}
	})

	// Array field: a fixed-size array element type must be reached through the
	// Array case so the array element's symbols invalidate the cache.
	t.Run("array element type", func(t *testing.T) {
		src := `
			type Thing { string s; }
			type Holder { Thing[3] arr; }
			main() {
				m := Map[int, Holder]();
				m[1] = Holder(arr: [Thing(s: "a"), Thing(s: "b"), Thing(s: "c")]);
			}
		`
		names := argTypeNameSet(t, src, "Map[int, Holder]")
		if !containsName(names, "Thing") {
			t.Errorf("expected Thing (via array element) in arg type names, got %v", names)
		}
	})

	// Tuple field: every tuple element type must be reached through the Tuple
	// case (both A and B contribute symbols to the layout/IR).
	t.Run("tuple element types", func(t *testing.T) {
		src := `
			type A { string s; }
			type B { int n; }
			type Pair { (A, B) p; }
			main() {
				m := Map[int, Pair]();
				m[1] = Pair(p: (A(s: "x"), B(n: 1)));
			}
		`
		names := argTypeNameSet(t, src, "Map[int, Pair]")
		if !containsName(names, "A") || !containsName(names, "B") {
			t.Errorf("expected both A and B (via tuple elements) in arg type names, got %v", names)
		}
	})

	// Function-typed field: a user type appearing in a function signature's
	// parameters/result must be reached through the Signature case.
	t.Run("signature param type", func(t *testing.T) {
		src := `
			type Arg { string s; }
			type Callback { (Arg) -> int fn; }
			main() {
				m := Map[int, Callback]();
			}
		`
		names := argTypeNameSet(t, src, "Map[int, Callback]")
		if !containsName(names, "Arg") {
			t.Errorf("expected Arg (via function-signature param) in arg type names, got %v", names)
		}
	})

	// Parent with type args: a generic parent's substituted type arguments must
	// be reached through the Instance-origin parent walk (Derived[string] is
	// Base[string] → Base's symbols participate in the cache key).
	t.Run("generic parent type args", func(t *testing.T) {
		src := `
			type Base[T] { T val; }
			type Derived[T] is Base[T] { int extra; }
			main() {
				m := Map[int, Derived[string]]();
				m[1] = Derived[string](val: "x", extra: 1);
			}
		`
		names := argTypeNameSet(t, src, "Map[int, Derived[string]]")
		if !containsName(names, "Derived") || !containsName(names, "Base") {
			t.Errorf("expected both Derived and Base (via parent type args) in arg type names, got %v", names)
		}
	})
}

// TestT0469_NoFnIterDropStubForNonIterGenerics verifies that the per-instance
// native-drop stub that delegates to __promise_iter_cleanup is generated ONLY
// for _FnIter[T] (whose layout {variant, {fn,env}, parent} matches what
// iter_cleanup walks), and NOT for other native-drop generics like Vector[T].
// A Vector instance's bytes are cap/len/buffer, not a closure+parent chain;
// the old code emitted `define void @"Vector[int].drop"` { call iter_cleanup },
// a latent corrupting stub that segfaulted once a field-drop site routed to the
// mono name (T0415's first attempt on Map._buckets).
func TestT0469_NoFnIterDropStubForNonIterGenerics(t *testing.T) {
	// A plain Vector program: Vector[int] is instantiated and dropped at scope
	// exit. No mono-named Vector drop stub may exist.
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			v.push(4);
		}
	`)
	// The origin native drop is still present and used everywhere.
	assertContains(t, ir, "define void @Vector.drop(")
	// But no per-instance mono stub that reinterprets Vector bytes as a closure.
	assertNotContainsMatch(t, ir, `define void @"Vector\[[^"]*\]\.drop"`)
}

// TestT0469_FnIterDropStubPreserved verifies the legitimate consumer is intact:
// an iterator combinator chain materializes _FnIter[T] instances whose native
// drop correctly maps to __promise_iter_cleanup, and those stubs must still be
// emitted with the iter_cleanup body.
func TestT0469_FnIterDropStubPreserved(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3, 4];
			total := 0;
			Iterator[int] it = v.iter();
			for x in it { total = total + x; }
			print_line(total.to_string());
		}
	`)
	// The _FnIter[int] instance's drop stub exists and delegates to iter_cleanup.
	stub := extractFunction(ir, `"_FnIter[int].drop"`)
	if stub == "" {
		t.Fatalf("_FnIter[int].drop stub not found in IR:\n%s", ir)
	}
	assertContains(t, stub, "call void @__promise_iter_cleanup")
}

// TestT0469_NativeGenericDropsAreTypeSpecific is the complementary guard to the
// Vector case: Channel[T], Task[T] and Ref[T] DO use their mono-mangled drop
// name (unlike Vector, which uses the origin native), but their bodies are
// produced by dedicated paths (getOrCreateChannelDrop, emitTaskJoinAndFree,
// getOrCreateArcDrop) — NEVER the FnIter stub. The pre-fix code routed every
// native generic drop through defineFnIterDrop; had that won the
// define-once race it would have stamped `call @__promise_iter_cleanup` onto a
// Channel/Task/Ref instance, reinterpreting ring-buffer / refcount bytes as a
// closure+parent chain. This locks in that none of those drops are iter-shaped.
func TestT0469_NativeGenericDropsAreTypeSpecific(t *testing.T) {
	// Channel + Task: a channel is dropped at scope exit; a `go` task handle is
	// dropped without being awaited.
	chanIR := generateIR(t, `
		main() {
			c := channel[int](2);
			c.send(1);
			c.close();
			t := go { 99 };
		}
	`)
	for _, name := range []string{`"Channel[int].drop"`, `"Task[int].drop"`} {
		drop := extractDefine(chanIR, name)
		if drop == "" {
			t.Fatalf("%s definition not found in IR:\n%s", name, chanIR)
		}
		if strings.Contains(drop, "__promise_iter_cleanup") {
			t.Errorf("%s must not be the corrupting FnIter stub, got:\n%s", name, drop)
		}
	}

	// Ref[T] keeps a declared mono stub (the Arc-family exception in the declare
	// phase) that getOrCreateArcDrop fills with a real atomic refcount decrement
	// — again never iter_cleanup.
	refIR := generateIR(t, `
		make() Ref[int] { return Ref[int](0); }
		main() {
			r := make();
			r2 := r.clone();
		}
	`)
	refDrop := extractDefine(refIR, `"Ref[int].drop"`)
	if refDrop == "" {
		t.Fatalf("Ref[int].drop definition not found in IR:\n%s", refIR)
	}
	if strings.Contains(refDrop, "__promise_iter_cleanup") {
		t.Errorf("Ref[int].drop must not be the FnIter stub, got:\n%s", refDrop)
	}
	// Sanity: it is the genuine refcounting drop (atomic decrement).
	assertContains(t, refDrop, "atomicrmw")
}

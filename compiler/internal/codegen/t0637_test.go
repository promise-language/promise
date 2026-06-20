package codegen

import (
	"strings"
	"testing"
)

// T0637: Inherited getter/setter/method from a generic parent crashed
// monomorphization at codegen because (a) resolveMethodOwner used
// LookupMethod which skips getters/setters → inherited getters/setters
// fell back to the child's own mangled name (e.g. "Child[int].fetched")
// for which no function is declared, and (b) resolveMonoParentName did
// not seed its substitution map from c.typeSubst, so inherited dispatch
// inside a mono method body resolved parent type args to unbound
// TypeParams (e.g. "Base[T].grab"). genSetterCall also did not route
// inherited setters through resolveMonoParentName, dormant under (a)
// but exposed by its fix.
//
// The fix:
//   - resolveMethodOwner walks parents via LookupAnyMethod.
//   - resolveMonoParentName seeds subst from c.typeSubst before targetType.
//   - genSetterCall uses resolveMonoParentName for inherited setters.
//
// Each test below covers one of the four reproducer shapes from the bug
// description plus the inherited-setter shapes.

// TestT0637_InheritedGenericGetterDirect — reproducer 1. A generic child
// inheriting a getter from a generic parent, accessed from concrete
// (non-mono) code. The call must route to "Base[int].fetched", which is
// already declared/defined by the existing mono pipeline.
func TestT0637_InheritedGenericGetterDirect(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] { T val; get fetched T { return this.val; } }
		type Child[T] is Base[T] {}
		main() {
			c := Child[int](val: 7);
			x := c.fetched;
		}
	`)
	// Parent's mono getter must exist and main must call it.
	if !strings.Contains(ir, `define i64 @"Base[int].fetched"(`) {
		t.Fatalf("expected @\"Base[int].fetched\" definition in IR:\n%s", ir)
	}
	if !strings.Contains(ir, `call i64 @"Base[int].fetched"(`) {
		t.Errorf("expected main to call @\"Base[int].fetched\":\n%s", ir)
	}
	// The bug used to look up @"Child[int].fetched" — make sure no such
	// declaration leaked back in.
	if strings.Contains(ir, `@"Child[int].fetched"`) {
		t.Errorf("must not reference a non-existent @\"Child[int].fetched\":\n%s", ir)
	}
}

// TestT0637_InheritedGenericGetterViaThis — reproducer 2. A generic child
// has a method that accesses an inherited generic-parent getter through
// `this`. Inside that mono method body, the receiver is the Named origin
// (Child), not an Instance — exercises both Gap 1 (LookupAnyMethod for
// the parent walk) and Gap 2 (seed subst from c.typeSubst).
func TestT0637_InheritedGenericGetterViaThis(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] { T val; get fetched T { return this.val; } }
		type Child[T] is Base[T] {
			via() T { return this.fetched; }
		}
		main() {
			c := Child[int](val: 7);
			x := c.via();
		}
	`)
	via := extractFunction(ir, `"Child[int].via"`)
	if via == "" {
		t.Fatalf("expected @\"Child[int].via\" definition:\n%s", ir)
	}
	if !strings.Contains(via, `call i64 @"Base[int].fetched"(`) {
		t.Errorf("Child[int].via must call @\"Base[int].fetched\" (not Base[T] or Child[int].fetched):\n%s", via)
	}
	if strings.Contains(ir, `@"Base[T].fetched"`) {
		t.Errorf("unbound @\"Base[T].fetched\" must not appear (Gap 2 regressed):\n%s", ir)
	}
}

// TestT0637_InheritedGenericMethodViaThis — reproducer 3. A generic child
// has a method that calls an inherited generic-parent method through
// `this`. Mirrors the getter test but for a regular method, which
// exercises the Gap 2 path on genMethodCall's resolveMonoParentName.
func TestT0637_InheritedGenericMethodViaThis(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] { T val; grab() T { return this.val; } }
		type Child[T] is Base[T] {
			via() T { return this.grab(); }
		}
		main() {
			c := Child[int](val: 7);
			x := c.via();
		}
	`)
	via := extractFunction(ir, `"Child[int].via"`)
	if via == "" {
		t.Fatalf("expected @\"Child[int].via\" definition:\n%s", ir)
	}
	if !strings.Contains(via, `call i64 @"Base[int].grab"(`) {
		t.Errorf("Child[int].via must call @\"Base[int].grab\" (not Base[T].grab):\n%s", via)
	}
	if strings.Contains(ir, `@"Base[T].grab"`) {
		t.Errorf("unbound @\"Base[T].grab\" must not appear:\n%s", ir)
	}
}

// TestT0637_InheritedGetterFromGenericParentNonGenericChild — reproducer 4.
// A non-generic child of a generic parent inheriting a getter and using
// it through `this`. The parent must still mono-resolve to Base[int] from
// the child's parent ref TypeArgs, even though the receiver itself is
// non-generic.
func TestT0637_InheritedGetterFromGenericParentNonGenericChild(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] { T val; get fetched T { return this.val; } }
		type Child is Base[int] {
			via() int { return this.fetched; }
		}
		main() {
			c := Child(val: 7);
			x := c.via();
			y := c.fetched;
		}
	`)
	if !strings.Contains(ir, `define i64 @"Base[int].fetched"(`) {
		t.Fatalf("expected @\"Base[int].fetched\" definition:\n%s", ir)
	}
	if !strings.Contains(ir, `call i64 @"Base[int].fetched"(`) {
		t.Errorf("main / Child.via must call @\"Base[int].fetched\":\n%s", ir)
	}
	if strings.Contains(ir, `@Child.fetched`) {
		t.Errorf("no @Child.fetched should exist (would be the pre-fix wrong mangling):\n%s", ir)
	}
}

// TestT0637_InheritedSetterFromGenericParent — non-generic child of a
// generic parent that owns both a getter and a setter. The setter
// assignment must route to @"Base[int].fetched$set" (mono-qualified
// parent name), not @"Child.fetched$set" (pre-fix wrong mangling) nor
// @"Base.fetched$set" (Gap 3 — genSetterCall not running through
// resolveMonoParentName).
func TestT0637_InheritedSetterFromGenericParent(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T _val;
			get fetched T { return this._val; }
			set fetched(T v) { this._val = v; }
		}
		type Child is Base[int] {}
		main() {
			c := Child(_val: 7);
			c.fetched = 9;
		}
	`)
	if !strings.Contains(ir, `define void @"Base[int].fetched$set"(`) {
		t.Fatalf("expected @\"Base[int].fetched$set\" definition:\n%s", ir)
	}
	if !strings.Contains(ir, `call void @"Base[int].fetched$set"(`) {
		t.Errorf("main must call @\"Base[int].fetched$set\":\n%s", ir)
	}
	// Pre-fix bad shapes: Gap 1 → @Child.fetched$set; Gap 3 → @Base.fetched$set.
	if strings.Contains(ir, `@Child.fetched$set`) {
		t.Errorf("must not call @Child.fetched$set (Gap 1):\n%s", ir)
	}
	if strings.Contains(ir, `@"Base.fetched$set"`) {
		t.Errorf("must not call @Base.fetched$set without mono args (Gap 3):\n%s", ir)
	}
}

// TestT0637_InheritedSetterNonGenericParent — coincidental fix from Gap 1:
// non-generic Child of non-generic Base inheriting a setter also panicked
// before the resolveMethodOwner change because LookupMethod skipped the
// setter in the parent walk. Pins it.
func TestT0637_InheritedSetterNonGenericParent(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int _val;
			get fetched int { return this._val; }
			set fetched(int v) { this._val = v; }
		}
		type Child is Base {}
		main() {
			c := Child(_val: 7);
			c.fetched = 9;
		}
	`)
	if !strings.Contains(ir, `define void @Base.fetched$set(`) {
		t.Fatalf("expected @Base.fetched$set definition:\n%s", ir)
	}
	if !strings.Contains(ir, `call void @Base.fetched$set(`) {
		t.Errorf("main must call @Base.fetched$set (inherited):\n%s", ir)
	}
	if strings.Contains(ir, `@Child.fetched$set`) {
		t.Errorf("must not call @Child.fetched$set (Gap 1 regression):\n%s", ir)
	}
}

// TestT0637_InheritedSetterOnGenericChild — generic Child of generic Base
// inheriting a setter, with the setter assignment happening on a concrete
// instance. The substitution map needs to come from the targetType
// (Instance) at the call site, but resolveMonoParentName must also work
// when c.typeSubst is empty (non-mono caller).
func TestT0637_InheritedSetterOnGenericChild(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T _val;
			get fetched T { return this._val; }
			set fetched(T v) { this._val = v; }
		}
		type Child[T] is Base[T] {}
		main() {
			c := Child[int](_val: 7);
			c.fetched = 9;
		}
	`)
	if !strings.Contains(ir, `define void @"Base[int].fetched$set"(`) {
		t.Fatalf("expected @\"Base[int].fetched$set\" definition:\n%s", ir)
	}
	if !strings.Contains(ir, `call void @"Base[int].fetched$set"(`) {
		t.Errorf("main must call @\"Base[int].fetched$set\":\n%s", ir)
	}
}

// TestT0637_InheritedSetterViaThis — strongest combination: a generic child
// of a generic parent has a method that assigns to an inherited setter
// through `this`. Exercises all three gaps together (LookupAnyMethod parent
// walk, c.typeSubst seeding in resolveMonoParentName, genSetterCall routing
// inherited setters through it) plus the `this`-receiver branch in
// genSetterCall's argument assembly.
func TestT0637_InheritedSetterViaThis(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T _val;
			get fetched T { return this._val; }
			set fetched(T v) { this._val = v; }
		}
		type Child[T] is Base[T] {
			set_via(T move v) {
				this.fetched = v;
			}
		}
		main() {
			c := Child[int](_val: 7);
			c.set_via(9);
		}
	`)
	setVia := extractFunction(ir, `"Child[int].set_via"`)
	if setVia == "" {
		t.Fatalf("expected @\"Child[int].set_via\" definition:\n%s", ir)
	}
	if !strings.Contains(setVia, `call void @"Base[int].fetched$set"(`) {
		t.Errorf("Child[int].set_via must call @\"Base[int].fetched$set\":\n%s", setVia)
	}
	// Pre-fix mismangles that the bug would produce inside the mono body:
	//  - Gap 2: parent T unresolved → @"Base[T].fetched$set"
	//  - Gap 3: setter not routed through resolveMonoParentName → @"Base.fetched$set"
	//  - Gap 1: setter inherited but child name wins → @"Child[int].fetched$set"
	if strings.Contains(ir, `@"Base[T].fetched$set"`) {
		t.Errorf("unbound @\"Base[T].fetched$set\" must not appear (Gap 2):\n%s", ir)
	}
	if strings.Contains(setVia, `@"Base.fetched$set"`) {
		t.Errorf("must not call @Base.fetched$set without mono args (Gap 3):\n%s", setVia)
	}
	if strings.Contains(setVia, `@"Child[int].fetched$set"`) {
		t.Errorf("must not call @Child[int].fetched$set (Gap 1 — setter inherited):\n%s", setVia)
	}
}

// TestT0637_TransitiveInheritedGetter — three-level inheritance, getter
// inherited from the grandparent. Exercises the *recursive* branch of
// findMonoParentName (pre-fix the immediate parent walk in
// resolveMethodOwner already failed, but once it succeeds findMonoParentName
// must walk through the intermediate Middle[T] to find Base as the owner).
func TestT0637_TransitiveInheritedGetter(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] { T val; get fetched T { return this.val; } }
		type Middle[T] is Base[T] {}
		type Child[T] is Middle[T] {}
		main() {
			c := Child[int](val: 42);
			x := c.fetched;
		}
	`)
	if !strings.Contains(ir, `define i64 @"Base[int].fetched"(`) {
		t.Fatalf("expected @\"Base[int].fetched\" definition:\n%s", ir)
	}
	if !strings.Contains(ir, `call i64 @"Base[int].fetched"(`) {
		t.Errorf("main must call @\"Base[int].fetched\" (resolved through Middle[int]):\n%s", ir)
	}
	// Pre-fix shape: parent walk found nothing for the getter and Child wins.
	if strings.Contains(ir, `@"Child[int].fetched"`) {
		t.Errorf("must not call @Child[int].fetched (Gap 1):\n%s", ir)
	}
	if strings.Contains(ir, `@"Middle[int].fetched"`) {
		t.Errorf("must not call @Middle[int].fetched (intermediate, not the definer):\n%s", ir)
	}
}

// TestT0637_TransitiveInheritedGetterViaThis — three-level inheritance plus
// via-`this` inside a Child mono method body. Combines the transitive
// recursion in findMonoParentName with Gap 2's c.typeSubst seeding.
func TestT0637_TransitiveInheritedGetterViaThis(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] { T val; get fetched T { return this.val; } }
		type Middle[T] is Base[T] {}
		type Child[T] is Middle[T] {
			via() T { return this.fetched; }
		}
		main() {
			c := Child[int](val: 42);
			x := c.via();
		}
	`)
	via := extractFunction(ir, `"Child[int].via"`)
	if via == "" {
		t.Fatalf("expected @\"Child[int].via\" definition:\n%s", ir)
	}
	if !strings.Contains(via, `call i64 @"Base[int].fetched"(`) {
		t.Errorf("Child[int].via must call @\"Base[int].fetched\" (transitive resolution):\n%s", via)
	}
	if strings.Contains(ir, `@"Base[T].fetched"`) {
		t.Errorf("unbound @\"Base[T].fetched\" must not appear (Gap 2):\n%s", ir)
	}
}

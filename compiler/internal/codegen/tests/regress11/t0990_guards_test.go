package regress11

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T0990, second half: the field-slot capture has to resolve the owner's TYPE
// before it can address the field, and it resolves each owner shape down a
// different arm of vectorFieldSlot. t0990_test.go pins the seven mutating forms
// through ONE shape (a plain `Holder` returned by a call); these pin the shapes.
//
// Two groups:
//
//   - ACCEPTED — a mutable borrow, a chained field, an inherited field, a
//     monomorphized generic owner. Each must evaluate the owner once and store the
//     relocated buffer back through the slot it read from.
//   - REJECTED — an enum owner, a failable getter, a member that is not a Vector.
//     vectorFieldSlot deliberately declines these, so they keep whatever path they
//     had before the fix; the requirement is that they still compile and never
//     write a relocated buffer into an instance field they only borrowed a copy of.

// t0990SlotRe builds a regex matching a getelementptr to field fieldIdx of the
// given LLVM instance type (e.g. `%promise_Holder_i`, `%"promise_Box[int]_i"`),
// capturing the slot register.
func t0990SlotRe(instType string, fieldIdx int) *regexp.Regexp {
	q := regexp.QuoteMeta(instType)
	return regexp.MustCompile(fmt.Sprintf(
		`(%%\d+) = getelementptr %s, %s\* %%\d+, i32 0, i32 %d\b`, q, q, fieldIdx))
}

// assertOwnerOnceStoreBackTo compiles src, then checks that fn calls @__user.<owner>
// exactly once, GEPs the named field slot exactly once, and stores a
// promise_vector_* result back into that very slot.
func assertOwnerOnceStoreBackTo(t *testing.T, src, fn, owner, instType string, fieldIdx int) {
	t.Helper()
	ir := codegentest.GenerateIR(t, src)
	body := codegentest.ExtractFunction(ir, "__user."+fn)
	if body == "" {
		t.Fatalf("expected __user.%s in IR", fn)
	}

	ownerRe := regexp.MustCompile(`@__user\.` + regexp.QuoteMeta(owner) + `\(`)
	if n := len(ownerRe.FindAllString(body, -1)); n != 1 {
		t.Errorf("%s: expected exactly 1 call to @__user.%s, got %d:\n%s", fn, owner, n, body)
	}

	slots := t0990SlotRe(instType, fieldIdx).FindAllStringSubmatch(body, -1)
	if len(slots) != 1 {
		t.Fatalf("%s: expected exactly 1 getelementptr to %s field %d, got %d:\n%s",
			fn, instType, fieldIdx, len(slots), body)
	}
	slot := slots[0][1]

	for _, m := range t0990VectorCall.FindAllStringSubmatch(body, -1) {
		if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", m[1], slot)) {
			return
		}
	}
	t.Errorf("%s: no promise_vector_* result stored back into the read slot %s:\n%s", fn, slot, body)
}

// TestT0990_MutRefOwnerSingleEvalStoreBack — a mutable-borrow owner (`Holder ~h`)
// has type MutRef[Holder], so the guard must peel the borrow (T0381) before it can
// find the layout; without the peel it would decline and fall back to the
// recompute. The borrow is a stable pointer, so the requirement is the same one:
// address the field once and store the grown buffer back through that address.
func TestT0990_MutRefOwnerSingleEvalStoreBack(t *testing.T) {
	for _, stmt := range []string{
		`h.items.push(9);`,
		`h.items.pop();`,
		`h.items.remove(0);`,
		`h.items[0] = 9;`,
		`h.items[0] += 9;`,
		`h.items[0]++;`,
		`h.items[0:1] = [9];`,
	} {
		ir := codegentest.GenerateIR(t, `
			type Holder { Vector[int] items; }
			mutate(Holder ~h) { `+stmt+` }
			main() { h := Holder(items: [1, 2, 3]); mutate(h); }
		`)
		body := codegentest.ExtractFunction(ir, "__user.mutate")
		if body == "" {
			t.Fatalf("%s: expected __user.mutate in IR", stmt)
		}
		slots := t0990SlotRe("%promise_Holder_i", 1).FindAllStringSubmatch(body, -1)
		if len(slots) != 1 {
			t.Fatalf("%s: expected exactly 1 getelementptr to Holder.items, got %d:\n%s",
				stmt, len(slots), body)
		}
		slot := slots[0][1]
		stored := false
		for _, m := range t0990VectorCall.FindAllStringSubmatch(body, -1) {
			if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", m[1], slot)) {
				stored = true
				break
			}
		}
		if !stored {
			t.Errorf("%s: no promise_vector_* result stored back into the borrowed field slot %s:\n%s",
				stmt, slot, body)
		}
	}
}

// TestT0990_ChainedOwnerSingleEvalStoreBack — `impure().inner.items` addresses a
// field of a field. The capture must be of the INNER field's slot (the one the
// buffer was loaded from), reached through a single evaluation of the outermost
// owner; recomputing the chain called the owner a second time and landed the grown
// buffer in a different instance's Holder.
func TestT0990_ChainedOwnerSingleEvalStoreBack(t *testing.T) {
	src := `
		type Holder { Vector[int] items; }
		type Outer { Holder inner; }
		make_outer() Outer { return Outer(inner: Holder(items: [1, 2, 3])); }
		caller() { make_outer().inner.items.push(9); }
		main() { caller(); }
	`
	assertOwnerOnceStoreBackTo(t, src, "caller", "make_outer", "%promise_Holder_i", 1)
}

// TestT0990_InheritedFieldOwnerSingleEvalStoreBack — the field is declared on the
// PARENT, so its slot index comes from the parent's layout merged into the child
// instance struct. The capture must use that merged index on the child's instance
// type, and still evaluate the impure owner once.
func TestT0990_InheritedFieldOwnerSingleEvalStoreBack(t *testing.T) {
	src := `
		type Base { Vector[int] items; }
		type Derived is Base { int tag; }
		make_derived() Derived { return Derived(items: [1, 2, 3], tag: 7); }
		caller() { make_derived().items.push(9); }
		main() { caller(); }
	`
	assertOwnerOnceStoreBackTo(t, src, "caller", "make_derived", "%promise_Derived_i", 1)
}

// TestT0990_GenericOwnerSingleEvalStoreBack — on a monomorphized owner the member's
// type is a TypeParam until `typeSubst` resolves it, so the guard's "is this member
// a Vector?" test runs against the substituted type. Both the method-call form and
// the slice-assign form are pinned: the latter is the only mutating form whose own
// codegen (genSliceAssign) also substitutes the target type, so it is where an
// unsubstituted check would go unnoticed.
func TestT0990_GenericOwnerSingleEvalStoreBack(t *testing.T) {
	for name, stmt := range map[string]string{
		"push":         `make_box().items.push(9);`,
		"index_assign": `make_box().items[0] = 9;`,
		"slice_assign": `make_box().items[0:1] = [9];`,
	} {
		src := `
			type Box[T] { Vector[T] items; }
			make_box() Box[int] { return Box[int](items: [1, 2, 3]); }
			caller() { ` + stmt + ` }
			main() { caller(); }
		`
		t.Run(name, func(t *testing.T) {
			assertOwnerOnceStoreBackTo(t, src, "caller", "make_box", `%"promise_Box[int]_i"`, 1)
		})
	}
}

// TestT0990_EnumOwnerFallsBack — an enum member is variant data, not an instance
// field, so the guard must decline before it tries to address a field on an enum
// layout. Here the member is a getter returning a clone, which the T1370 temp path
// owns; the requirement is that the pushed-into clone is NOT stored into any GEP of
// the enum's instance struct (that would alias the temp into the enum's payload).
func TestT0990_EnumOwnerFallsBack(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum ShapeKind {
			Circle(int[] xs),
			Square,

			get all Vector[int] {
				match this {
					ShapeKind.Circle(xs) => { return xs.clone(); },
					ShapeKind.Square => { return []; },
				}
			}
		}
		caller() {
			ShapeKind s = ShapeKind.Circle([1, 2, 3]);
			s.all.push(9);
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	push := t0990VectorCall.FindStringSubmatch(body)
	if push == nil {
		t.Fatalf("expected a promise_vector_* call in caller:\n%s", body)
	}
	anyGEP := regexp.MustCompile(`(%\d+) = getelementptr %promise_ShapeKind\w*,`)
	for _, m := range anyGEP.FindAllStringSubmatch(body, -1) {
		if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", push[1], m[1])) {
			t.Errorf("the getter's clone must not be stored into the enum slot %s:\n%s", m[1], body)
		}
	}
}

// TestT0990_FailableGetterOwnerFallsBack — a member read that auto-propagates an
// error is not a plain field read: capturing it would replace the read that unwraps
// the propagation. The guard declines, so the push must go to the T1370 temp slot
// (a stack alloca) and never into a FailableHolder field.
func TestT0990_FailableGetterOwnerFallsBack(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type FailableHolder {
			Vector[int] items;
			get view! Vector[int] { return this.items.clone(); }
		}
		push_it!(FailableHolder ~h) { h.view.push(9); }
		main() { }
	`)
	body := codegentest.ExtractFunction(ir, "__user.push_it")
	if body == "" {
		t.Fatalf("expected __user.push_it in IR")
	}
	pushRe := regexp.MustCompile(`(%\d+) = call i8\* @promise_vector_push\(`)
	push := pushRe.FindStringSubmatch(body)
	if push == nil {
		t.Fatalf("expected a promise_vector_push call in push_it:\n%s", body)
	}
	// The getter's returned buffer is parked in a temp alloca; that alloca is the
	// only legal store-back target here.
	tempRe := regexp.MustCompile(`extractvalue \{ i1, i8\*, i8\* \} %\d+, 1\n\s*store i8\* %\d+, i8\*\* (%\d+)\n`)
	temp := tempRe.FindStringSubmatch(body)
	if temp == nil {
		t.Fatalf("expected the getter's payload to be parked in a temp slot:\n%s", body)
	}
	if !strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", push[1], temp[1])) {
		t.Errorf("the pushed buffer must be written back to the temp slot %s:\n%s", temp[1], body)
	}
	for _, m := range t0990SlotRe("%promise_FailableHolder_i", 1).FindAllStringSubmatch(body, -1) {
		if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", push[1], m[1])) {
			t.Errorf("the failable getter's clone must not be stored into the field slot %s:\n%s", m[1], body)
		}
	}
}

// TestT0990_NonVectorMemberSliceAssignFallsBack — genSliceAssign hands EVERY
// sliceable target to the shared place read, including a member whose type is a
// user type with its own `[:]=`. The guard must decline (there is no Vector buffer
// to write back) and leave the ordinary receiver evaluation in place: the user
// setter is called, and no vector store-back is emitted.
func TestT0990_NonVectorMemberSliceAssignFallsBack(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Cell {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=(int? low, int? high, int v) { this.x = v; }
		}
		type CellHolder { Cell cell; }
		caller() {
			h := CellHolder(cell: Cell(x: 1));
			h.cell[0:1] = 9;
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"Cell.[:]="(`) {
		t.Errorf("expected the user [:]= setter to be called:\n%s", body)
	}
	if strings.Contains(body, "@promise_vector_cow(") {
		t.Errorf("a non-Vector member must not take the vector COW store-back path:\n%s", body)
	}
}

// TestT0990_GenericBodyOwnerCapturesFieldSlot — inside a monomorphized body the
// owner's type and the member's type are still TypeParams; the guard substitutes
// both before it decides. Without the substitution the owner has no resolvable
// layout, so `Box[T]`'s own methods would silently fall back to the recompute the
// fix removed. Covers a `this` owner and a generic-parameter owner, and the
// slice-assign form, whose own codegen substitutes the target type too.
func TestT0990_GenericBodyOwnerCapturesFieldSlot(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			Vector[T] items;
			grow_self(~this, T move v) { this.items.push(move v); }
		}
		grow[T](Box[T] ~b, T move v) { b.items.push(move v); }
		reslice[T](Box[T] ~b, Vector[T] move src) { b.items[0:1] = src; }
		main() {
			b := Box[int](items: [1, 2, 3]);
			grow(b, 9);
			int[] s = [9];
			reslice(b, move s);
			b.grow_self(7);
		}
	`)
	for _, fn := range []string{`"Box[int].grow_self"`, `"grow[int]"`, `"reslice[int]"`} {
		body := codegentest.ExtractFunction(ir, fn)
		if body == "" {
			t.Fatalf("expected %s in IR", fn)
		}
		slots := t0990SlotRe(`%"promise_Box[int]_i"`, 1).FindAllStringSubmatch(body, -1)
		if len(slots) != 1 {
			t.Fatalf("%s: expected exactly 1 getelementptr to Box[int].items, got %d:\n%s",
				fn, len(slots), body)
		}
		slot := slots[0][1]
		stored := false
		for _, m := range t0990VectorCall.FindAllStringSubmatch(body, -1) {
			if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", m[1], slot)) {
				stored = true
				break
			}
		}
		if !stored {
			t.Errorf("%s: no promise_vector_* result stored back into the substituted field slot %s:\n%s",
				fn, slot, body)
		}
	}
}

// TestT0990_StructuralDefaultOwnerFallsBack — inside a structural default method
// `this` has the interface's Self type, which the guard resolves to the concrete
// type before looking for a field. An interface declares getters, not fields, so
// the lookup finds none and the guard declines: the returned clone must stay on the
// T1370 temp path and must not be written into the concrete type's own field.
func TestT0990_StructuralDefaultOwnerFallsBack(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HasItems `+"`"+`structural(protocol: true) {
			get items Vector[int] `+"`"+`abstract;

			grow_default(~this) {
				this.items.push(9);
			}
		}
		type Bag is HasItems {
			Vector[int] data;
			get items Vector[int] { return this.data.clone(); }
		}
		main() {
			b := Bag(data: [1, 2, 3]);
			b.grow_default();
		}
	`)
	body := codegentest.ExtractFunction(ir, "Bag.grow_default")
	if body == "" {
		t.Fatalf("expected Bag.grow_default in IR")
	}
	pushRe := regexp.MustCompile(`(%\d+) = call i8\* @promise_vector_push\(`)
	push := pushRe.FindStringSubmatch(body)
	if push == nil {
		t.Fatalf("expected a promise_vector_push in the synthesized default:\n%s", body)
	}
	for _, m := range t0990SlotRe("%promise_Bag_i", 1).FindAllStringSubmatch(body, -1) {
		if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", push[1], m[1])) {
			t.Errorf("the getter's clone must not be stored into Bag's own field slot %s:\n%s", m[1], body)
		}
	}
}

// TestT0990_ModuleGetterOwnerFallsBack — `mod.property` is a module-qualified
// getter, not a field on an instance: there is nothing to address, so the guard
// declines and the returned Vector stays on the temp path. Pins that a module
// getter still compiles through every mutating form's shared place read.
func TestT0990_ModuleGetterOwnerFallsBack(t *testing.T) {
	ir := codegentest.GenerateIRWithCatalogModule(t, "cfg", `
		get flags Vector[int] `+"`"+`public {
			return [1, 2, 3];
		}
	`, `
		use cfg;
		caller() { cfg.flags.push(9); }
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@promise_vector_push(") {
		t.Errorf("expected the module getter's vector to be pushed:\n%s", body)
	}
	// Nothing to address: the module getter must not produce an instance-field GEP.
	if regexp.MustCompile(`getelementptr %promise_\w+_i,`).MatchString(body) {
		t.Errorf("a module getter has no instance field to address:\n%s", body)
	}
}

// TestT0990_ReadOnlyMethodOnFieldReadsSlotOnce — every Vector method call routes
// its receiver through the same evaluation, so the field-slot capture now also
// serves READ-ONLY methods (`contains`, `clone`, …) on a field. Those have no
// store-back, so the requirement is narrower but still load-bearing: address the
// field once, read the real buffer (not a clone of it), and leave it alone — a copy
// here would be a leak, since a read has no owner to hand it to. Run through both a
// mutable and a shared borrow of the owner.
func TestT0990_ReadOnlyMethodOnFieldReadsSlotOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { Vector[int] items; }
		read_owned(Holder ~h) bool { return h.items.contains(2); }
		read_shared(Holder h) bool { return h.items.contains(2); }
		main() {
			h := Holder(items: [1, 2, 3]);
			read_shared(h);
			read_owned(h);
		}
	`)
	for _, fn := range []string{"__user.read_owned", "__user.read_shared"} {
		body := codegentest.ExtractFunction(ir, fn)
		if body == "" {
			t.Fatalf("expected %s in IR", fn)
		}
		slots := t0990SlotRe("%promise_Holder_i", 1).FindAllStringSubmatch(body, -1)
		if len(slots) != 1 {
			t.Fatalf("%s: expected exactly 1 getelementptr to Holder.items, got %d:\n%s",
				fn, len(slots), body)
		}
		if !strings.Contains(body, "load i8*, i8** "+slots[0][1]) {
			t.Errorf("%s: the contains() receiver must be loaded from the field slot %s:\n%s",
				fn, slots[0][1], body)
		}
		// A read borrows the field; it must not shallow-copy it (that would leak).
		if strings.Contains(body, "@promise_vector_dup(") || strings.Contains(body, "@Vector.clone(") {
			t.Errorf("%s: a read-only method must borrow the field, not copy it:\n%s", fn, body)
		}
	}
}

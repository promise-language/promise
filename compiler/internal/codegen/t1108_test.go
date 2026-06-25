package codegen

import "testing"

// T1108: An inline enum-constructor temp with a droppable payload passed to a
// BORROW param must be tracked for statement-end cleanup regardless of where the
// payload came from (ident move, cast, or call result). Previously the temp was
// suppressed whenever the payload was *moved* into the variant (the old
// movedDroppable gate), leaking the payload when the enum was only borrowed.

// Call-result payload (`make_resource()`) moved into an enum temp passed to a
// borrow param must be dropped at statement end.
func TestEnumCtorTempCallResultBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		make_resource() Resource { return Resource(name: "x"); }
		consume(Holder h) {}
		test() {
			consume(Holder.Has(r: make_resource()));
		}
	`)
	assertContains(t, ir, "enum.ctor.drop")
	assertContains(t, ir, "call void @Holder.drop")
}

// Ident-move payload (`move r`) into an enum temp passed to a borrow param must
// be dropped at statement end — the source ident's drop flag was cleared at the
// move, so the enum temp is the payload's sole owner.
func TestEnumCtorTempIdentMoveBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		consume(Holder h) {}
		test() {
			r := Resource(name: "x");
			consume(Holder.Has(r: move r));
		}
	`)
	assertContains(t, ir, "enum.ctor.drop")
	assertContains(t, ir, "call void @Holder.drop")
}

// Move (consume) param must NOT leave the caller-side enum.ctor.drop tracked:
// the callee owns and drops the enum, so a caller statement-end drop would be a
// double-free. genCallArgsWithMutRef claims (untracks) the enum temp.
func TestEnumCtorTempMoveParamNoCallerDrop(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		make_resource() Resource { return Resource(name: "x"); }
		take(Holder move h) {}
		test() {
			take(Holder.Has(r: make_resource()));
		}
	`)
	// The callee consumes the enum; the caller must not emit a synchronous
	// enum.ctor.drop for it (that would double-free).
	assertNotContains(t, ir, "enum.ctor.drop")
}

// Cast-subject payload (`move d as! Animal`) moved into an enum temp passed to a
// borrow param must be dropped at statement end. The T1108 fix removed the
// movedDroppable suppression from the cast branch of genEnumVariantCallLayout;
// the cast subject's drop flag is cleared at the move, so the enum temp is the
// payload's sole owner and the caller must drop it.
func TestEnumCtorTempCastSubjectBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; drop(~this) {} }
		type Dog is Animal { int legs; }
		enum Pet { Owned(Animal a), None, }
		consume(Pet p) {}
		test() {
			d := Dog(name: "rex", legs: 4);
			consume(Pet.Owned(move d as! Animal));
		}
	`)
	assertContains(t, ir, "enum.ctor.drop")
	assertContains(t, ir, "call void @Pet.drop")
}

// Cast-subject payload moved into an enum temp passed to a MOVE param must NOT
// leave a caller-side enum.ctor.drop tracked — the callee owns and drops the
// enum, so a caller statement-end drop would be a double-free.
func TestEnumCtorTempCastSubjectMoveParamNoCallerDrop(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; drop(~this) {} }
		type Dog is Animal { int legs; }
		enum Pet { Owned(Animal a), None, }
		take(Pet move p) {}
		test() {
			d := Dog(name: "rex", legs: 4);
			take(Pet.Owned(move d as! Animal));
		}
	`)
	assertNotContains(t, ir, "enum.ctor.drop")
}

// T1108/T1106: a `go`-call with an inline enum-constructor temp carrying a
// droppable payload must NOT emit a caller-side synchronous enum.ctor.drop — the
// goroutine may reference the payload after the spawning statement returns, so a
// statement-end drop would be a use-after-free. The enum temp is truncated from
// the caller's tracking; its ownership is left to pre-existing handling (T1106).
func TestEnumCtorTempGoCallNoSyncDrop(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		make_resource() Resource { return Resource(name: "x"); }
		consume(Holder h) {}
		test() {
			go consume(Holder.Has(r: make_resource()));
		}
	`)
	assertNotContains(t, ir, "enum.ctor.drop")
}

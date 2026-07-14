package ownership

import "testing"

// T1093: Regression tests asserting that a heap user type narrowed as an enum
// variant field is sema-rejected on escape. T1011's codegen correctness for
// heap user types relies entirely on checkFieldMoveOwnership emitting
// "cannot move field" — there is no dupHeapFieldForEscape path for heap user
// types (only string/vector/channel). These tests pin all four escape shapes
// so a future change to checkFieldMoveOwnership or dupHeapFieldForEscape
// cannot silently reopen the UAF.
//
// Contrast: string/vector/channel variant fields ARE allowed to escape because
// isAutoDupType covers them — codegen dups via dupString/dupVector/dupChannel.
// Heap user types (explicit or synthesised drop, not autoDup) must be cloned
// explicitly by the caller via .clone(); in the narrowed-field context there
// is no clone, so the only safe outcome is a compile-time rejection.

// Mini droppable heap user type used throughout this file.
// _Res has an explicit drop, so:
//   - isDroppableType(_Res) = true
//   - isAutoDupType(_Res)   = false  → no dup path → must be rejected

// --- Return escape ---

// Returning a narrowed heap-user-type variant field out of the narrowing scope
// would alias the enum's payload; the synth enum drop frees it at scope exit →
// use-after-free. checkFieldMoveOwnership must reject it.
func TestT1093_NarrowedEnumHeapUserFieldReturnRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
enum MsgR { Has(_Res res), Empty, }
extract(MsgR e) _Res {
    if e is Has {
        return e.res;
    }
    return _Res(id: 0);
}
`)
	expectOwnerError(t, errs, "cannot move field 'res'")
}

// --- Store-to-outer escape ---

// Assigning a narrowed heap-user-type variant field to a variable that
// outlives the if-scope would alias the enum's payload.
func TestT1093_NarrowedEnumHeapUserFieldStoreOuterRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
enum MsgR { Has(_Res res), Empty, }
test() {
    MsgR e = MsgR.Has(res: _Res(id: 1));
    _Res outer = _Res(id: 0);
    if e is Has {
        outer = e.res;
    }
}
`)
	expectOwnerError(t, errs, "cannot move field 'res'")
}

// --- Consuming `~` (move) parameter escape ---

// Passing a narrowed heap-user-type variant field to a consuming (move)
// parameter hands an alias of the enum's payload to the callee, which will
// drop it; the enum's synth drop also frees it → double-free.
func TestT1093_NarrowedEnumHeapUserFieldTildeParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
enum MsgR { Has(_Res res), Empty, }
take(_Res move r) {}
test() {
    MsgR e = MsgR.Has(res: _Res(id: 1));
    if e is Has {
        take(e.res);
    }
}
`)
	expectOwnerError(t, errs, "cannot move field 'res'")
}

// --- Constructor-field arg escape ---

// Passing a narrowed heap-user-type variant field as a constructor argument
// stores an alias of the enum's payload into the new object; the enum's synth
// drop frees it while the Sink object still references it → use-after-free.
func TestT1093_NarrowedEnumHeapUserFieldCtorArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
enum MsgR { Has(_Res res), Empty, }
type Sink { _Res r; }
test() {
    MsgR e = MsgR.Has(res: _Res(id: 1));
    if e is Has {
        sink := Sink(r: e.res);
    }
}
`)
	expectOwnerError(t, errs, "cannot move field 'res'")
}

// NOTE (T1270): a GENERIC enum instantiation like Maybe[_Res] is NOT currently
// rejected by checkFieldMoveOwnership because isDroppableOwner does not handle
// *types.Instance with *types.Enum origin. Only bare *types.Enum (non-generic
// enums) are covered. A dedicated bug T1270 tracks the fix. When T1270 is
// resolved, add TestT1093_NarrowedGenericEnumHeapUserFieldReturnRejected here:
//
//   func TestT1093_NarrowedGenericEnumHeapUserFieldReturnRejected(t *testing.T) {
//     errs := ownerErrs(t, `
//       type _Res { int id; drop(~this) {} }
//       enum Maybe[T] { Some(T val), None, }
//       extract(Maybe[_Res] o) _Res {
//           if o is Some { return o.val; }
//           return _Res(id: 0);
//       }
//     `)
//     expectOwnerError(t, errs, "cannot move field 'val'")
//   }

// --- Negative: string field is auto-dup'd, not rejected ---

// A string variant field escaping the narrowing scope is auto-dup'd by
// isAutoDupType (via dupString), so checkFieldMoveOwnership returns early
// and no error is emitted. Guards that the isAutoDupType carve-out is not
// accidentally widened to cover heap user types.
func TestT1093_NarrowedEnumStringFieldOK(t *testing.T) {
	ownerOK(t, `
enum MsgStr { Text(string body), Empty, }
extract(MsgStr m) string {
    if m is Text {
        return m.body;
    }
    return "";
}
`)
}

package codegen

import (
	"fmt"
	"reflect"
	"testing"
	"unsafe"

	"github.com/llir/llvm/ir"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1588: compilerState is the snapshot the compiler takes when it has to
// generate a nested function body in the middle of another one (lazy
// structural-default synthesis, lambdas, coroutines). Every per-function field
// MUST round-trip through saveState/restoreState — a field that is captured but
// never written back (or never captured at all) is silently lost, which is
// exactly how T1588 dropped mutRefPtrs and panicked with `undefined variable`.
// The mirror failure is a per-function field that no define* entry resets and
// that saveState does not clear: the nested body silently INHERITS it, which is
// how the same synthesis inside a `go {}` / generator body emitted branches to
// the enclosing coroutine's blocks. Fields in that second group are cleared by
// saveState, so this test still round-trips them (it snapshots the markers
// before saveState runs).
//
// This test enforces the round-trip mechanically: fill every compilerState-
// carried field on a Compiler with a distinct non-zero value, saveState, zero
// the Compiler's fields, restoreState, and require every field to come back. A
// missing restoreState line fails directly; a missing saveState line fails via
// the same round-trip.
func TestCompilerStateRoundTrip(t *testing.T) {
	st := reflect.TypeOf(compilerState{})
	cv := reflect.ValueOf(&Compiler{}).Elem()
	ct := cv.Type()

	// Every compilerState field must name a Compiler field of the same type —
	// otherwise save/restore could not be assigning the field it claims to.
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		cf, ok := ct.FieldByName(f.Name)
		if !ok {
			t.Fatalf("compilerState.%s has no matching Compiler field", f.Name)
		}
		if cf.Type != f.Type {
			t.Fatalf("compilerState.%s is %s but Compiler.%s is %s", f.Name, f.Type, f.Name, cf.Type)
		}
	}

	c := &Compiler{}
	v := reflect.ValueOf(c).Elem()
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		fv := settableField(v, name)
		marker := distinctValue(fv.Type(), i+1)
		if !marker.IsValid() {
			t.Fatalf("no distinct marker value for compilerState.%s (%s) — extend distinctValue", name, fv.Type())
		}
		fv.Set(marker)
	}

	// Copy, don't alias: settableField returns a live handle on the field, which
	// would read back as zero after the wipe below and make the test vacuous.
	want := map[string]reflect.Value{}
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		fv := settableField(v, name)
		cp := reflect.New(fv.Type()).Elem()
		cp.Set(fv)
		want[name] = cp
	}

	saved := c.saveState()

	// Wipe every carried field, the way a define* entry point does.
	for i := 0; i < st.NumField(); i++ {
		fv := settableField(v, st.Field(i).Name)
		fv.Set(reflect.Zero(fv.Type()))
	}

	c.restoreState(saved)

	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		got := settableField(v, name)
		if !sameValue(got, want[name]) {
			t.Errorf("compilerState.%s did not survive saveState/restoreState "+
				"(got %s, want %s) — add it to both saveState and restoreState (T1588)",
				name, describe(got), describe(want[name]))
		}
	}
}

// settableField returns an addressable, settable handle on an unexported
// Compiler field. reflect refuses to Set unexported fields even from inside the
// package, so re-derive the value from its address.
func settableField(v reflect.Value, name string) reflect.Value {
	f := v.FieldByName(name)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
}

// describe renders a field value in a form that distinguishes nil from empty
// (a nil map and an empty map both print as "map[]" under %v).
func describe(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Map, reflect.Slice, reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return "<nil>"
		}
		return fmt.Sprintf("non-nil %s", v.Type())
	}
	return fmt.Sprintf("%v", v)
}

// distinctValue builds a non-zero value of typ, seeded by n so no two fields of
// the same type share a value — a save/restore pair that crosses two fields is
// then caught. Bools are the exception: there is no distinct non-zero bool, so a
// swap between two bool fields is not detectable this way.
func distinctValue(typ reflect.Type, n int) reflect.Value {
	switch typ.Kind() {
	case reflect.Bool:
		return reflect.ValueOf(true)
	case reflect.Int:
		return reflect.ValueOf(n).Convert(typ)
	case reflect.Map:
		return reflect.MakeMapWithSize(typ, n)
	case reflect.Slice:
		return reflect.MakeSlice(typ, n, n)
	case reflect.Ptr:
		return reflect.New(typ.Elem())
	case reflect.Interface:
		// A fresh allocation per field, so every field gets a distinct pointer.
		for _, cand := range []reflect.Type{
			reflect.TypeOf((*types.Named)(nil)),
			reflect.TypeOf((*ast.BinaryExpr)(nil)),
			reflect.TypeOf((*ir.InstAlloca)(nil)),
		} {
			if cand.Implements(typ) {
				return reflect.New(cand.Elem())
			}
		}
		return reflect.Value{}
	case reflect.Array:
		a := reflect.New(typ).Elem()
		if typ.Elem().Kind() != reflect.Int {
			return reflect.Value{}
		}
		for i := 0; i < typ.Len(); i++ {
			a.Index(i).SetInt(int64(n*10 + i))
		}
		return a
	}
	return reflect.Value{}
}

// sameValue compares two field values by identity where identity is what
// matters (maps, slices, pointers all round-trip by reference).
func sameValue(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Map, reflect.Slice, reflect.Ptr:
		return a.Pointer() == b.Pointer() && a.IsNil() == b.IsNil()
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return a.Elem().Kind() == reflect.Ptr && b.Elem().Kind() == reflect.Ptr &&
			a.Elem().Pointer() == b.Elem().Pointer()
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}

// clearedBySaveState is the set of compilerState-carried fields saveState must
// ZERO after snapshotting them, because a nested body must not inherit them from
// the enclosing one. It is the second half of the T1588 invariant, and
// TestCompilerStateRoundTrip cannot see it: round-tripping only proves the
// enclosing body gets its value BACK, which stays true whether or not the nested
// body was handed a stale copy in between.
//
// Both real T1588-family regressions live here, not in the round-trip:
//   - dropping the mutRefPtrs group let a view-method adapter's default-argument
//     expression resolve an identifier against the ENCLOSING function's mut-ref
//     parameter, emitting `load ... %param` for a %param of another function;
//   - dropping the coroutine/generator/`go! {}` group let a lazily synthesized
//     structural default emit `br label %cleanup` / `%final.suspend` into blocks
//     the enclosing coroutine owns.
//
// Anything added to compilerState is per-function by definition, so the default
// answer for a new field is "clear it too". A field legitimately meant to carry
// INTO the nested body (fn/block/locals are rebuilt by the caller; typeSubst,
// monoCtx and selfSubst deliberately stay in force so the nested body
// monomorphizes under the same substitution) belongs in carriedIntoNestedBody
// with the reason written down.
var clearedBySaveState = []string{
	// T1029
	"discardedExpr", "discardAliasArgPtrs",
	// T0262
	"panicExitBlock", "coroutineReturnBlock",
	// T1590: coroutine / generator / `go! {}` body flags
	"inCoroutine", "coroCleanupBlk", "coroSuspendBlk",
	"inGenerator", "generatorCanError", "generatorYieldSlot", "generatorErrorSlot",
	"generatorCoroId", "generatorCleanup", "generatorSuspend", "generatorFinalSuspend",
	"inFailableGoBlock", "failableGoBlockAggType", "failableGoBlockFinalSuspend",
	// T1588: per-name bindings consulted by genIdentExpr ahead of `locals`
	"mutRefPtrs", "mutRefTypes", "matchBorrowedIdents", "borrowOptionalLocals",
	// T0945
	"borrowedValueParams",
	// T1329 / T1331
	"blockTempFloorStmt", "blockTempFloorHeap", "blockTempFloorEnv", "blockTempFloorEnum",
	"loopTempFloor",
}

// carriedIntoNestedBody lists the compilerState fields saveState deliberately
// leaves in force, with the reason. Everything in compilerState must be in
// exactly one of these two lists, so adding a field without deciding which
// fails the test rather than silently defaulting to "inherited".
var carriedIntoNestedBody = map[string]string{
	"fn":                   "the caller assigns c.fn to the new function right after saveState",
	"block":                "ditto — the caller opens the nested body's entry block",
	"entryBlock":           "ditto",
	"locals":               "the caller installs a fresh map (define* entries and the hand-built bodies both do)",
	"localNameCount":       "ditto",
	"dropFlags":            "ditto",
	"dropBindings":         "ditto",
	"scopeBindings":        "ditto",
	"castSubjectMatch":     "T0849: reset by every define* entry and by emitViewMethodAdapter",
	"forInHandleSlotPtr":   "T0617: ditto",
	"blockCounter":         "monotonic per module, not per function — sharing it keeps block names unique",
	"canError":             "set from the nested body's own signature by the caller",
	"currentRetType":       "ditto",
	"currentNamed":         "ditto",
	"thisRecvIsOwned":      "ditto",
	"loopScopeDepth":       "reset by the caller alongside the temp floors",
	"selfSubst":            "intentional: a structural default's `Self` must resolve to the concrete type",
	"targetType":           "expression-scoped, not function-scoped; the nested body sets its own",
	"typeSubst":            "intentional: the nested body monomorphizes under the SAME substitution",
	"monoCtx":              "intentional: ditto",
	"lambdaWritebacks":     "reset by the caller",
	"stmtTemps":            "temp lists — reset by the caller (T1460 does this explicitly)",
	"stmtTempMap":          "ditto",
	"heapTemps":            "ditto",
	"heapTempMap":          "ditto",
	"envTemps":             "ditto",
	"envTempMap":           "ditto",
	"enumCtorTemps":        "ditto",
	"mergeBoundStructFlag": "T1211: expression-scoped",
	"tempTrackingEnabled":  "set explicitly by the caller for the nested body",
	"currentOpValueParams": "T0897: operator-scoped; set by the operator define path",
}

func TestSaveStateClearsPerBodyFields(t *testing.T) {
	st := reflect.TypeOf(compilerState{})

	cleared := map[string]bool{}
	for _, n := range clearedBySaveState {
		cleared[n] = true
	}

	// Every compilerState field must be classified exactly once.
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		_, carried := carriedIntoNestedBody[name]
		switch {
		case cleared[name] && carried:
			t.Errorf("compilerState.%s is in BOTH clearedBySaveState and carriedIntoNestedBody", name)
		case !cleared[name] && !carried:
			t.Errorf("compilerState.%s is classified in neither list — decide whether saveState must "+
				"clear it for the nested body (the default for a per-function field) or whether it is "+
				"deliberately carried in, and add it to the matching list (T1588)", name)
		}
	}
	for name := range cleared {
		if _, ok := st.FieldByName(name); !ok {
			t.Errorf("clearedBySaveState names %q, which is not a compilerState field", name)
		}
	}
	for name := range carriedIntoNestedBody {
		if _, ok := st.FieldByName(name); !ok {
			t.Errorf("carriedIntoNestedBody names %q, which is not a compilerState field", name)
		}
	}

	// Fill every carried field with a distinct non-zero value, then saveState and
	// require the cleared set to have gone back to zero on the Compiler.
	c := &Compiler{}
	v := reflect.ValueOf(c).Elem()
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		fv := settableField(v, name)
		marker := distinctValue(fv.Type(), i+1)
		if !marker.IsValid() {
			t.Fatalf("no distinct marker value for compilerState.%s (%s) — extend distinctValue", name, fv.Type())
		}
		fv.Set(marker)
	}

	saved := c.saveState()

	for _, name := range clearedBySaveState {
		got := settableField(v, name)
		if !got.IsZero() {
			t.Errorf("saveState left compilerState.%s set (%s) — a body synthesized mid-statement "+
				"inherits the enclosing one's value (T1588/T1590)", name, describe(got))
		}
	}

	// Clearing must not have damaged the snapshot: restoreState still brings the
	// enclosing body's values back. (The round-trip test covers this in general;
	// asserted here so a "fix" that clears the snapshot itself fails both ways.)
	c.restoreState(saved)
	for _, name := range clearedBySaveState {
		if got := settableField(v, name); got.IsZero() {
			t.Errorf("compilerState.%s was cleared but not restored — restoreState must put the "+
				"enclosing body's value back (T1588)", name)
		}
	}
}

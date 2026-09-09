package codegen

import (
	"testing"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1940 folded the Arc/Weak/Mutex/Task/MutexGuard/Channel dispatch that had been
// copied into three genExpr-side arms into one trackNativeHandleResult, and gave
// trackUnwrappedFailableTemp — which had never had it — the same call. Two of the
// helper's lines are guards that none of those three callers can reach today, so
// no Promise program and no IR test can execute them:
//
//   - the nil guard. Each caller has already established a non-nil result, and
//     two of the three a non-nil type; the third (the ErrorHandlerExpr arm) can
//     in principle pass a nil exprType, but only if sema recorded no type for the
//     node. This half is defence in depth — ownedI8PtrResultDrop opens with the
//     same nil check — so the nil cases below hold even with the guard deleted;
//     they pin the contract, and would catch its removal from BOTH places.
//   - the string/Vector guard. Each caller reaches the helper only from the
//     `else` of its own string/Vector branch, so a string or a Vector never
//     arrives here.
//
// They are fail-closed guards against a *fourth* caller wired up in front of its
// own string/Vector branch — the failure mode would be a double free, since the
// caller's own branch would then register a second temp for the same value with
// a second drop. That is exactly the kind of line that rots unnoticed, so it is
// pinned directly rather than through the language: delete the string/Vector
// early return and the last two cases below report one temp where they want
// none.
//
// The positive control is what makes the guard cases mean anything: it proves
// this fixture *would* register a temp, so "nothing registered" in the guard
// cases is the guard acting and not the fixture failing to work. It also proves
// the two guards are not vacuous — `promise_string_drop` and `Vector.drop` are
// both present in c.funcs, so ownedI8PtrResultDrop would return a real drop
// function for a string or a Vector, and the temp count would go to 1 if the
// early return were deleted.
func TestT1940TrackNativeHandleResultGuards(t *testing.T) {
	// newFixture builds the smallest Compiler trackTempWithDrop/appendStmtTemp
	// will actually act on: an open current block, an entry block for the
	// allocas, temp tracking on, and the three non-getOrCreate drop symbols
	// ownedI8PtrResultDrop looks up by name.
	newFixture := func() (*Compiler, value.Value) {
		m := ir.NewModule()
		fn := m.NewFunc("t1940.fixture", irtypes.Void)
		entry := fn.NewBlock("entry")
		decl := func(name string) *ir.Func {
			return m.NewFunc(name, irtypes.Void, ir.NewParam("p", irtypes.I8Ptr))
		}
		c := &Compiler{
			module:              m,
			entryBlock:          entry,
			block:               entry,
			stmtTempMap:         map[value.Value]int{},
			tempTrackingEnabled: true,
			funcs: map[string]*ir.Func{
				"promise_string_drop": decl("promise_string_drop"),
				"Vector.drop":         decl("Vector.drop"),
				"MutexGuard.drop":     decl("MutexGuard.drop"),
			},
		}
		// A distinct i8* SSA value to stand in for the unwrapped handle.
		handle := entry.NewIntToPtr(constant.NewInt(irtypes.I64, 1), irtypes.I8Ptr)
		return c, handle
	}

	// Positive control — a MutexGuard[int] IS tracked, with MutexGuard.drop.
	// MutexGuard is the one handle whose drop is a plain c.funcs lookup rather
	// than a getOrCreate*Drop synthesis, so it needs no further compiler state.
	t.Run("handle_is_tracked", func(t *testing.T) {
		c, handle := newFixture()
		c.trackNativeHandleResult(handle, types.NewMutexGuard(types.TypInt))
		if len(c.stmtTemps) != 1 {
			t.Fatalf("MutexGuard[int] result: got %d stmtTemps, want 1", len(c.stmtTemps))
		}
		if got := c.stmtTemps[0].dropFunc; got != c.funcs["MutexGuard.drop"] {
			t.Errorf("MutexGuard[int] result: drop is %v, want MutexGuard.drop", got)
		}
		if idx, ok := c.stmtTempMap[handle]; !ok || idx != 0 {
			t.Errorf("MutexGuard[int] result: stmtTempMap[handle] = (%d, %v), want (0, true)", idx, ok)
		}
	})

	for _, tc := range []struct {
		name   string
		result func(value.Value) value.Value
		rt     types.Type
	}{
		{"nil_result", func(value.Value) value.Value { return nil }, types.NewMutexGuard(types.TypInt)},
		{"nil_type", func(h value.Value) value.Value { return h }, nil},
		// The fail-closed half: a caller placed ahead of its own string/Vector
		// branch would otherwise get a second temp for a value its own branch
		// tracks — a double free, not a leak.
		{"string_is_left_to_the_caller", func(h value.Value) value.Value { return h }, types.TypString},
		{"vector_is_left_to_the_caller", func(h value.Value) value.Value { return h }, types.NewVector(types.TypString)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, handle := newFixture()
			c.trackNativeHandleResult(tc.result(handle), tc.rt)
			if len(c.stmtTemps) != 0 {
				t.Fatalf("got %d stmtTemps, want 0 (%s must fall through to the caller)", len(c.stmtTemps), tc.name)
			}
			if len(c.stmtTempMap) != 0 {
				t.Errorf("got %d stmtTempMap entries, want 0", len(c.stmtTempMap))
			}
		})
	}
}

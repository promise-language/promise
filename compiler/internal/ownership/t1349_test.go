package ownership

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// taintOrigin on a borrow-receiver iter() builder seeds a taint only for a tracked
// local. A receiver root that is not a tracked local (e.g. a module-level binding)
// is deferred — taintOrigin returns "". Driven directly since Promise's ownership
// harness has no top-level value bindings to reproduce it end-to-end.
func TestT1349_TaintOriginNonLocalRoot(t *testing.T) {
	recv := types.NewParam("this", nil, types.RefNone)
	sig := types.NewSignature(recv, nil, nil, false)
	sig.SetReturnHoldsReceiver(true)

	member := &ast.MemberExpr{Target: &ast.IdentExpr{Name: "g"}, Field: "iter"}
	call := &ast.CallExpr{Callee: member}

	c := &Checker{
		info:             &sema.Info{Types: map[ast.Expr]types.Type{member: sig}},
		state:            StateMap{},
		params:           map[string]bool{},
		iterBorrowOrigin: map[string]string{},
	}

	// 'g' is not a tracked local → deferred, no taint.
	if o := c.taintOrigin(call); o != "" {
		t.Errorf("non-local root: expected no taint, got %q", o)
	}
	// Once 'g' is a tracked local, the same call seeds the taint.
	c.state["g"] = Owned
	if o := c.taintOrigin(call); o != "g" {
		t.Errorf("tracked local root: expected taint 'g', got %q", o)
	}
}

// taintOrigin's recv==nil / recvExpr==nil guard: a ReturnHoldsReceiver signature
// with no receiver, or a non-method callee, yields no taint. Both are
// unreachable-by-construction in the real pipeline (the flag is only set on
// method signatures, always called via a member/index callee) but are exercised
// directly to lock the defensive guard.
func TestT1349_TaintOriginMissingReceiver(t *testing.T) {
	// (a) ReturnHoldsReceiver but nil receiver, with a valid member callee.
	noRecvSig := types.NewSignature(nil, nil, nil, false)
	noRecvSig.SetReturnHoldsReceiver(true)
	member := &ast.MemberExpr{Target: &ast.IdentExpr{Name: "g"}, Field: "iter"}
	memberCall := &ast.CallExpr{Callee: member}

	// (b) ReturnHoldsReceiver with a receiver but a non-method (ident) callee.
	recvSig := types.NewSignature(types.NewParam("this", nil, types.RefNone), nil, nil, false)
	recvSig.SetReturnHoldsReceiver(true)
	ident := &ast.IdentExpr{Name: "f"}
	identCall := &ast.CallExpr{Callee: ident}

	c := &Checker{
		info: &sema.Info{Types: map[ast.Expr]types.Type{
			member: noRecvSig,
			ident:  recvSig,
		}},
		state:            StateMap{"g": Owned},
		params:           map[string]bool{},
		iterBorrowOrigin: map[string]string{},
	}
	if o := c.taintOrigin(memberCall); o != "" {
		t.Errorf("nil-receiver sig: expected no taint, got %q", o)
	}
	if o := c.taintOrigin(identCall); o != "" {
		t.Errorf("non-method callee: expected no taint, got %q", o)
	}
}

// methodReceiverExpr returns nil for a non-method callee (neither member nor
// generic-instantiation index).
func TestT1349_MethodReceiverExprNonMethod(t *testing.T) {
	if methodReceiverExpr(nil) != nil {
		t.Error("nil callee: expected nil receiver")
	}
	if methodReceiverExpr(&ast.IdentExpr{Name: "f"}) != nil {
		t.Error("plain ident callee: expected nil receiver")
	}
	member := &ast.MemberExpr{Target: &ast.IdentExpr{Name: "v"}, Field: "iter"}
	if got := methodReceiverExpr(member); got != member.Target {
		t.Error("member callee: expected the member target")
	}
	// Generic call `v.map[int](...)` wraps the member in an IndexExpr layer.
	if got := methodReceiverExpr(&ast.IndexExpr{Target: member}); got != member.Target {
		t.Error("index-wrapped member callee: expected the member target")
	}
}

// checkReturnBorrowsLocal must tolerate a bare `return;` (nil value) without
// dereferencing it. (The checkStmt caller already guards on Value != nil, so this
// is a defensive guard exercised directly.)
func TestT1349_CheckReturnBorrowsLocalBareReturn(t *testing.T) {
	c := &Checker{iterBorrowOrigin: map[string]string{}}
	c.checkReturnBorrowsLocal(&ast.ReturnStmt{Value: nil})
	if len(c.errors) != 0 {
		t.Errorf("bare return must not error, got %d errors", len(c.errors))
	}
}

// T1349: returning (or escaping) an iterator that borrows a local variable is a
// dangling borrow — the local is dropped at scope exit while the iterator still
// holds a raw borrow of it. Ownership must reject it at the escape site.

const t1349ErrSubstr = "cannot return iterator that borrows local"

func TestT1349_ReturnLocalIter(t *testing.T) {
	errs := ownerErrs(t, `
		mk() Iterator[int] {
		  int[] local = [1, 2, 3];
		  return local.iter();
		}
		main() {}
	`)
	expectOwnerError(t, errs, t1349ErrSubstr)
}

func TestT1349_ReturnLaunderedLocalIter(t *testing.T) {
	errs := ownerErrs(t, `
		mk() Iterator[int] {
		  int[] local = [1, 2, 3];
		  Iterator[int] y = local.iter();
		  return y;
		}
		main() {}
	`)
	expectOwnerError(t, errs, t1349ErrSubstr)
}

func TestT1349_ReturnLocalIterMapChain(t *testing.T) {
	// A `~this` combinator (map) propagates the seed taint of iter().
	errs := ownerErrs(t, `
		mk() Iterator[int] {
		  int[] local = [1, 2, 3];
		  return local.iter().map[int](|int n| -> int { return n * 2; });
		}
		main() {}
	`)
	expectOwnerError(t, errs, t1349ErrSubstr)
}

func TestT1349_ReturnInnerIterFromLambda(t *testing.T) {
	// The flat_map lambda returns an iterator borrowing a lambda-body local — the
	// segfaulting shape in the bug report.
	errs := ownerErrs(t, `
		run() int[] {
		  int[] outer = [1, 2];
		  return outer.iter().flat_map[int](|int n| -> Iterator[int] {
		    int[] inner = [n, n * 10];
		    return inner.iter();
		  }).collect();
		}
		main() {}
	`)
	expectOwnerError(t, errs, t1349ErrSubstr)
}

func TestT1349_ReturnLaunderedInnerIterFromLambda(t *testing.T) {
	// Laundering the inner iterator through a local inside the flat_map lambda must
	// still be caught — the lambda body gets its own taint map (fresh per lambda),
	// so the intra-lambda `Iterator[int] z = inner.iter(); return z;` resolves.
	errs := ownerErrs(t, `
		run() int[] {
		  int[] outer = [1, 2];
		  return outer.iter().flat_map[int](|int n| -> Iterator[int] {
		    int[] inner = [n, n * 10];
		    Iterator[int] z = inner.iter();
		    return z;
		  }).collect();
		}
		main() {}
	`)
	expectOwnerError(t, errs, t1349ErrSubstr)
}

func TestT1349_InScopeConsumeOK(t *testing.T) {
	// Binding then consuming in the same scope is not an escape.
	ownerOK(t, `
		consume() int {
		  int[] local = [1, 2, 3];
		  Iterator[int] it = local.iter();
		  int[] r = it.collect();
		  return r.len;
		}
		main() {}
	`)
}

func TestT1349_ForInLocalIterOK(t *testing.T) {
	ownerOK(t, `
		loop_it() int {
		  int[] local = [1, 2, 3];
		  int total = 0;
		  for x in local.iter() {
		    total = total + x;
		  }
		  return total;
		}
		main() {}
	`)
}

func TestT1349_ReturnCollectOK(t *testing.T) {
	// collect() breaks the chain — returning an owned Vector is safe.
	ownerOK(t, `
		mk_owned() int[] {
		  int[] local = [1, 2, 3];
		  return local.iter().map[int](|int n| -> int { return n * 2; }).collect();
		}
		main() {}
	`)
}

func TestT1349_IfBranchReturnLocalIter(t *testing.T) {
	// A `return local.iter()` nested in a statement-form if-branch is still an
	// escape — the inner return is checked directly.
	errs := ownerErrs(t, `
		mk(bool b) Iterator[int] {
		  int[] local = [1, 2, 3];
		  if b {
		    return local.iter();
		  }
		  return local.iter().map[int](|int n| -> int { return n; });
		}
		main() {}
	`)
	expectOwnerError(t, errs, t1349ErrSubstr)
}

func TestT1349_FilterChainEscape(t *testing.T) {
	// filter is another `~this` combinator — it must propagate the seed taint of
	// iter() just like map.
	errs := ownerErrs(t, `
		mk() Iterator[int] {
		  int[] local = [1, 2, 3, 4];
		  return local.iter().filter(|int n| -> bool { return n > 2; });
		}
		main() {}
	`)
	expectOwnerError(t, errs, t1349ErrSubstr)
}

func TestT1349_BareReturnOK(t *testing.T) {
	// A bare `return;` has no value — the escape check must not choke on it, and a
	// same-scope-consumed iterator over a local is fine.
	ownerOK(t, `
		f(bool b) {
		  int[] local = [1, 2, 3];
		  int[] r = local.iter().collect();
		  if b {
		    return;
		  }
		}
		main() {}
	`)
}

func TestT1349_ReturnParamIterOK(t *testing.T) {
	// Returning an iterator over a `move`-param source is the documented gap: the
	// param root is deferred to the caller, so it is not flagged here.
	ownerOK(t, `
		mk(int[] src) Iterator[int] {
		  return src.iter();
		}
		main() {}
	`)
}

func TestT1349_ReturnThisFieldIterOK(t *testing.T) {
	// Returning an iterator over a field of the (caller-owned) receiver is not a
	// local escape — the verdict is deferred to the caller.
	ownerOK(t, `
		type Wrapper {
		  int[] data;
		  items(this) Iterator[int] {
		    return this.data.iter();
		  }
		}
		main() {}
	`)
}

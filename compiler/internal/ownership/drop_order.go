package ownership

import "github.com/promise-language/promise/compiler/internal/types"

// trackDeclOrder records the declaration order of a variable and its type.
// Called when variables are declared (typed, inferred, use, destructure) and
// for function/method parameters. The order is used at scope exit to validate
// that variable-scoped borrows respect LIFO drop ordering.
func (c *Checker) trackDeclOrder(name string, typ types.Type) {
	if c.declOrder == nil {
		return
	}
	c.declOrder[name] = c.nextOrder
	c.varTypes[name] = typ
	c.nextOrder++
	// T1137: a fresh binding of this name (a new var decl, use/destructure
	// binding, or a shadowing decl in a disjoint scope) means later uses refer to
	// the NEW variable, not the previously-aliased one. Drop its pending
	// reuse candidates so `<-t` over a re-declared `t` is not mis-flagged as a
	// reuse of an earlier aliased handle — mirrors the reassignment carve-out in
	// checkAssignStmt. Candidates already flipped `reused` before this rebind stay
	// recorded in aliasHandleReuses, so genuine same-scope reuse is still caught.
	delete(c.pendingAliasLocals, name)
}

// hasDropMethod returns true if the given type has a drop(~this) method.
func hasDropMethod(typ types.Type) bool {
	if typ == nil {
		return false
	}
	switch t := typ.(type) {
	case *types.Named:
		return t.HasDrop()
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			return n.HasDrop()
		}
	}
	return false
}

// checkDropOrderSafety validates that active variable-scoped borrows at scope
// exit do not violate LIFO drop ordering.
//
// In Promise, variables are dropped in reverse declaration order (LIFO) at scope
// exit. If a variable B borrows from variable A, and A was declared AFTER B, then
// A is dropped first — while B still holds a borrow on it. If B's type has a
// drop() method, that method could access the dangling borrow.
//
// The check: for each active variable-scoped borrow, if the origin was declared
// after the borrower AND the borrower's type has drop(), report an error.
func (c *Checker) checkDropOrderSafety() {
	if c.borrows == nil || c.declOrder == nil {
		return
	}
	for _, b := range c.borrows.borrows {
		// Skip call-scoped borrows (no named borrower).
		if b.Borrower == "" {
			continue
		}
		borrowerOrder, hasBorrower := c.declOrder[b.Borrower]
		originOrder, hasOrigin := c.declOrder[b.Origin]
		if !hasBorrower || !hasOrigin {
			continue
		}
		// Safe: origin declared before borrower → origin dropped after borrower (LIFO).
		if originOrder < borrowerOrder {
			continue
		}
		// Origin declared at same position or after borrower → origin dropped
		// first or at same time. The borrower's drop() would see a dangling borrow.
		borrowerType := c.varTypes[b.Borrower]
		if borrowerType != nil && hasDropMethod(borrowerType) {
			c.errorf(b.Pos, "borrow of '%s' by '%s' violates drop ordering: '%s' is dropped before '%s'",
				b.Origin, b.Borrower, b.Origin, b.Borrower)
		}
	}
}

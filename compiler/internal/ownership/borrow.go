package ownership

import "github.com/promise-language/promise/compiler/internal/ast"

// BorrowKind represents the kind of borrow.
type BorrowKind int

const (
	BorrowNone   BorrowKind = -1   // not a borrow param
	BorrowShared BorrowKind = iota // &
	BorrowMut                      // ~
)

// Borrow represents an active borrow of a variable.
type Borrow struct {
	// Origin is the name of the root variable being borrowed.
	Origin string
	// FieldPath tracks the chain of field accesses from the origin.
	// nil means the whole variable; ["x"] means origin.x; ["x","inner"] means origin.x.inner.
	FieldPath []string
	// Kind is the borrow kind (shared or mutable).
	Kind BorrowKind
	// Borrower is the variable name holding the reference.
	// Empty string means call-scoped (expires at end of statement).
	Borrower string
	// Pos is the source position where the borrow was created.
	Pos ast.Pos
}

// BorrowSet tracks all active borrows in the current scope.
type BorrowSet struct {
	borrows []*Borrow
}

// NewBorrowSet creates an empty borrow set.
func NewBorrowSet() *BorrowSet {
	return &BorrowSet{}
}

// Add registers a new active borrow.
func (bs *BorrowSet) Add(b *Borrow) {
	if bs == nil {
		return
	}
	bs.borrows = append(bs.borrows, b)
}

// ActiveBorrowsOf returns all active borrows of the given origin variable.
func (bs *BorrowSet) ActiveBorrowsOf(name string) []*Borrow {
	if bs == nil {
		return nil
	}
	var result []*Borrow
	for _, b := range bs.borrows {
		if b.Origin == name {
			result = append(result, b)
		}
	}
	return result
}

// HasAnyBorrow returns true if the given variable has any active borrow.
func (bs *BorrowSet) HasAnyBorrow(name string) bool {
	if bs == nil {
		return false
	}
	for _, b := range bs.borrows {
		if b.Origin == name {
			return true
		}
	}
	return false
}

// ExpireCallScoped removes all call-scoped borrows (those with empty Borrower).
func (bs *BorrowSet) ExpireCallScoped() {
	if bs == nil {
		return
	}
	n := 0
	for _, b := range bs.borrows {
		if b.Borrower != "" {
			bs.borrows[n] = b
			n++
		}
	}
	bs.borrows = bs.borrows[:n]
}

// ExpireBorrower removes all borrows where the given variable is the borrower.
func (bs *BorrowSet) ExpireBorrower(name string) {
	if bs == nil {
		return
	}
	n := 0
	for _, b := range bs.borrows {
		if b.Borrower != name {
			bs.borrows[n] = b
			n++
		}
	}
	bs.borrows = bs.borrows[:n]
}

// Clone returns a deep copy of the borrow set.
func (bs *BorrowSet) Clone() *BorrowSet {
	if bs == nil {
		return NewBorrowSet()
	}
	c := &BorrowSet{
		borrows: make([]*Borrow, len(bs.borrows)),
	}
	for i, b := range bs.borrows {
		cp := *b
		c.borrows[i] = &cp
	}
	return c
}

// MergeBorrowSets conservatively merges two borrow sets (union).
// A borrow present in either set is present in the result.
func MergeBorrowSets(a, b *BorrowSet) *BorrowSet {
	if a == nil && b == nil {
		return NewBorrowSet()
	}
	result := a.Clone()
	if b == nil {
		return result
	}
	for _, bb := range b.borrows {
		if !result.containsBorrow(bb) {
			cp := *bb
			result.borrows = append(result.borrows, &cp)
		}
	}
	return result
}

// containsBorrow checks if a borrow with the same origin, field path, kind, and borrower exists.
func (bs *BorrowSet) containsBorrow(b *Borrow) bool {
	for _, existing := range bs.borrows {
		if existing.Origin == b.Origin && existing.Kind == b.Kind && existing.Borrower == b.Borrower && fieldPathsEqual(existing.FieldPath, b.FieldPath) {
			return true
		}
	}
	return false
}

// HasOverlappingBorrow returns true if the given variable+path has any active
// borrow that overlaps (same path, parent/child, or whole-variable).
func (bs *BorrowSet) HasOverlappingBorrow(name string, path []string) bool {
	if bs == nil {
		return false
	}
	for _, b := range bs.borrows {
		if b.Origin == name && pathsOverlap(b.FieldPath, path) {
			return true
		}
	}
	return false
}

// HasOverlappingMutBorrow returns true if the given variable+path has an active
// mutable borrow that overlaps.
func (bs *BorrowSet) HasOverlappingMutBorrow(name string, path []string) bool {
	if bs == nil {
		return false
	}
	for _, b := range bs.borrows {
		if b.Origin == name && b.Kind == BorrowMut && pathsOverlap(b.FieldPath, path) {
			return true
		}
	}
	return false
}

// pathsOverlap returns true if two field paths overlap. Overlap means one is a
// prefix of the other (parent/child) or they are equal. Two paths that diverge
// at the same level are disjoint (sibling fields).
// A nil path means the whole variable and overlaps with everything.
func pathsOverlap(a, b []string) bool {
	minLen := min(len(a), len(b))
	for i := range minLen {
		if a[i] != b[i] {
			return false // disjoint siblings
		}
	}
	return true // equal or one is a prefix of the other
}

// fieldPathsEqual returns true if two field paths are identical.
func fieldPathsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

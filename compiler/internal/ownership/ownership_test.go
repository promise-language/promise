package ownership

import (
	"fmt"
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
	antlr "github.com/antlr4-go/antlr/v4"
)

// --- Test helpers ---

// stdAll provides all builtin type declarations needed by tests.
var stdAll string

func init() {
	var b strings.Builder

	// Numeric types: arithmetic + comparison + unary negate + inc/dec
	for _, name := range []string{"int", "i8", "i16", "i32", "i64", "uint", "u8", "u16", "u32", "u64", "f32", "f64"} {
		fmt.Fprintf(&b, "type %s `native {\n", name)
		for _, op := range []string{"+", "-", "*", "/", "%"} {
			fmt.Fprintf(&b, "\t%s(%s other) %s `native;\n", op, name, name)
		}
		for _, op := range []string{"==", "!=", "<", ">", "<=", ">="} {
			fmt.Fprintf(&b, "\t%s(%s other) bool `native;\n", op, name)
		}
		fmt.Fprintf(&b, "\t-() %s `native;\n", name)
		fmt.Fprintf(&b, "\t++() %s `native;\n", name)
		fmt.Fprintf(&b, "\t--() %s `native;\n", name)
		// Bitwise operators for integer types only (not floats)
		if name != "f32" && name != "f64" {
			for _, op := range []string{"&", "|", "^", "<<", ">>"} {
				fmt.Fprintf(&b, "\t%s(%s other) %s `native;\n", op, name, name)
			}
			fmt.Fprintf(&b, "\t~() %s `native;\n", name)
		}
		if name != "f32" && name != "f64" {
			fmt.Fprintf(&b, "\t..(%s end) range `native;\n", name)
			fmt.Fprintf(&b, "\t..=(%s end) range `native;\n", name)
		}
		b.WriteString("\tget hash int `native;\n")
		b.WriteString("}\n")
	}

	// Bool
	b.WriteString("type bool `native {\n")
	b.WriteString("\t&&(bool other) bool `native;\n")
	b.WriteString("\t||(bool other) bool `native;\n")
	b.WriteString("\t==(bool other) bool `native;\n")
	b.WriteString("\t!=(bool other) bool `native;\n")
	b.WriteString("\t!() bool `native;\n")
	b.WriteString("\tget hash int `native;\n}\n")

	// Char
	b.WriteString("type char `native {\n")
	for _, op := range []string{"==", "!=", "<", ">", "<=", ">="} {
		fmt.Fprintf(&b, "\t%s(char other) bool `native;\n", op)
	}
	b.WriteString("\t..(char end) range `native;\n")
	b.WriteString("\t..=(char end) range `native;\n")
	b.WriteString("\tget hash int `native;\n")
	b.WriteString("}\n")

	// String (operators + methods)
	b.WriteString("type string `native {\n\tint len;\n")
	b.WriteString("\t+(string other) string `native;\n")
	for _, op := range []string{"==", "!=", "<", ">", "<=", ">="} {
		fmt.Fprintf(&b, "\t%s(string other) bool `native;\n", op)
	}
	b.WriteString("\tcontains(string sub) bool {\n")
	b.WriteString("\t\tif sub.len == 0 { return true; }\n")
	b.WriteString("\t\tif sub.len > this.len { return false; }\n")
	b.WriteString("\t\tint limit = this.len - sub.len;\n")
	b.WriteString("\t\tint i = 0;\n")
	b.WriteString("\t\twhile i <= limit {\n")
	b.WriteString("\t\t\tint j = 0;\n")
	b.WriteString("\t\t\twhile j < sub.len { if this[i + j] != sub[j] { break; } j = j + 1; }\n")
	b.WriteString("\t\t\tif j == sub.len { return true; }\n")
	b.WriteString("\t\t\ti = i + 1;\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t\treturn false;\n")
	b.WriteString("\t}\n")
	b.WriteString("\tstarts_with(string prefix) bool {\n")
	b.WriteString("\t\tif prefix.len > this.len { return false; }\n")
	b.WriteString("\t\tint i = 0;\n")
	b.WriteString("\t\twhile i < prefix.len { if this[i] != prefix[i] { return false; } i = i + 1; }\n")
	b.WriteString("\t\treturn true;\n")
	b.WriteString("\t}\n")
	b.WriteString("\tends_with(string suffix) bool {\n")
	b.WriteString("\t\tif suffix.len > this.len { return false; }\n")
	b.WriteString("\t\tint offset = this.len - suffix.len;\n")
	b.WriteString("\t\tint i = 0;\n")
	b.WriteString("\t\twhile i < suffix.len { if this[offset + i] != suffix[i] { return false; } i = i + 1; }\n")
	b.WriteString("\t\treturn true;\n")
	b.WriteString("\t}\n")
	b.WriteString("\tindex_of(string sub) int? {\n")
	b.WriteString("\t\tif sub.len == 0 { return 0; }\n")
	b.WriteString("\t\tif sub.len > this.len { return none; }\n")
	b.WriteString("\t\tint limit = this.len - sub.len;\n")
	b.WriteString("\t\tint i = 0;\n")
	b.WriteString("\t\twhile i <= limit {\n")
	b.WriteString("\t\t\tint j = 0;\n")
	b.WriteString("\t\t\twhile j < sub.len { if this[i + j] != sub[j] { break; } j = j + 1; }\n")
	b.WriteString("\t\t\tif j == sub.len { return i; }\n")
	b.WriteString("\t\t\ti = i + 1;\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t\treturn none;\n")
	b.WriteString("\t}\n")
	b.WriteString("\ttrim() string `native;\n")
	b.WriteString("\tsplit(string sep) string[] `native;\n")
	b.WriteString("\t[](int index) char `native;\n")
	b.WriteString("\t[:](int? start, int? end) string `native;\n")
	b.WriteString("\tget hash int `native;\n")
	b.WriteString("\tget is_empty bool => this.len == 0;\n}\n")

	// Containers
	b.WriteString("type Vector[T] `native {\n\tint len;\n")
	b.WriteString("\tnew(int capacity) `native;\n")
	b.WriteString("\t[](int index) T `native;\n")
	b.WriteString("\t[]=(int index, T value) `native;\n")
	b.WriteString("\t[:](int? start, int? end) T[] {\n")
	b.WriteString("\t\tint s = 0;\n")
	b.WriteString("\t\tif val := start { s = val; }\n")
	b.WriteString("\t\tint e = this.len;\n")
	b.WriteString("\t\tif val := end { e = val; }\n")
	b.WriteString("\t\tT[] result = [];\n")
	b.WriteString("\t\tint i = s;\n")
	b.WriteString("\t\twhile i < e {\n")
	b.WriteString("\t\t\tresult.push(this[i]);\n")
	b.WriteString("\t\t\ti = i + 1;\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t\treturn result;\n")
	b.WriteString("\t}\n")
	b.WriteString("\t[:]=(int? start, int? end, T[] value) {\n")
	b.WriteString("\t\tint s = 0;\n")
	b.WriteString("\t\tif val := start { s = val; }\n")
	b.WriteString("\t\tint e = this.len;\n")
	b.WriteString("\t\tif val := end { e = val; }\n")
	b.WriteString("\t\tint vi = 0;\n")
	b.WriteString("\t\tint i = s;\n")
	b.WriteString("\t\twhile i < e {\n")
	b.WriteString("\t\t\tif vi >= value.len { break; }\n")
	b.WriteString("\t\t\tthis[i] = value[vi];\n")
	b.WriteString("\t\t\tvi = vi + 1;\n")
	b.WriteString("\t\t\ti = i + 1;\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t}\n")
	b.WriteString("\tpush(T elem) `native;\n")
	b.WriteString("\tpop() T? `native;\n")
	b.WriteString("\tcontains(T elem) bool `native;\n")
	b.WriteString("\tremove(int index) `native;\n")
	b.WriteString("\tget is_empty bool => this.len == 0;\n}\n")

	b.WriteString(`enum Slot[K, V] {
	Empty,
	Tombstone,
	Used(K key, V value),
}
type map[K: Hashable + Equal, V] {
	Slot[K, V][] _buckets;
	int _count;
	new(~this) {
		this._buckets = [Slot.Empty];
		for _ in 1..16 { this._buckets.push(Slot.Empty); }
		this._count = 0;
	}
	get len int => this._count;
	get is_empty bool => this._count == 0;
	[](K key) V? {
		int cap = this._buckets.len;
		int h = key.hash % cap;
		if h < 0 { h = h + cap; }
		for {
			match this._buckets[h] {
				Slot.Empty => { return none; },
				Slot.Used(k, v) => {
					if k == key { return v; }
				},
				Slot.Tombstone => {},
			}
			h = (h + 1) % cap;
		}
	}
	[]=(K key, V value) {
		if this._count * 4 >= this._buckets.len * 3 {
			this._rehash();
		}
		int cap = this._buckets.len;
		int h = key.hash % cap;
		if h < 0 { h = h + cap; }
		for {
			match this._buckets[h] {
				Slot.Empty => {
					this._buckets[h] = Slot.Used(key: key, value: value);
					this._count = this._count + 1;
					return;
				},
				Slot.Used(k, _) => {
					if k == key {
						this._buckets[h] = Slot.Used(key: key, value: value);
						return;
					}
				},
				Slot.Tombstone => {
					this._buckets[h] = Slot.Used(key: key, value: value);
					this._count = this._count + 1;
					return;
				},
			}
			h = (h + 1) % cap;
		}
	}
	contains(K key) bool {
		int cap = this._buckets.len;
		int h = key.hash % cap;
		if h < 0 { h = h + cap; }
		for {
			match this._buckets[h] {
				Slot.Empty => { return false; },
				Slot.Used(k, _) => {
					if k == key { return true; }
				},
				Slot.Tombstone => {},
			}
			h = (h + 1) % cap;
		}
	}
	remove(K key) bool {
		int cap = this._buckets.len;
		int h = key.hash % cap;
		if h < 0 { h = h + cap; }
		for {
			match this._buckets[h] {
				Slot.Empty => { return false; },
				Slot.Used(k, _) => {
					if k == key {
						this._buckets[h] = Slot.Tombstone;
						this._count = this._count - 1;
						return true;
					}
				},
				Slot.Tombstone => {},
			}
			h = (h + 1) % cap;
		}
	}
	keys() K[] {
		K[] result = [];
		for slot in this._buckets {
			match slot {
				Slot.Used(k, _) => result.push(k),
				_ => {},
			}
		}
		return result;
	}
	values() V[] {
		V[] result = [];
		for slot in this._buckets {
			match slot {
				Slot.Used(_, v) => result.push(v),
				_ => {},
			}
		}
		return result;
	}
	clear() {
		for i in 0..this._buckets.len {
			this._buckets[i] = Slot.Empty;
		}
		this._count = 0;
	}
	_rehash() {
		Slot[K, V][] old = this._buckets;
		int new_cap = old.len * 2;
		this._buckets = [Slot.Empty];
		for _ in 1..new_cap { this._buckets.push(Slot.Empty); }
		this._count = 0;
		for slot in old {
			match slot {
				Slot.Used(k, v) => {
					this._set(k, v);
				},
				_ => {},
			}
		}
	}
	_set(K key, V value) {
		int cap = this._buckets.len;
		int h = key.hash % cap;
		if h < 0 { h = h + cap; }
		for {
			match this._buckets[h] {
				Slot.Empty => {
					this._buckets[h] = Slot.Used(key: key, value: value);
					this._count = this._count + 1;
					return;
				},
				Slot.Used(k, _) => {
					if k == key {
						this._buckets[h] = Slot.Used(key: key, value: value);
						return;
					}
				},
				Slot.Tombstone => {
					this._buckets[h] = Slot.Used(key: key, value: value);
					this._count = this._count + 1;
					return;
				},
			}
			h = (h + 1) % cap;
		}
	}
}
`)

	// Iter/Stream
	b.WriteString("type iter[T] `native {\n\tnext() T? `abstract;\n}\n")
	b.WriteString("type stream[T] `native {\n\titer() iter[T] `abstract;\n}\n")

	// Range
	b.WriteString("type range `native {\n\tint start `value;\n\tint end `value;\n\tbool inclusive `value;\n}\n")

	// Constraint interfaces
	b.WriteString("type Equal `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n")
	b.WriteString("type Hashable `structural {\n\tget hash int `abstract;\n}\n")
	b.WriteString("type Ordered is Equal `structural {\n\t<(Self other) bool `abstract;\n\t>(Self other) bool => other < this;\n\t<=(Self other) bool => !(other < this);\n\t>=(Self other) bool => !(this < other);\n}\n")

	// Hash implementation (FNV-1a) — used by genNativeHashGetter for int/bool/char types
	b.WriteString("_fnv1a_hash(int raw_bits) int {\n")
	b.WriteString("\tuint h = 0xcbf29ce484222325;\n")
	b.WriteString("\tuint prime = 0x00000100000001b3;\n")
	b.WriteString("\tuint v = raw_bits as! uint;\n")
	b.WriteString("\th = (h ^ (v & 255)) * prime;\n")
	b.WriteString("\th = (h ^ ((v >> 8) & 255)) * prime;\n")
	b.WriteString("\th = (h ^ ((v >> 16) & 255)) * prime;\n")
	b.WriteString("\th = (h ^ ((v >> 24) & 255)) * prime;\n")
	b.WriteString("\th = (h ^ ((v >> 32) & 255)) * prime;\n")
	b.WriteString("\th = (h ^ ((v >> 40) & 255)) * prime;\n")
	b.WriteString("\th = (h ^ ((v >> 48) & 255)) * prime;\n")
	b.WriteString("\th = (h ^ ((v >> 56) & 255)) * prime;\n")
	b.WriteString("\treturn h as! int;\n}\n")

	stdAll = b.String()
}

// checkOwnership parses source, runs sema, then runs ownership analysis.
// It automatically includes stdAll as std declarations.
func checkOwnership(t *testing.T, src string) []error {
	t.Helper()

	// Parse std
	stdInput := antlr.NewInputStream(stdAll)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, buildErrs := ast.Build("std.pr", stdTree)
	if len(buildErrs) > 0 {
		t.Fatalf("std AST build errors: %v", buildErrs)
	}
	for _, d := range stdFile.Decls {
		switch dd := d.(type) {
		case *ast.FuncDecl:
			dd.IsStd = true
		case *ast.TypeDecl:
			dd.IsStd = true
		case *ast.EnumDecl:
			dd.IsStd = true
		}
	}

	// Parse user
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, buildErrs := ast.Build("test.pr", tree)
	if len(buildErrs) > 0 {
		t.Fatalf("AST build errors: %v", buildErrs)
	}

	// Merge: std first, then user
	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(file.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, file.Decls...)
	file.Decls = merged

	info, semaErrs := sema.Check(file)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	return Check(file, info)
}

func ownerOK(t *testing.T, src string) {
	t.Helper()
	errs := checkOwnership(t, src)
	if len(errs) > 0 {
		t.Errorf("unexpected ownership errors: %v", errs)
	}
}

func ownerErrs(t *testing.T, src string) []error {
	t.Helper()
	return checkOwnership(t, src)
}

func expectOwnerError(t *testing.T, errs []error, substr string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return
		}
	}
	t.Errorf("expected ownership error containing %q, got %v", substr, errs)
}

// === Move tracking ===

func TestUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			string t = s;
			string u = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestUseAfterMoveInCall(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			consume(s);
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestDoubleMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			consume(s);
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestMoveInConstructor(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog {
			string name;
		}
		test() {
			string s = "Rex";
			Dog d = Dog(name: s);
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestReturnMovesOK(t *testing.T) {
	ownerOK(t, `
		foo() string {
			string s = "hi";
			return s;
		}
	`)
}

// === Copy exemption ===

func TestIntIsCopy(t *testing.T) {
	ownerOK(t, `
		test() {
			int x = 42;
			int y = x;
			int z = x;
		}
	`)
}

func TestBoolIsCopy(t *testing.T) {
	ownerOK(t, `
		test() {
			bool b = true;
			bool c = b;
			bool d = b;
		}
	`)
}

// === Resurrection ===

func TestAssignResurrects(t *testing.T) {
	ownerOK(t, `
		test() {
			string s = "hi";
			string t = s;
			s = "world";
			string u = s;
		}
	`)
}

func TestAssignResurrectsAfterCall(t *testing.T) {
	ownerOK(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			consume(s);
			s = "new";
			consume(s);
		}
	`)
}

// === Control flow ===

func TestMoveInIfBranch(t *testing.T) {
	// Conservative: moved in then-branch without else means possibly moved after.
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			bool b = true;
			if b {
				consume(s);
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestMoveInBothBranchesNoUse(t *testing.T) {
	// Moved in both branches, but no use after — should be OK.
	ownerOK(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			bool b = true;
			if b {
				consume(s);
			} else {
				consume(s);
			}
		}
	`)
}

func TestMoveInLoopBody(t *testing.T) {
	// Conservative: moved in loop body means possibly moved after.
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			while true {
				consume(s);
				break;
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Borrow conflicts (unit tests — sema doesn't yet support implicit T → T&/T~ coercion) ===

func TestBorrowConflictDetection(t *testing.T) {
	// Directly test the borrow conflict checker with constructed types.
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	// Build a signature: f(string ~a, string &b)
	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefMut),
		types.NewParam("b", types.TypString, types.RefShared),
	}, nil, false)

	// Build a CallExpr with two args both referencing "s"
	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 borrow conflict error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "cannot borrow") {
		t.Errorf("expected 'cannot borrow' error, got: %v", c.errors[0])
	}
}

func TestBorrowConflictDoubleMut(t *testing.T) {
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefMut),
		types.NewParam("b", types.TypString, types.RefMut),
	}, nil, false)

	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 borrow conflict error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "cannot borrow") {
		t.Errorf("expected 'cannot borrow' error, got: %v", c.errors[0])
	}
}

func TestMultipleSharedNoBorrowConflict(t *testing.T) {
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefShared),
		types.NewParam("b", types.TypString, types.RefShared),
	}, nil, false)

	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 0 {
		t.Errorf("expected no borrow conflict errors, got: %v", c.errors)
	}
}

// === Unsafe pointer (unit tests — sema doesn't yet support pointer value construction) ===

func TestIsPointerTypeRef(t *testing.T) {
	ptr := &ast.PointerTypeRef{}
	if !isPointerTypeRef(ptr) {
		t.Error("expected PointerTypeRef to be detected as pointer")
	}
	named := &ast.NamedTypeRef{}
	if isPointerTypeRef(named) {
		t.Error("expected NamedTypeRef to NOT be detected as pointer")
	}
}

func TestPointerCheckOutsideUnsafe(t *testing.T) {
	// Directly test the pointer check in checkTypedVarDecl.
	c := &Checker{
		state:    make(StateMap),
		inUnsafe: 0,
		info:     &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	decl := &ast.TypedVarDecl{
		Type:  &ast.PointerTypeRef{},
		Name:  "p",
		Value: &ast.IntLit{Raw: "0"},
	}
	c.checkTypedVarDecl(decl)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 pointer error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "raw pointer") {
		t.Errorf("expected 'raw pointer' error, got: %v", c.errors[0])
	}
}

func TestPointerCheckInsideUnsafe(t *testing.T) {
	// Same declaration but inside unsafe — no error.
	c := &Checker{
		state:    make(StateMap),
		inUnsafe: 1,
		info:     &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	decl := &ast.TypedVarDecl{
		Type:  &ast.PointerTypeRef{},
		Name:  "p",
		Value: &ast.IntLit{Raw: "0"},
	}
	c.checkTypedVarDecl(decl)

	if len(c.errors) != 0 {
		t.Errorf("expected no errors inside unsafe, got: %v", c.errors)
	}
}

// === Member access after move ===

func TestUseAfterMoveViaMemberAccess(t *testing.T) {
	// Accessing a member on a moved variable should still trigger use-after-move.
	// Unit test because sema doesn't resolve .length on string.
	c := &Checker{
		state: make(StateMap),
		info:  &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	// Register variable "s" as a string (non-copy) in both state and objects.
	ident1 := &ast.IdentExpr{Name: "s"}
	sVar := types.NewVar(types.Pos{}, "s", types.TypString)
	c.info.Objects[ident1] = sVar
	c.state["s"] = Owned

	// First, check and move "s" (simulating `string t = s;`).
	c.checkIdentUse(ident1)
	c.state["s"] = Moved

	// Now access s.length — the ident inside the MemberExpr should trigger error.
	ident2 := &ast.IdentExpr{Name: "s"}
	c.info.Objects[ident2] = sVar
	memberExpr := &ast.MemberExpr{Target: ident2, Field: "length"}
	c.checkExpr(memberExpr)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "use of moved variable 's'") {
		t.Errorf("expected use-after-move error for 's', got: %v", c.errors[0])
	}
}

// === Borrow conflict ordering ===

func TestBorrowConflictSharedBeforeMut(t *testing.T) {
	// Verify that the conflict is detected even when shared comes before mutable.
	// This validates the fix for Issue 1 (the `other` variable bug).
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	// Signature: f(string &a, string ~b) — shared first, then mutable.
	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefShared),
		types.NewParam("b", types.TypString, types.RefMut),
	}, nil, false)

	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 borrow conflict error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "cannot borrow") {
		t.Errorf("expected 'cannot borrow' error, got: %v", c.errors[0])
	}
	// Verify the error message mentions the correct borrow kind for the other borrow.
	if !strings.Contains(c.errors[0].Error(), "shared") {
		t.Errorf("expected error to mention 'shared', got: %v", c.errors[0])
	}
}

// === isCopyType ===

func TestIsCopyType(t *testing.T) {
	copyTypes := []types.Type{
		types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypF32, types.TypF64,
		types.TypBool, types.TypChar, types.TypNone, types.TypVoid,
		types.NewSharedRef(types.TypString),
		types.NewMutRef(types.TypString),
	}
	for _, typ := range copyTypes {
		if !isCopyType(typ) {
			t.Errorf("expected %s to be copy type", typ)
		}
	}

	moveTypes := []types.Type{
		types.TypString,
		nil,
	}
	for _, typ := range moveTypes {
		if isCopyType(typ) {
			t.Errorf("expected %v to NOT be copy type", typ)
		}
	}
}

// === Inferred var decl (:=) ===

func TestInferredVarDeclMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			s := "hi";
			consume(s);
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestInferredVarDeclCopy(t *testing.T) {
	ownerOK(t, `
		test() {
			x := 42;
			int y = x;
			int z = x;
		}
	`)
}

// === Destructure var decl ===

func TestDestructureVarDeclMove(t *testing.T) {
	errs := ownerErrs(t, `
		pair() (string, string) { return ("a", "b"); }
		test() {
			(a, b) := pair();
			string c = a;
			string d = a;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

func TestDestructureVarDeclCopy(t *testing.T) {
	ownerOK(t, `
		pair() (int, int) { return (1, 2); }
		test() {
			(a, b) := pair();
			int c = a;
			int d = a;
		}
	`)
}

// === For-in loop ===

func TestForInMoveInsideBody(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			for i in 0..3 {
				consume(s);
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestForInBindingOK(t *testing.T) {
	ownerOK(t, `
		test() {
			for i in 0..10 {
				int x = i;
			}
		}
	`)
}

// === Classic for loop ===

func TestClassicForMoveInBody(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			for int i = 0; i < 3; i += 1 {
				consume(s);
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Infinite loop ===

func TestInfiniteLoopMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			for {
				consume(s);
				break;
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Match expression ===

func TestMatchPatternBindingOK(t *testing.T) {
	ownerOK(t, `
		enum Option { Some(int value), None }
		test() {
			Option o = Option.Some(42);
			x := match o {
				Some(v) => v + 1,
				None => 0,
			};
		}
	`)
}

func TestMatchMoveInOneArm(t *testing.T) {
	errs := ownerErrs(t, `
		enum Color { Red, Green, Blue }
		consume(string s) {}
		test() {
			Color c = Color.Red;
			string s = "hi";
			int x = match c {
				Color.Red => { consume(s); 1; },
				Color.Green => 2,
				Color.Blue => 3,
			};
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === If-expression ===

func TestIfExprMoveInBranch(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			bool b = true;
			int x = if b { consume(s); 1; } else { 2; };
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Method body ===

func TestMethodBodyOwnership(t *testing.T) {
	ownerOK(t, `
		type Dog {
			string name;
			bark() string {
				return this.name;
			}
		}
	`)
}

func TestMethodBodyUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog {
			string name;
			test() {
				string s = "hi";
				string t = s;
				string u = s;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Lambda expression ===

func TestLambdaParamOwnership(t *testing.T) {
	ownerOK(t, `
		test() {
			f := |int x| -> x + 1;
		}
	`)
}

func TestLambdaDoesNotLeakMoveState(t *testing.T) {
	ownerOK(t, `
		test() {
			string s = "hi";
			f := |int x| -> x + 1;
			string t = s;
		}
	`)
}

// === Expression branches ===

func TestBinaryExprMovedOperand(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			string t = s;
			string u = s + "world";
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestParenExprMovedInner(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			string t = s;
			string u = (s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestTupleLitMoveElements(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			x := (s, s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestArrayLitMoveElements(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string a = "a";
			string b = "b";
			x := [a, b];
			string c = a;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

func TestMapLitMoveValues(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string v = "val";
			m := {"key": v};
			string x = v;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v'")
}

func TestIndexExprMovedTarget(t *testing.T) {
	errs := ownerErrs(t, `
		consume(int[] a) {}
		test() {
			int[] items = [1, 2, 3];
			consume(items);
			int x = items[0];
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'items'")
}

// === Assignment targets ===

func TestAssignTargetMemberExpr(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog { string name; }
		test() {
			string s = "hi";
			string t = s;
			Dog d = Dog(name: "Rex");
			d.name = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestCompoundAssignReadsTarget(t *testing.T) {
	// int is copy so no error — just exercises the compound assign code path.
	ownerOK(t, `
		test() {
			int x = 1;
			int y = 2;
			x += y;
		}
	`)
}

func TestWhileUnwrapStmt(t *testing.T) {
	// Unit test: construct WhileUnwrapStmt directly since sema syntax support is limited.
	c := &Checker{
		state: make(StateMap),
		info:  &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	// Simulate: while val := expr { <use s> }
	// Register "s" as owned non-copy var.
	sIdent := &ast.IdentExpr{Name: "s"}
	sVar := types.NewVar(types.Pos{}, "s", types.TypString)
	c.info.Objects[sIdent] = sVar
	c.state["s"] = Owned

	// Build a WhileUnwrapStmt with binding "val" and body that uses "s".
	stmt := &ast.WhileUnwrapStmt{
		Binding: "val",
		Value:   &ast.IntLit{Raw: "1"},
		Body: &ast.Block{
			Stmts: []ast.Stmt{
				&ast.ExprStmt{Expr: sIdent},
			},
		},
	}
	c.checkWhileUnwrapStmt(stmt)

	// After the loop, "s" should still be usable (conservative merge with pre-loop),
	// but the binding "val" should be registered as Owned.
	if c.state["val"] != Owned {
		t.Errorf("expected binding 'val' to be Owned, got %v", c.state["val"])
	}
}

// === Copy meta integration ===

func TestUserCopyTypeNeverMoves(t *testing.T) {
	ownerOK(t, `
		type Point `+"`copy"+` {
			int x;
			int y;
		}
		test() {
			Point p = Point(x: 1, y: 2);
			Point q = p;
			Point r = p;
		}
	`)
}

func TestUserNonCopyTypeMoves(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog {
			string name;
		}
		test() {
			Dog d = Dog(name: "Rex");
			Dog e = d;
			Dog f = d;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'd'")
}

func TestUserCopyInCall(t *testing.T) {
	ownerOK(t, `
		type Pt `+"`copy"+` {
			int x;
		}
		take(Pt p) {}
		test() {
			Pt p = Pt(x: 1);
			take(p);
			take(p);
		}
	`)
}

// ===== Stage 6b: Borrow Tracking =====

// === Call-scoped borrow expiry ===

func TestCallScopedBorrowExpires(t *testing.T) {
	// Passing a variable by shared borrow should not prevent subsequent moves.
	// The borrow expires at the statement boundary.
	ownerOK(t, `
		read(string &s) {}
		consume(string s) {}
		test() {
			string s = "a";
			read(s);
			consume(s);
		}
	`)
}

func TestSequentialMutBorrowsOK(t *testing.T) {
	// Each mutable borrow expires at statement boundary, so sequential calls are OK.
	ownerOK(t, `
		modify(string ~s) {}
		test() {
			string s = "a";
			modify(s);
			modify(s);
		}
	`)
}

func TestSequentialSharedThenMutOK(t *testing.T) {
	// Shared borrow expires before mutable borrow starts.
	ownerOK(t, `
		read(string &s) {}
		modify(string ~s) {}
		test() {
			string s = "a";
			read(s);
			modify(s);
		}
	`)
}

// === Cross-statement borrow conflicts (variable-scoped borrows) ===

func TestStoredBorrowBlocksMove(t *testing.T) {
	// When a function returns a ref type and the result is stored,
	// borrow is promoted to variable-scoped. Moving the origin is blocked.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestStoredBorrowBlocksMutBorrow(t *testing.T) {
	// Stored shared borrow blocks a subsequent mutable borrow.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		modify(string ~s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			modify(s);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 's' as mutable")
}

func TestStoredMutBorrowBlocksShared(t *testing.T) {
	// Stored mutable borrow blocks a subsequent shared borrow.
	errs := ownerErrs(t, `
		getMut(string ~s) string~ { return s; }
		read(string &s) {}
		test() {
			string s = "hello";
			string ~r = getMut(s);
			read(s);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 's' as shared")
}

// === Move-while-borrowed ===

func TestMoveWhileBorrowedAssign(t *testing.T) {
	// Assigning a borrowed variable to another variable is a move.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		test() {
			string s = "hello";
			string &r = getRef(s);
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// === Assignment-while-borrowed ===

func TestAssignWhileBorrowed(t *testing.T) {
	// Cannot reassign a variable while it is borrowed by another variable.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		test() {
			string s = "hello";
			string &r = getRef(s);
			s = "world";
		}
	`)
	expectOwnerError(t, errs, "cannot assign to 's' while it is borrowed")
}

func TestBorrowerReassignExpiresBorrow(t *testing.T) {
	// When the borrower variable is reassigned, the old borrow expires.
	// However, if r is reassigned to a new borrow of s, s is still borrowed.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			r = getRef(s);
			consume(s);
		}
	`)
	// s is still borrowed through the new r
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// === Return reference safety ===

func TestReturnRefToLocal(t *testing.T) {
	// Cannot return a reference to a local variable — would create a dangling reference.
	errs := ownerErrs(t, `
		bad() string& {
			string s = "hello";
			return s;
		}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable 's'")
}

func TestReturnRefToParam(t *testing.T) {
	// Returning a reference to a parameter is OK — the caller still owns it.
	ownerOK(t, `
		good(string &s) string& { return s; }
	`)
}

func TestReturnNonRefOK(t *testing.T) {
	// Returning a non-ref type local is fine (it's a move, not a dangling ref).
	ownerOK(t, `
		ok() string {
			string s = "hello";
			return s;
		}
	`)
}

// === Method receiver borrows ===

func TestMethodSharedReceiverCallScoped(t *testing.T) {
	// Calling a shared-receiver method creates a call-scoped borrow that expires.
	ownerOK(t, `
		type T {
			int x;
			read(&this) int { return this.x; }
		}
		consume(T t) {}
		test() {
			T t = T(x: 1);
			t.read();
			consume(t);
		}
	`)
}

func TestMethodMutReceiverCallScoped(t *testing.T) {
	// Calling a mut-receiver method creates a call-scoped borrow that expires.
	ownerOK(t, `
		type T {
			int x;
			mutate(~this) { this.x = 2; }
		}
		consume(T t) {}
		test() {
			T t = T(x: 1);
			t.mutate();
			consume(t);
		}
	`)
}

func TestMethodReceiverStoredBorrow(t *testing.T) {
	// Method returning a ref type creates a stored borrow on the receiver.
	errs := ownerErrs(t, `
		type T {
			int x;
			getRef(&this) int& { return this.x; }
		}
		consume(T t) {}
		test() {
			T t = T(x: 1);
			int &r = t.getRef();
			consume(t);
		}
	`)
	expectOwnerError(t, errs, "cannot move 't' while it is borrowed")
}

// === Control flow and borrows ===

func TestBorrowInIfBranch(t *testing.T) {
	// Conservative: stored borrow created in then-branch persists after if.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			bool b = true;
			string &r = "";
			if b {
				r = getRef(s);
			}
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestBorrowInLoop(t *testing.T) {
	// Conservative: stored borrow created in loop body persists after loop.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = "";
			while true {
				r = getRef(s);
				break;
			}
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestBorrowInBothBranches(t *testing.T) {
	// Conservative: stored borrow in both branches → borrow persists.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			bool b = true;
			string &r = "";
			if b {
				r = getRef(s);
			} else {
				r = getRef(s);
			}
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// === Copy types and borrows ===

func TestCopyTypeNoBorrowTracking(t *testing.T) {
	// Copy types don't need borrow tracking — borrows of copy types are allowed freely.
	ownerOK(t, `
		read(int &x) {}
		test() {
			int x = 1;
			read(x);
			int y = x;
		}
	`)
}

func TestBorrowDoesNotMoveValue(t *testing.T) {
	// Passing by borrow does NOT consume the value — the variable can still be used.
	ownerOK(t, `
		read(string &s) {}
		consume(string s) {}
		test() {
			string s = "hello";
			read(s);
			read(s);
			consume(s);
		}
	`)
}

// === Borrow parameter does not move ===

func TestBorrowParamMultipleCalls(t *testing.T) {
	// Multiple shared borrow calls on same variable should work (borrows expire).
	ownerOK(t, `
		read(string &s) {}
		test() {
			string s = "hello";
			read(s);
			read(s);
			read(s);
		}
	`)
}

func TestMutBorrowParamDoesNotMove(t *testing.T) {
	// Passing by mutable borrow does NOT consume the value.
	ownerOK(t, `
		modify(string ~s) {}
		consume(string s) {}
		test() {
			string s = "hello";
			modify(s);
			consume(s);
		}
	`)
}

// === Cross-statement borrow: inferred var decl ===

func TestStoredBorrowInferredVarDecl(t *testing.T) {
	// Borrow promotion should also work with inferred var decls.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			r := getRef(s);
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// ===== Coverage: unit tests for uncovered branches =====

// newUnitChecker creates a Checker suitable for unit tests.
func newUnitChecker() *Checker {
	return &Checker{
		state: make(StateMap),
		info: &sema.Info{
			Types:   make(map[ast.Expr]types.Type),
			Objects: make(map[*ast.IdentExpr]types.Object),
			Scopes:  make(map[ast.Node]*types.Scope),
		},
	}
}

// movedIdent registers a string variable as Moved and returns its IdentExpr.
func movedIdent(c *Checker, name string) *ast.IdentExpr {
	ident := &ast.IdentExpr{Name: name}
	c.info.Objects[ident] = types.NewVar(types.Pos{}, name, types.TypString)
	c.state[name] = Moved
	return ident
}

// ownedIdent registers a string variable as Owned and returns its IdentExpr.
func ownedIdent(c *Checker, name string) *ast.IdentExpr {
	ident := &ast.IdentExpr{Name: name}
	c.info.Objects[ident] = types.NewVar(types.Pos{}, name, types.TypString)
	c.state[name] = Owned
	return ident
}

// --- BorrowSet ---

func TestActiveBorrowsOf(t *testing.T) {
	bs := NewBorrowSet()
	bs.Add(&Borrow{Origin: "s", Kind: BorrowShared, Borrower: "r"})
	bs.Add(&Borrow{Origin: "t", Kind: BorrowMut})
	bs.Add(&Borrow{Origin: "s", Kind: BorrowMut, Borrower: "q"})

	if got := len(bs.ActiveBorrowsOf("s")); got != 2 {
		t.Errorf("expected 2 borrows of 's', got %d", got)
	}
	if got := len(bs.ActiveBorrowsOf("t")); got != 1 {
		t.Errorf("expected 1 borrow of 't', got %d", got)
	}
	if got := len(bs.ActiveBorrowsOf("x")); got != 0 {
		t.Errorf("expected 0 borrows of 'x', got %d", got)
	}
}

// --- checkExpr: expression branches ---

func TestThisExprMoved(t *testing.T) {
	c := newUnitChecker()
	c.state["this"] = Moved
	c.checkExpr(&ast.ThisExpr{})
	expectOwnerError(t, c.errors, "use of moved variable 'this'")
}

func TestOptionalChainOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.OptionalChainExpr{Target: ident, Field: "length"})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestIsExprOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.IsExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestCastExprOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.CastExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestErrorPropagateOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.ErrorPropagateExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestErrorUnwrapOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.ErrorUnwrapExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestErrorHandlerBindingOwnership(t *testing.T) {
	c := newUnitChecker()
	c.checkExpr(&ast.ErrorHandlerExpr{
		Expr:    &ast.IntLit{Raw: "1"},
		Binding: "err",
		Body:    &ast.Block{},
	})
	if c.state["err"] != Owned {
		t.Errorf("expected 'err' to be Owned, got %v", c.state["err"])
	}
}

func TestGoExprBlockMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.GoExpr{
		Block: &ast.Block{
			Stmts: []ast.Stmt{&ast.ExprStmt{Expr: ident}},
		},
	})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestGoExprExprMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.GoExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestUnsafeExprMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.UnsafeExpr{
		Body: &ast.Block{
			Stmts: []ast.Stmt{&ast.ExprStmt{Expr: ident}},
		},
	})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

// --- checkStmt: statement branches ---

func TestRaiseStmtMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.RaiseStmt{Value: ident})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after raise, got %v", c.state["s"])
	}
}

func TestYieldStmtMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.YieldStmt{Value: ident})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after yield, got %v", c.state["s"])
	}
}

func TestYieldDelegateStmtMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.YieldDelegateStmt{Value: ident})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after yield*, got %v", c.state["s"])
	}
}

func TestNestedBlockStmt(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.Block{
		Stmts: []ast.Stmt{
			&ast.InferredVarDecl{Name: "t", Value: ident},
		},
	})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after nested block, got %v", c.state["s"])
	}
}

// --- registerPatternBindings ---

func TestEnumDestructurePatternBindings(t *testing.T) {
	c := newUnitChecker()
	c.registerPatternBindings(&ast.EnumDestructureMatchPattern{
		Enum:     "Color",
		Variant:  "Custom",
		Bindings: []string{"r", "g", "_"},
	})
	if c.state["r"] != Owned {
		t.Errorf("expected 'r' to be Owned")
	}
	if c.state["g"] != Owned {
		t.Errorf("expected 'g' to be Owned")
	}
	if _, exists := c.state["_"]; exists {
		t.Error("'_' should not be registered in state")
	}
}

func TestTypeBindingPatternBindings(t *testing.T) {
	c := newUnitChecker()
	c.registerPatternBindings(&ast.TypeBindingMatchPattern{
		TypeName: "Circle",
		Binding:  "c",
	})
	if c.state["c"] != Owned {
		t.Errorf("expected 'c' to be Owned")
	}
}

// --- checkAssignTarget: IndexExpr branch ---

func TestAssignTargetIndexExpr(t *testing.T) {
	c := newUnitChecker()
	target := movedIdent(c, "arr")
	index := movedIdent(c, "idx")
	c.checkAssignTarget(&ast.IndexExpr{Target: target, Index: index})
	if len(c.errors) != 2 {
		t.Fatalf("expected 2 use-after-move errors, got %d: %v", len(c.errors), c.errors)
	}
}

// --- checkForInStmt: index binding ---

func TestForInIndexBinding(t *testing.T) {
	c := newUnitChecker()
	c.checkStmt(&ast.ForInStmt{
		Binding:  "v",
		Index:    "i",
		Iterable: &ast.IntLit{Raw: "0"},
		Body:     &ast.Block{},
	})
	if c.state["v"] != Owned {
		t.Errorf("expected 'v' to be Owned")
	}
	if c.state["i"] != Owned {
		t.Errorf("expected 'i' to be Owned")
	}
}

// --- Stage 8m: use Bindings ---

func TestUseVarCannotBeMoved(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			close() {}
		}
		consume(Resource r) {}
		test() {
			use r := Resource();
			consume(r);
		}
	`)
	expectOwnerError(t, errs, "cannot move use-bound variable 'r'")
}

// --- Getter/Setter same name ---

func TestOwnershipGetterSetterSameName(t *testing.T) {
	// Ownership checker must resolve getter and setter bodies independently.
	ownerOK(t, `
		type Box {
			Box _inner;
			get inner Box { return this._inner; }
			set inner(Box v) { this._inner = v; }
		}
		test(Box b, Box v) {
			b.inner = v;
		}
	`)
}

// --- Droppable variable ownership ---

func TestDroppableVariableMove(t *testing.T) {
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 1);
			consume(r);
		}
	`)
}

func TestDroppableVariableUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 1);
			consume(r);
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableConditionalMoveUseAfter(t *testing.T) {
	// Moving in one branch makes it "maybe moved" — use after is an error
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test(bool cond) {
			r := Resource(id: 1);
			if cond {
				consume(r);
			}
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableConditionalMoveBothBranchesOK(t *testing.T) {
	// Moving in both branches is fine — no use after the if/else
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		other(Resource r) { }
		test(bool cond) {
			r := Resource(id: 1);
			if cond {
				consume(r);
			} else {
				other(r);
			}
		}
	`)
}

func TestDroppableMoveToAssignment(t *testing.T) {
	// Moving via assignment is valid
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			Resource a = Resource(id: 1);
			Resource b = a;
			int x = b.id;
		}
	`)
}

func TestDroppableMoveToAssignmentUseAfter(t *testing.T) {
	// Use after move via assignment
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			Resource a = Resource(id: 1);
			Resource b = a;
			int x = a.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

func TestDroppableMoveToMethodArg(t *testing.T) {
	// Moving to a method argument is valid
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		type Container {
			int id;
			take(Resource r) { }
		}
		test() {
			c := Container(id: 0);
			r := Resource(id: 1);
			c.take(r);
		}
	`)
}

func TestDroppableMoveToMethodArgUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		type Container {
			int id;
			take(Resource r) { }
		}
		test() {
			c := Container(id: 0);
			r := Resource(id: 1);
			c.take(r);
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableMoveToConstructorField(t *testing.T) {
	// Moving into constructor is valid
	ownerOK(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			r := Inner(id: 1);
			o := Outer(inner: r);
			int x = o.inner.id;
		}
	`)
}

func TestDroppableMoveToConstructorFieldUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			r := Inner(id: 1);
			o := Outer(inner: r);
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableReturnMove(t *testing.T) {
	// Returning a droppable variable is a valid move
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() Resource {
			r := Resource(id: 1);
			return r;
		}
	`)
}

func TestDroppableReturnMoveUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() Resource {
			r := Resource(id: 1);
			consume(r);
			return r;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableMoveToMemberAssign(t *testing.T) {
	// Moving via member assignment is valid
	ownerOK(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			o := Outer(inner: Inner(id: 0));
			r := Inner(id: 1);
			o.inner = r;
		}
	`)
}

func TestDroppableMoveToMemberAssignUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			o := Outer(inner: Inner(id: 0));
			r := Inner(id: 1);
			o.inner = r;
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableNoMoveNoError(t *testing.T) {
	// Variable never moved — just used normally, then dropped at scope exit
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			r := Resource(id: 1);
			int x = r.id;
			int y = r.id;
		}
	`)
}

func TestDroppableMultipleVarsIndependentMoves(t *testing.T) {
	// Multiple droppable vars moved independently
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			a := Resource(id: 1);
			b := Resource(id: 2);
			consume(a);
			consume(b);
		}
	`)
}

func TestDroppableReassignmentResurrects(t *testing.T) {
	// After moving, reassigning brings the variable back to Owned
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 1);
			consume(r);
			r = Resource(id: 2);
			int x = r.id;
		}
	`)
}

// checkAssignTarget: index expression target checks sub-expressions
func TestAssignTargetIndexSubExpressions(t *testing.T) {
	// arr[i] = val — should check arr is not moved
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			arr := [1, 2, 3];
			consume_arr(arr);
			arr[0] = 5;
		}
		consume_arr(int[] a) { }
	`)
	expectOwnerError(t, errs, "use of moved variable 'arr'")
}

// checkAssignTarget: member expression target checks sub-expressions
func TestAssignTargetMemberSubExpressions(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			int val;
		}
		consume(Box b) { }
		test() {
			b := Box(val: 1);
			consume(b);
			b.val = 5;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// checkAssignTarget: slice expression target
func TestAssignTargetSliceSubExpressions(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			arr := [1, 2, 3];
			consume_arr(arr);
			arr[0:2] = [5, 6];
		}
		consume_arr(int[] a) { }
	`)
	expectOwnerError(t, errs, "use of moved variable 'arr'")
}

// checkAssignTarget: index expression checks both target AND index
func TestAssignTargetIndexExprChecksIndex(t *testing.T) {
	// The index sub-expression itself uses a moved variable
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 0);
			consume(r);
			arr := [1, 2, 3];
			arr[r.id] = 5;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// Move to index assignment
func TestDroppableMoveToIndexAssign(t *testing.T) {
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			arr := [Resource(id: 0)];
			r := Resource(id: 1);
			arr[0] = r;
		}
	`)
}

// Use after move to index assignment
func TestDroppableMoveToIndexAssignUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			arr := [Resource(id: 0)];
			r := Resource(id: 1);
			arr[0] = r;
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// === Lambda capture ownership ===

func TestMoveCaptureMarksVariableMoved(t *testing.T) {
	errs := ownerErrs(t, `
		type Foo { int x; drop(~this) {} }
		test() {
			f := Foo(x: 1);
			g := move |int y| -> f.x + y;
			int z = f.x;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

func TestCopyCaptureDoesNotMoveVariable(t *testing.T) {
	ownerOK(t, `
		test() {
			int x = 42;
			f := |int y| -> x + y;
			int z = x + 1;
		}
	`)
}

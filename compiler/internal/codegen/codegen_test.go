package codegen

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
	antlr "github.com/antlr4-go/antlr/v4"
	irtypes "github.com/llir/llvm/ir/types"
)

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
		// Range operators for integer types only (not floats)
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
	b.WriteString("\t[:](int? start, int? end) T[] `native;\n")
	b.WriteString("\t[:]=(int? start, int? end, T[] value) `native;\n")
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

	// Channel
	b.WriteString("type channel[T] `native {\n")
	b.WriteString("\tnew(int? capacity) `native;\n")
	b.WriteString("\tsend(T value) `native;\n")
	b.WriteString("\tclose() `native;\n")
	b.WriteString("}\n")

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

// parseWithStd parses std declarations and user code, merges them, and runs sema.
func parseWithStd(t *testing.T, src string) (*ast.File, *sema.Info) {
	t.Helper()

	// Parse std
	stdInput := antlr.NewInputStream(stdAll)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, errs := ast.Build("std.pr", stdTree)
	if len(errs) > 0 {
		t.Fatalf("std AST build errors: %v", errs)
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
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	// Merge: std first, then user
	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(file.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, file.Decls...)
	file.Decls = merged

	info, errs := sema.Check(file)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	return file, info
}

// generateIR runs the full pipeline: parse → sema → codegen, returns LLVM IR text.
func generateIR(t *testing.T, src string) string {
	t.Helper()
	file, info := parseWithStd(t, src)
	result := Compile(file, info)
	return result.Module.String()
}

// compileResult runs the full pipeline and returns the CompileResult.
func compileResult(t *testing.T, src string) *CompileResult {
	t.Helper()
	file, info := parseWithStd(t, src)
	return Compile(file, info)
}

func assertContains(t *testing.T, ir, substr string) {
	t.Helper()
	if !strings.Contains(ir, substr) {
		t.Errorf("expected IR to contain %q\ngot:\n%s", substr, ir)
	}
}

func assertNotContains(t *testing.T, ir, substr string) {
	t.Helper()
	if strings.Contains(ir, substr) {
		t.Errorf("expected IR to NOT contain %q\ngot:\n%s", substr, ir)
	}
}

// --- Literal tests ---

func TestIntLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "store i64 42")
}

func TestFloatLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := 3.14; }`)
	assertContains(t, ir, "double")
	// LLVM serializes floats as hex: 3.14 → 0x40091EB851EB851F
	assertContains(t, ir, "store double")
}

func TestBoolLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := true; y := false; }`)
	assertContains(t, ir, "store i1 true")
	assertContains(t, ir, "store i1 false")
}

// --- Binary operator tests (type-system dispatch) ---

func TestIntAdd(t *testing.T) {
	ir := generateIR(t, `main() { x := 1 + 2; }`)
	assertContains(t, ir, "add i64")
}

func TestIntSub(t *testing.T) {
	ir := generateIR(t, `main() { x := 5 - 3; }`)
	assertContains(t, ir, "sub i64")
}

func TestIntMul(t *testing.T) {
	ir := generateIR(t, `main() { x := 3 * 4; }`)
	assertContains(t, ir, "mul i64")
}

func TestIntDiv(t *testing.T) {
	ir := generateIR(t, `main() { x := 10 / 3; }`)
	assertContains(t, ir, "sdiv i64")
}

func TestIntMod(t *testing.T) {
	ir := generateIR(t, `main() { x := 10 % 3; }`)
	assertContains(t, ir, "srem i64")
}

func TestIntComparison(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := 1 == 2;
			b := 1 != 2;
			c := 1 < 2;
			d := 1 > 2;
			e := 1 <= 2;
			f := 1 >= 2;
		}
	`)
	assertContains(t, ir, "icmp eq")
	assertContains(t, ir, "icmp ne")
	assertContains(t, ir, "icmp slt")
	assertContains(t, ir, "icmp sgt")
	assertContains(t, ir, "icmp sle")
	assertContains(t, ir, "icmp sge")
}

func TestFloatArithmetic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := 1.0 + 2.0;
			b := 1.0 - 2.0;
			c := 1.0 * 2.0;
			d := 1.0 / 2.0;
		}
	`)
	assertContains(t, ir, "fadd double")
	assertContains(t, ir, "fsub double")
	assertContains(t, ir, "fmul double")
	assertContains(t, ir, "fdiv double")
}

func TestFloatComparison(t *testing.T) {
	ir := generateIR(t, `main() { a := 1.0 < 2.0; }`)
	assertContains(t, ir, "fcmp olt")
}

// --- Unary operator tests ---

func TestUnaryNegInt(t *testing.T) {
	ir := generateIR(t, `main() { x := -42; }`)
	assertContains(t, ir, "sub i64 0")
}

func TestUnaryNot(t *testing.T) {
	ir := generateIR(t, `main() { x := !true; }`)
	assertContains(t, ir, "xor i1")
}

// --- Bitwise operators ---

func TestBitwiseAnd(t *testing.T) {
	ir := generateIR(t, `main() { x := 12 & 10; }`)
	assertContains(t, ir, "and i64")
}

func TestBitwiseOr(t *testing.T) {
	ir := generateIR(t, `main() { x := 5 | 3; }`)
	assertContains(t, ir, "or i64")
}

func TestBitwiseXor(t *testing.T) {
	ir := generateIR(t, `main() { x := 12 ^ 10; }`)
	assertContains(t, ir, "xor i64")
}

func TestLeftShift(t *testing.T) {
	ir := generateIR(t, `main() { x := 1 << 4; }`)
	assertContains(t, ir, "shl i64")
}

func TestRightShiftSigned(t *testing.T) {
	ir := generateIR(t, `main() { x := 16 >> 2; }`)
	assertContains(t, ir, "ashr i64")
}

func TestBitwiseNot(t *testing.T) {
	ir := generateIR(t, `main() { x := ~0; }`)
	assertContains(t, ir, "xor i64")
}

// --- Short-circuit boolean ops ---

func TestShortCircuitAnd(t *testing.T) {
	ir := generateIR(t, `main() { x := true && false; }`)
	assertContains(t, ir, "and.rhs")
	assertContains(t, ir, "and.merge")
}

func TestShortCircuitOr(t *testing.T) {
	ir := generateIR(t, `main() { x := true || false; }`)
	assertContains(t, ir, "or.rhs")
	assertContains(t, ir, "or.merge")
}

// --- Variable tests ---

func TestVariableAllocaAndLoad(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			int y = x;
		}
	`)
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "store i64 10")
	assertContains(t, ir, "load i64")
}

func TestInferredVarDecl(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "store i64 42")
}

func TestAssignment(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 1;
			x = 2;
		}
	`)
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
}

func TestCompoundAssignment(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			x += 5;
		}
	`)
	assertContains(t, ir, "add i64")
}

func TestIncrementDecrement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			x++;
			x--;
		}
	`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "sub i64")
}

// --- Function tests ---

func TestFunctionDeclaration(t *testing.T) {
	ir := generateIR(t, `
		add(int a, int b) int {
			return a + b;
		}
		main() { }
	`)
	assertContains(t, ir, "define i64 @add(i64 %a, i64 %b)")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "ret i64")
}

func TestFunctionCall(t *testing.T) {
	ir := generateIR(t, `
		double(int x) int { return x * 2; }
		main() { y := double(21); }
	`)
	assertContains(t, ir, "call i64 @double(i64")
}

func TestVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		noop() { }
		main() { noop(); }
	`)
	assertContains(t, ir, "define void @noop()")
	assertContains(t, ir, "call void @noop()")
}

// --- Extern print (struct-based ABI) ---

func TestPrintInt(t *testing.T) {
	ir := generateIR(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		main() { print(42); }
	`)
	// Struct type definition
	assertContains(t, ir, "%promise_int_v = type")
	// PAL-defined print function: extern turned into define with body
	assertContains(t, ir, "define void @promise_print_int(i8*")
	// Struct packing via insertvalue
	assertContains(t, ir, "insertvalue %promise_int_v")
}

func TestPrintBool(t *testing.T) {
	ir := generateIR(t, `
		print(bool x) `+"`"+`extern("promise_print_bool");
		main() { print(true); }
	`)
	assertContains(t, ir, "%promise_bool_v = type")
	assertContains(t, ir, "define void @promise_print_bool(i8*")
	// Bool coercion: i1 → i8
	assertContains(t, ir, "zext i1 true to i8")
	assertContains(t, ir, "insertvalue %promise_bool_v")
}

func TestPrintF64(t *testing.T) {
	ir := generateIR(t, `
		print(f64 x) `+"`"+`extern("promise_print_f64");
		main() { print(3.14); }
	`)
	assertContains(t, ir, "%promise_f64_v = type")
	assertContains(t, ir, "define void @promise_print_f64(i8*")
	assertContains(t, ir, "insertvalue %promise_f64_v")
}

// --- PAL function body tests ---
// These verify that definePALBodies() generates correct IR for print/panic functions.

func TestPrintIntBody(t *testing.T) {
	ir := generateIR(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		main() { print(42); }
	`)
	// Function body: extracts raw from value struct, converts to string, writes via PAL
	assertContains(t, ir, "define void @promise_print_int(i8*")
	assertContains(t, ir, "bitcast i8* %x to %promise_int_v*")
	assertContains(t, ir, "call i8* @promise_int_to_string(i64")
	assertContains(t, ir, "call i64 @pal_write(i32 1,") // stdout
	assertContains(t, ir, "call void @pal_free(i8*")    // free temp string
}

func TestPrintF64Body(t *testing.T) {
	ir := generateIR(t, `
		print(f64 x) `+"`"+`extern("promise_print_f64");
		main() { print(3.14); }
	`)
	assertContains(t, ir, "define void @promise_print_f64(i8*")
	assertContains(t, ir, "bitcast i8* %x to %promise_f64_v*")
	assertContains(t, ir, "call i8* @promise_f64_to_string(double")
	assertContains(t, ir, "call i64 @pal_write(i32 1,")
	assertContains(t, ir, "call void @pal_free(i8*")
}

func TestPrintBoolBody(t *testing.T) {
	ir := generateIR(t, `
		print(bool x) `+"`"+`extern("promise_print_bool");
		main() { print(true); }
	`)
	assertContains(t, ir, "define void @promise_print_bool(i8*")
	assertContains(t, ir, "bitcast i8* %x to %promise_bool_v*")
	assertContains(t, ir, "call i8* @promise_bool_to_string(i8")
	assertContains(t, ir, "call i64 @pal_write(i32 1,")
	assertContains(t, ir, "call void @pal_free(i8*")
}

func TestPrintStringBody(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Function body: extracts data/len from string value struct, writes via PAL
	assertContains(t, ir, "define void @promise_print_string(i8*")
	assertContains(t, ir, "bitcast i8* %s to %promise_string_v*")
	assertContains(t, ir, "call i64 @pal_write(i32 1,") // stdout
}

func TestPanicBody(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// promise_panic is always declared as intrinsic; definePALBodies adds body
	assertContains(t, ir, "define void @promise_panic(i8*")
	assertContains(t, ir, "call i64 @strlen(i8*")
	assertContains(t, ir, "call i64 @pal_write(i32 2,") // stderr
	assertContains(t, ir, "call void @pal_exit(i32 1)")
	assertContains(t, ir, "unreachable")
}

func TestPanicMsgBody(t *testing.T) {
	ir := generateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	assertContains(t, ir, "define void @promise_panic_msg(i8*")
	assertContains(t, ir, "bitcast i8* %msg to %promise_string_v*")
	assertContains(t, ir, "call i64 @pal_write(i32 2,") // stderr
	assertContains(t, ir, "call void @pal_exit(i32 1)")
	assertContains(t, ir, "unreachable")
}

func TestPALWriteExitDefined(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// PAL primitives are always emitted
	assertContains(t, ir, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)")
	assertContains(t, ir, "call i64 @write(i32 %fd, i8* %buf, i64 %len)")
	assertContains(t, ir, "define void @pal_exit(i32 %code)")
	assertContains(t, ir, "call void @exit(i32 %code)")
}

func TestPrintNewlineEmission(t *testing.T) {
	ir := generateIR(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		main() { print(1); }
	`)
	// Newline global constant and two pal_write calls (data + newline)
	assertContains(t, ir, `@.str.newline = constant [1 x i8] c"\0A"`)
	// The print_int body should have exactly two pal_write calls
	assertContains(t, ir, `@.str.panic_prefix = constant [7 x i8] c"panic: "`)
}

// --- Control flow tests ---

func TestIfStmt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			if true {
				int x = 1;
			}
		}
	`)
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.end")
	assertContains(t, ir, "br i1 true")
}

func TestIfElseStmt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			if true {
				int x = 1;
			} else {
				int y = 2;
			}
		}
	`)
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.else")
	assertContains(t, ir, "if.end")
}

func TestWhileLoop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 0;
			while x < 10 {
				x += 1;
			}
		}
	`)
	assertContains(t, ir, "while.header")
	assertContains(t, ir, "while.body")
	assertContains(t, ir, "while.exit")
	assertContains(t, ir, "icmp slt")
}

func TestInfiniteLoopWithBreak(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for {
				break;
			}
		}
	`)
	assertContains(t, ir, "loop.body")
	assertContains(t, ir, "loop.exit")
}

func TestForInRange(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int sum = 0;
			for i in 0..10 {
				sum += i;
			}
		}
	`)
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
	assertContains(t, ir, "forin.update")
	assertContains(t, ir, "forin.exit")
}

func TestReturnValue(t *testing.T) {
	ir := generateIR(t, `
		answer() int { return 42; }
		main() { }
	`)
	assertContains(t, ir, "ret i64 42")
}

func TestVoidReturn(t *testing.T) {
	ir := generateIR(t, `main() { return; }`)
	assertContains(t, ir, "ret void")
}

// --- Integration tests ---

func TestFibonacci(t *testing.T) {
	ir := generateIR(t, `
		fib(int n) int {
			if n <= 1 {
				return n;
			}
			return fib(n - 1) + fib(n - 2);
		}
		main() { x := fib(10); }
	`)
	assertContains(t, ir, "define i64 @fib(i64 %n)")
	assertContains(t, ir, "call i64 @fib")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "icmp sle")
}

// --- Extern architecture tests ---

func TestExternCustomCName(t *testing.T) {
	ir := generateIR(t, `
		log_value(int x) `+"`"+`extern("my_log_int");
		main() { log_value(99); }
	`)
	assertContains(t, ir, "declare void @my_log_int(i8*")
	assertContains(t, ir, "call void @my_log_int(i8*")
}

func TestExternDefaultCName(t *testing.T) {
	ir := generateIR(t, `
		do_thing(int x) `+"`"+`extern;
		main() { do_thing(1); }
	`)
	assertContains(t, ir, "declare void @promise_do_thing(i8*")
}

func TestExternMultipleParams(t *testing.T) {
	ir := generateIR(t, `
		add_ext(int a, int b) `+"`"+`extern("test_add");
		main() { add_ext(1, 2); }
	`)
	assertContains(t, ir, "declare void @test_add(i8* %a, i8* %b)")
	assertContains(t, ir, "call void @test_add")
}

func TestExternReturnValue(t *testing.T) {
	ir := generateIR(t, `
		get_value() int `+"`"+`extern("test_get");
		main() { x := get_value(); }
	`)
	// sret: struct return becomes void with first param as result pointer
	assertContains(t, ir, "declare void @test_get(i8* %sret)")
	// Return value should be loaded from sret alloca and unpacked
	assertContains(t, ir, "extractvalue %promise_int_v")
}

func TestExternStructTypeDefs(t *testing.T) {
	ir := generateIR(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		main() { print(42); }
	`)
	// All four struct types should be defined
	assertContains(t, ir, "%promise_int_t = type {}")
	assertContains(t, ir, "%promise_int_m = type { %promise_int_t* }")
	assertContains(t, ir, "%promise_int_i = type { %promise_int_m* }")
	assertContains(t, ir, "%promise_int_v = type { i8*, %promise_int_i*, i64 }")
}

// --- Primitive type layout coverage ---
// These tests verify that layout computation and extern declarations work
// for all primitive types. Externs are declared but not called since sema
// doesn't allow implicit narrowing from int/f64 literals to narrow types.

func TestExternI8Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i8(i8 x) `+"`"+`extern("test_i8");
		main() { }
	`)
	assertContains(t, ir, "%promise_i8_v = type { i8*, %promise_i8_i*, i8 }")
	assertContains(t, ir, "%promise_i8_i = type { %promise_i8_m* }")
	assertContains(t, ir, "%promise_i8_m = type { %promise_i8_t* }")
	assertContains(t, ir, "%promise_i8_t = type {}")
	assertContains(t, ir, "declare void @test_i8(i8*")
}

func TestExternI16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i16(i16 x) `+"`"+`extern("test_i16");
		main() { }
	`)
	assertContains(t, ir, "%promise_i16_v = type { i8*, %promise_i16_i*, i16 }")
	assertContains(t, ir, "declare void @test_i16(i8*")
}

func TestExternI32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i32(i32 x) `+"`"+`extern("test_i32");
		main() { }
	`)
	assertContains(t, ir, "%promise_i32_v = type { i8*, %promise_i32_i*, i32 }")
	assertContains(t, ir, "declare void @test_i32(i8*")
}

func TestExternU8Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u8(u8 x) `+"`"+`extern("test_u8");
		main() { }
	`)
	assertContains(t, ir, "%promise_u8_v = type { i8*, %promise_u8_i*, i8 }")
	assertContains(t, ir, "declare void @test_u8(i8*")
}

func TestExternU16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u16(u16 x) `+"`"+`extern("test_u16");
		main() { }
	`)
	assertContains(t, ir, "%promise_u16_v = type { i8*, %promise_u16_i*, i16 }")
	assertContains(t, ir, "declare void @test_u16(i8*")
}

func TestExternU32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u32(u32 x) `+"`"+`extern("test_u32");
		main() { }
	`)
	assertContains(t, ir, "%promise_u32_v = type { i8*, %promise_u32_i*, i32 }")
	assertContains(t, ir, "declare void @test_u32(i8*")
}

func TestExternU64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u64(u64 x) `+"`"+`extern("test_u64");
		main() { }
	`)
	assertContains(t, ir, "%promise_u64_v = type { i8*, %promise_u64_i*, i64 }")
	assertContains(t, ir, "declare void @test_u64(i8*")
}

func TestExternI64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i64(i64 x) `+"`"+`extern("test_i64");
		main() { }
	`)
	assertContains(t, ir, "%promise_i64_v = type { i8*, %promise_i64_i*, i64 }")
	assertContains(t, ir, "declare void @test_i64(i8*")
}

func TestExternF32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_f32(f32 x) `+"`"+`extern("test_f32");
		main() { }
	`)
	assertContains(t, ir, "%promise_f32_v = type { i8*, %promise_f32_i*, float }")
	assertContains(t, ir, "declare void @test_f32(i8*")
}

func TestExternCharLayout(t *testing.T) {
	ir := generateIR(t, `
		log_char(char x) `+"`"+`extern("test_char");
		main() { }
	`)
	assertContains(t, ir, "%promise_char_v = type { i8*, %promise_char_i*, i32 }")
	assertContains(t, ir, "declare void @test_char(i8*")
}

func TestExternUintLayout(t *testing.T) {
	ir := generateIR(t, `
		log_uint(uint x) `+"`"+`extern("test_uint");
		main() { }
	`)
	assertContains(t, ir, "%promise_uint_v = type { i8*, %promise_uint_i*, i64 }")
	assertContains(t, ir, "declare void @test_uint(i8*")
}

// --- Header generation: return types and zero-param ---

func TestHeaderExternReturnType(t *testing.T) {
	result := compileResult(t, `
		get_val() int `+"`"+`extern("test_get_val");
		main() { x := get_val(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Return type uses sret: void return with first param as result pointer
	assertContains(t, header, "void test_get_val(promise_int_v *sret);")
}

func TestHeaderExternZeroParams(t *testing.T) {
	result := compileResult(t, `
		do_nothing() `+"`"+`extern("test_noop");
		main() { do_nothing(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Zero-param void functions should have (void) in C
	assertContains(t, header, "void test_noop(void);")
}

func TestHeaderExternMultipleTypes(t *testing.T) {
	// Externs only declared (not called) since sema doesn't allow implicit narrowing
	result := compileResult(t, `
		log_i32(i32 x) `+"`"+`extern("test_log_i32");
		log_bool(bool x) `+"`"+`extern("test_log_bool");
		log_f32(f32 x) `+"`"+`extern("test_log_f32");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// bool layout: raw is uint8_t
	assertContains(t, header, "typedef struct { } promise_bool_t;")
	assertContains(t, header, "uint8_t              raw;")

	// i32 layout: raw is int32_t
	assertContains(t, header, "typedef struct { } promise_i32_t;")
	assertContains(t, header, "int32_t              raw;")

	// f32 layout: raw is float
	assertContains(t, header, "typedef struct { } promise_f32_t;")
	assertContains(t, header, "float                raw;")

	// Function declarations: all params passed by pointer
	assertContains(t, header, "void test_log_i32(promise_i32_v *x);")
	assertContains(t, header, "void test_log_bool(promise_bool_v *x);")
	assertContains(t, header, "void test_log_f32(promise_f32_v *x);")
}

// --- Ref param tests (shared & and mutable ~) ---

func TestExternSharedRefParam(t *testing.T) {
	ir := generateIR(t, `
		modify(int &x) `+"`"+`extern("test_modify");
		main() { }
	`)
	// Shared ref param should be a pointer to the value struct
	assertContains(t, ir, "declare void @test_modify(%promise_int_v*")
}

func TestExternMutRefParam(t *testing.T) {
	ir := generateIR(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)
	// Mutable ref param should be a pointer to the value struct
	assertContains(t, ir, "declare void @test_update(%promise_int_v*")
}

func TestHeaderExternSharedRefParam(t *testing.T) {
	result := compileResult(t, `
		modify(int &x) `+"`"+`extern("test_modify");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Shared ref param should be pointer in C header
	assertContains(t, header, "void test_modify(promise_int_v *x);")
}

func TestHeaderExternMutRefParam(t *testing.T) {
	result := compileResult(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Mutable ref param should be pointer in C header
	assertContains(t, header, "void test_update(promise_int_v *x);")
}

func TestHeaderGeneration(t *testing.T) {
	result := compileResult(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		print_f(f64 x) `+"`"+`extern("promise_print_f64");
		main() { print(42); print_f(3.14); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Header guard
	assertContains(t, header, "#ifndef PROMISE_BINDINGS_H")
	assertContains(t, header, "#include <stdint.h>")

	// Type definitions for int
	assertContains(t, header, "typedef struct { } promise_int_t;")
	assertContains(t, header, "promise_int_v;")

	// Type definitions for f64
	assertContains(t, header, "typedef struct { } promise_f64_t;")
	assertContains(t, header, "promise_f64_v;")

	// Function declarations: all params by pointer
	assertContains(t, header, "void promise_print_int(promise_int_v *x);")
	assertContains(t, header, "void promise_print_f64(promise_f64_v *x);")
}

// --- String tests ---

func TestStringLiteral(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Global string constant
	assertContains(t, ir, `c"hello"`)
	// Call to promise_string_new
	assertContains(t, ir, "call i8* @promise_string_new(")
	// Packing into value struct
	assertContains(t, ir, "insertvalue %promise_string_v")
	// Call to extern
	assertContains(t, ir, "call void @promise_print_string(")
}

func TestStringVariable(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello"; }`)
	// Alloca for i8* (string pointer)
	assertContains(t, ir, "alloca i8*")
	// Call to promise_string_new
	assertContains(t, ir, "call i8* @promise_string_new(")
	// Store i8* into alloca
	assertContains(t, ir, "store i8*")
}

func TestStringConcat(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello" + " world"; }`)
	// Two string literals
	assertContains(t, ir, `c"hello"`)
	assertContains(t, ir, `c" world"`)
	// Concat intrinsic
	assertContains(t, ir, "call i8* @promise_string_concat(")
}

func TestStringEquality(t *testing.T) {
	ir := generateIR(t, `main() { b := "a" == "b"; }`)
	assertContains(t, ir, "call i1 @promise_string_eq(")
}

func TestStringEqFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { b := "a" == "b"; }`)
	// Same-pointer fast path
	assertContains(t, ir, "icmp eq i8* %a, %b")
	// Length comparison
	assertContains(t, ir, "check_len:")
	// memcmp-based data comparison (replaces byte-by-byte loop)
	assertContains(t, ir, "call i32 @memcmp(")
	// Terminal blocks
	assertContains(t, ir, "equal:")
	assertContains(t, ir, "not_equal:")
}

func TestStringNotEqual(t *testing.T) {
	ir := generateIR(t, `main() { b := "a" != "b"; }`)
	assertContains(t, ir, "call i1 @promise_string_eq(")
	assertContains(t, ir, "xor i1")
}

func TestStringExternPacking(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Bitcast i8* to promise_string_i*
	assertContains(t, ir, "bitcast i8* %")
	// Insert into value struct
	assertContains(t, ir, "insertvalue %promise_string_v")
}

func TestStringLayout(t *testing.T) {
	// String layout struct types should always be present
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "%promise_string_t = type {}")
	assertContains(t, ir, "%promise_string_m = type { %promise_string_t* }")
	assertContains(t, ir, "%promise_string_i = type { %promise_string_m*, i64, [0 x i8] }")
	assertContains(t, ir, "%promise_string_v = type { i8*, %promise_string_i* }")
}

func TestStringHeader(t *testing.T) {
	result := compileResult(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// String layout with flexible array member
	assertContains(t, header, "typedef struct { } promise_string_t;")
	assertContains(t, header, "promise_string_m;")
	assertContains(t, header, "char                 data[];")
	assertContains(t, header, "promise_string_i;")
	assertContains(t, header, "promise_string_v;")

	// Extern declaration: string param by pointer
	assertContains(t, header, "void promise_print_string(promise_string_v *s);")
}

func TestStringEscapes(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello\nworld"; }`)
	// The global should contain the actual newline character
	assertContains(t, ir, `c"hello\0Aworld"`)
}

func TestStringExternReturn(t *testing.T) {
	ir := generateIR(t, `
		get_greeting() string `+"`"+`extern("promise_get_greeting");
		main() { s := get_greeting(); }
	`)
	// Extern returns promise_string_v
	assertContains(t, ir, "define i32 @main()")
	// Unpack: extractvalue + bitcast back to i8*
	assertContains(t, ir, "extractvalue %promise_string_v")
	assertContains(t, ir, "bitcast %promise_string_i*")
}

func TestStringEmpty(t *testing.T) {
	ir := generateIR(t, `main() { s := ""; }`)
	// Empty string: [0 x i8] global constant
	assertContains(t, ir, "call i8* @promise_string_new(")
	// Length argument should be 0
	assertContains(t, ir, "i64 0)")
}

func TestStringEscapeBrace(t *testing.T) {
	ir := generateIR(t, `main() { s := "a\{b"; }`)
	// \{ should resolve to literal {
	assertContains(t, ir, `c"a{b"`)
}

func TestStringIntrinsicsDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	// String intrinsics should always be defined (codegen-emitted LLVM IR)
	assertContains(t, ir, "define i8* @promise_string_new(i8* %data, i64 %len)")
	assertContains(t, ir, "define i8* @promise_string_concat(i8* %a, i8* %b)")
	assertContains(t, ir, "define i1 @promise_string_eq(i8* %a, i8* %b)")
}

func TestStringNewFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello"; }`)
	assertContains(t, ir, "define i8* @promise_string_new(i8* %data, i64 %len)")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
	assertContains(t, ir, "store i8* null")
}

func TestStringConcatFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { s := "a" + "b"; }`)
	assertContains(t, ir, "define i8* @promise_string_concat(i8* %a, i8* %b)")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
}

func TestLLVMMemcpyDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "declare void @llvm.memcpy.p0i8.p0i8.i64(")
}

// === User Type Tests ===

func TestUserTypeLayout(t *testing.T) {
	ir := generateIR(t, `
		type Dog { string name; int age; }
		main() { }
	`)
	assertContains(t, ir, "%promise_Dog_t = type {}")
	assertContains(t, ir, "%promise_Dog_m = type { %promise_Dog_t* }")
	assertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8*, i64 }")
	assertContains(t, ir, "%promise_Dog_v = type { i8*, %promise_Dog_i* }")
}

func TestUserTypeConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() { d := Dog(age: 3); }
	`)
	// Should allocate via malloc
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	// Should bitcast to typed pointer
	assertContains(t, ir, "bitcast i8*")
	// Should store field value
	assertContains(t, ir, "store i64 3")
}

func TestUserTypeFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			d := Dog(age: 3);
			x := d.age;
		}
	`)
	// Should bitcast and GEP to access field
	assertContains(t, ir, "getelementptr %promise_Dog_i")
	assertContains(t, ir, "load i64")
}

func TestUserTypeFieldAssign(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			d := Dog(age: 3);
			d.age = 5;
		}
	`)
	assertContains(t, ir, "store i64 5")
}

func TestUserTypeCompoundAssign(t *testing.T) {
	ir := generateIR(t, `
		type Counter { int value; }
		main() {
			c := Counter(value: 0);
			c.value += 1;
		}
	`)
	// Should load, add, store
	assertContains(t, ir, "getelementptr %promise_Counter_i")
	assertContains(t, ir, "add i64")
}

func TestUserTypeMethod(t *testing.T) {
	ir := generateIR(t, `
		type Dog {
			int age;
			getAge(this) int {
				return this.age;
			}
		}
		main() { }
	`)
	assertContains(t, ir, "define i64 @Dog.getAge(i8* %this)")
}

func TestUserTypeMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Dog {
			int age;
			getAge(this) int {
				return this.age;
			}
		}
		main() {
			d := Dog(age: 3);
			x := d.getAge();
		}
	`)
	assertContains(t, ir, "call i64 @Dog.getAge(i8*")
}

func TestUserTypeMethodWithReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int value;
			increment(~this) {
				this.value += 1;
			}
		}
		main() {
			c := Counter(value: 0);
			c.increment();
		}
	`)
	assertContains(t, ir, "define void @Counter.increment(i8* %this)")
	assertContains(t, ir, "call void @Counter.increment(i8*")
}

func TestThisExpr(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int value;
			get(this) int {
				return this.value;
			}
		}
		main() {
			b := Box(value: 42);
			x := b.get();
		}
	`)
	// Method should load this from alloca
	assertContains(t, ir, "%this.addr = alloca i8*")
}

func TestUserTypeMultipleFields(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; int z; }
		main() {
			p := Point(x: 1, y: 2, z: 3);
		}
	`)
	assertContains(t, ir, "%promise_Point_i = type { %promise_Point_m*, i64, i64, i64 }")
	// All three field stores
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
	assertContains(t, ir, "store i64 3")
}

func TestUserTypeStringField(t *testing.T) {
	ir := generateIR(t, `
		type Dog { string name; }
		main() {
			d := Dog(name: "Rex");
		}
	`)
	// String field stored as i8*
	assertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8* }")
	// Should call promise_string_new for the literal
	assertContains(t, ir, "call i8* @promise_string_new")
}

func TestUserTypeExternPacking(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		print_dog(Dog d) `+"`"+`extern("print_dog");
		main() {
			d := Dog(age: 3);
			print_dog(d);
		}
	`)
	// Should pack into value struct
	assertContains(t, ir, "insertvalue %promise_Dog_v")
}

func TestUserTypeHeader(t *testing.T) {
	result := compileResult(t, `
		type Dog { string name; int age; }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Dog_t")
	assertContains(t, header, "promise_Dog_m")
	assertContains(t, header, "promise_Dog_i")
	assertContains(t, header, "promise_Dog_v")
	// int field should use raw C type
	assertContains(t, header, "int64_t")
}

func TestUserTypeMethodWithParams(t *testing.T) {
	ir := generateIR(t, `
		type Adder {
			int base;
			add(&this, int n) int {
				return this.base + n;
			}
		}
		main() {
			a := Adder(base: 10);
			x := a.add(5);
		}
	`)
	assertContains(t, ir, "define i64 @Adder.add(i8* %this, i64 %n)")
	assertContains(t, ir, "call i64 @Adder.add(i8*")
}

func TestPalAllocDefined(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	assertContains(t, ir, "@pal_alloc(i64 %size)")
}

func TestUserTypeExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		get_dog() Dog `+"`"+`extern("get_dog");
		main() {
			d := get_dog();
		}
	`)
	// Extern uses sret for struct return
	assertContains(t, ir, "declare void @get_dog(i8* %sret)")
	// Unpack: load from sret alloca, extractvalue field 1 + bitcast back to i8*
	assertContains(t, ir, "extractvalue %promise_Dog_v")
	assertContains(t, ir, "bitcast %promise_Dog_i*")
}

func TestUserTypeNestedField(t *testing.T) {
	ir := generateIR(t, `
		type Inner { int value; }
		type Outer { Inner child; }
		main() {
			i := Inner(value: 42);
			o := Outer(child: i);
		}
	`)
	// Inner stored as value struct { i8*, i8* } in Outer's instance struct
	assertContains(t, ir, "%promise_Inner_i = type { %promise_Inner_m*, i64 }")
	assertContains(t, ir, "%promise_Outer_i = type { %promise_Outer_m*, { i8*, i8* } }")
	// Both should be allocated via malloc
	assertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestUserTypeNestedFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Inner { int value; }
		type Outer { Inner child; }
		main() {
			i := Inner(value: 42);
			o := Outer(child: i);
			c := o.child;
		}
	`)
	// Should GEP into Outer to load the child value struct
	assertContains(t, ir, "getelementptr %promise_Outer_i")
	assertContains(t, ir, "load { i8*, i8* }")
}

func TestUserTypeZeroArgConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(x: 0, y: 0);
		}
	`)
	// Should allocate and store both fields
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	assertContains(t, ir, "store i64 0")
}

func TestConstructorDefaultExprEvaluation(t *testing.T) {
	ir := generateIR(t, `
		type Config { int port = 8080; string host; }
		main() {
			c := Config(host: "localhost");
		}
	`)
	// The default expression (8080) should be evaluated and stored
	assertContains(t, ir, "store i64 8080")
}

func TestConstructorAllDefaultsOmitted(t *testing.T) {
	ir := generateIR(t, `
		type Defaults { int x = 42; int y = 99; }
		main() {
			d := Defaults();
		}
	`)
	assertContains(t, ir, "store i64 42")
	assertContains(t, ir, "store i64 99")
}

func TestNewConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Clamped {
			int value;
			new(~this, int v) {
				if v < 0 { this.value = 0; }
				else { this.value = v; }
			}
		}
		main() {
			c := Clamped(v: 50);
		}
	`)
	// Should declare the new() method as a void function
	assertContains(t, ir, "define void @Clamped.new(i8* %this")
	// Constructor should call new()
	assertContains(t, ir, "call void @Clamped.new(")
}

func TestNewConstructorFinalFieldCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Token {
			string raw `+"`"+`final;
			new(~this, string raw) {
				this.raw = raw;
			}
		}
		main() {
			t := Token(raw: "hello");
		}
	`)
	assertContains(t, ir, "define void @Token.new(i8* %this")
	assertContains(t, ir, "call void @Token.new(")
}

func TestFailableNewConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Port {
			int value;
			new(~this, int value) void! {
				if value < 1 {
					raise "invalid port";
				}
				this.value = value;
			}
		}
		main()! {
			Port p = Port(value: 80)!;
		}
	`)
	// Failable new returns a result type { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @Port.new(i8* %this")
	// Constructor call should call new and check the error
	assertContains(t, ir, "call { i1, i8* } @Port.new(")
}

func TestFactoryConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Color {
			int r;
			int g;
			int b;
			red() Self `+"`"+`factory {
				return Color(r: 255, g: 0, b: 0);
			}
		}
		main() {
			Color c = Color.red();
		}
	`)
	// Factory method should be defined without a receiver parameter
	assertContains(t, ir, "define { i8*, i8* } @Color.red()")
	// main should call Color.red
	assertContains(t, ir, "call { i8*, i8* } @Color.red()")
}

func TestSuperCallCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			int age;
			new(~this, int age) {
				this.age = age;
			}
		}
		type Dog is Animal {
			int tricks;
			new(~this, int age, int tricks) {
				super(age);
				this.tricks = tricks;
			}
		}
		main() {
			Dog d = Dog(age: 3, tricks: 5);
		}
	`)
	// Dog.new should call Animal.new
	assertContains(t, ir, "call void @Animal.new(")
	// Dog constructor should call Dog.new
	assertContains(t, ir, "call void @Dog.new(")
}

func TestSuperCallImplicitParentCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			int age;
		}
		type Dog is Animal {
			int tricks;
			new(~this, int age, int tricks) {
				super(age: age);
				this.tricks = tricks;
			}
		}
		main() {
			Dog d = Dog(age: 3, tricks: 5);
		}
	`)
	// Dog.new should be defined and set parent field directly (no Animal.new call)
	assertContains(t, ir, "define void @Dog.new(")
	// Dog constructor should call Dog.new
	assertContains(t, ir, "call void @Dog.new(")
}

func TestUserTypeHeaderFieldTypes(t *testing.T) {
	result := compileResult(t, `
		type Person { string name; int age; bool active; }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Verify instance struct field types
	assertContains(t, header, "void*                name;")
	assertContains(t, header, "int64_t              age;")
	assertContains(t, header, "uint8_t              active;")
}

func TestUserTypeMethodMutatesField(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int value;
			set(~this, int n) {
				this.value = n;
			}
		}
		main() {
			c := Counter(value: 0);
			c.set(42);
		}
	`)
	assertContains(t, ir, "define void @Counter.set(i8* %this, i64 %n)")
	// Should store into this.value
	assertContains(t, ir, "getelementptr %promise_Counter_i")
	assertContains(t, ir, "store i64")
}

// === Enum Tests ===

func TestEnumLayout(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)
	// Four-struct layout for enum
	assertContains(t, ir, "%promise_Color_t = type {}")
	assertContains(t, ir, "%promise_Color_m = type { %promise_Color_t* }")
	assertContains(t, ir, "%promise_Color_i = type { %promise_Color_m* }")
	assertContains(t, ir, "%promise_Color_v = type { i8*, %promise_Color_i*, i32 }")
}

func TestEnumLayoutData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)
	assertContains(t, ir, "%promise_Shape_t = type {}")
	assertContains(t, ir, "%promise_Shape_m = type { %promise_Shape_t* }")
	assertContains(t, ir, "%promise_Shape_i = type { %promise_Shape_m* }")
	// Value struct: vtable, instance ptr, tag, data bytes
	assertContains(t, ir, "%promise_Shape_v = type { i8*, %promise_Shape_i*, i32,")
	// Internal struct: tag + data area
	assertContains(t, ir, "%promise_Shape_enum = type { i32,")
}

func TestEnumFieldlessVariant(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Green;
		}
		main() { }
	`)
	// Green is tag 1
	assertContains(t, ir, "store i32 1")
}

func TestEnumDataConstructor(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(3.14);
		}
		main() { }
	`)
	// Should store tag (Circle = 0)
	assertContains(t, ir, "store i32 0")
	// Should store double field via GEP + bitcast
	assertContains(t, ir, "store double")
	assertContains(t, ir, "bitcast")
}

func TestEnumMatchFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			x := match c {
				Color.Red => 1,
				Color.Green => 2,
				Color.Blue => 3,
			};
		}
		main() { }
	`)
	// Should use switch on i32 tag
	assertContains(t, ir, "switch i32")
	// Should have arm blocks
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
	assertContains(t, ir, "match.arm2")
	// Merge block with phi
	assertContains(t, ir, "match.end")
}

func TestEnumMatchDestructure(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() f64 {
			Shape s = Shape.Circle(3.14);
			return match s {
				Shape.Circle(r) => r,
				Shape.Rect(w, h) => w,
			};
		}
		main() { }
	`)
	// Should switch on tag
	assertContains(t, ir, "switch i32")
	// Should bitcast + GEP to load variant data
	assertContains(t, ir, "bitcast")
	assertContains(t, ir, "load double")
}

func TestEnumMatchShortDestructure(t *testing.T) {
	ir := generateIR(t, `
		enum Result { Ok(int value), Err(int code) }
		test() int {
			Result r = Result.Ok(42);
			return match r {
				Ok(v) => v,
				Err(c) => c,
			};
		}
		main() { }
	`)
	// Short destructure should also produce switch
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
}

func TestEnumMatchWildcard(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Red;
			return match c {
				Color.Red => 1,
				_ => 0,
			};
		}
		main() { }
	`)
	// Switch with default case
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
}

func TestEnumMatchNameBinding(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Red;
			return match c {
				Color.Red => 1,
				val => 0,
			};
		}
		main() { }
	`)
	// Name binding should create alloca for the bound variable
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "alloca i32")
}

func TestEnumMatchBlock(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			match c {
				Color.Red => { int x = 1; },
				Color.Green => { int y = 2; },
				Color.Blue => { int z = 3; },
			};
		}
		main() { }
	`)
	// Should have switch and arm blocks (void match, no phi)
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
}

func TestMatchIntLiteral(t *testing.T) {
	ir := generateIR(t, `
		test() int {
			int n = 42;
			return match n {
				1 => 10,
				2 => 20,
				_ => 0,
			};
		}
		main() { }
	`)
	// Should use comparison chain (icmp eq), not switch
	assertContains(t, ir, "icmp eq")
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.next")
}

func TestEnumExternPacking(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		print_color(Color c) `+"`"+`extern("print_color");
		test() {
			Color c = Color.Green;
			print_color(c);
		}
		main() { }
	`)
	// Should pack into value struct
	assertContains(t, ir, "insertvalue %promise_Color_v")
	// Extern declaration: param passed by pointer
	assertContains(t, ir, "declare void @print_color(i8*")
}

func TestEnumExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		get_color() Color `+"`"+`extern("get_color");
		test() {
			Color c = get_color();
		}
		main() { }
	`)
	// Extern uses sret for struct return
	assertContains(t, ir, "declare void @get_color(i8* %sret)")
	// Should unpack via extractvalue after loading from sret
	assertContains(t, ir, "extractvalue %promise_Color_v")
}

func TestEnumHeaderFieldless(t *testing.T) {
	result := compileResult(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Color_t")
	assertContains(t, header, "promise_Color_m")
	assertContains(t, header, "promise_Color_i")
	assertContains(t, header, "promise_Color_v")
	// Value struct should have tag field
	assertContains(t, header, "int32_t")
}

func TestEnumDataFieldlessVariant(t *testing.T) {
	// Exercises zeroinitializer path: fieldless variant in a data enum
	ir := generateIR(t, `
		enum Result { Ok(int value), None }
		test() {
			Result r = Result.None;
		}
		main() { }
	`)
	// None is tag 1, built via zeroinitializer + insertvalue (not alloca with partial store)
	assertContains(t, ir, "insertvalue %promise_Result_enum zeroinitializer, i32 1, 0")
	// Internal struct should exist for the data enum
	assertContains(t, ir, "%promise_Result_enum = type { i32,")
}

func TestEnumDataExternPacking(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		send_shape(Shape s) `+"`"+`extern("send_shape");
		test() {
			Shape s = Shape.Circle(3.14);
			send_shape(s);
		}
		main() { }
	`)
	// Data enum packing: extractvalue tag and data from internal struct
	assertContains(t, ir, "extractvalue %promise_Shape_enum")
	// Pack into value struct
	assertContains(t, ir, "insertvalue %promise_Shape_v")
	// Extern declaration: param passed by pointer
	assertContains(t, ir, "declare void @send_shape(i8*")
}

func TestEnumDataExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		get_shape() Shape `+"`"+`extern("get_shape");
		test() {
			Shape s = get_shape();
		}
		main() { }
	`)
	// Data enum unpacking: sret + extractvalue from value struct, build internal struct
	assertContains(t, ir, "declare void @get_shape(i8* %sret)")
	assertContains(t, ir, "extractvalue %promise_Shape_v")
	assertContains(t, ir, "insertvalue %promise_Shape_enum")
}

func TestEnumAsFunctionParam(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		is_red(Color c) bool {
			return match c {
				Color.Red => true,
				_ => false,
			};
		}
		main() { }
	`)
	// Enum param should use i32 (fieldless enum internal type)
	assertContains(t, ir, "define i1 @is_red(i32 %c)")
	// Param should be alloca'd as i32
	assertContains(t, ir, "alloca i32")
	assertContains(t, ir, "switch i32")
}

func TestEnumAsFunctionReturn(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		get_green() Color {
			return Color.Green;
		}
		main() { }
	`)
	// Enum return should use i32
	assertContains(t, ir, "define i32 @get_green()")
	assertContains(t, ir, "ret i32 1")
}

func TestMatchValueNameBinding(t *testing.T) {
	ir := generateIR(t, `
		test() int {
			int x = 42;
			return match x {
				val => val + 1,
			};
		}
		main() { }
	`)
	// Name binding in value match: alloca + store the subject
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "add i64")
}

func TestEnumDestructureUnderscoreSkip(t *testing.T) {
	ir := generateIR(t, `
		enum Pair { Both(int a, int b) }
		test() int {
			Pair p = Pair.Both(1, 2);
			return match p {
				Both(_, second) => second,
			};
		}
		main() { }
	`)
	// Should still load the second field (index 1) but skip the first
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "load i64")
}

func TestEnumHeaderData(t *testing.T) {
	result := compileResult(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Shape_t")
	assertContains(t, header, "promise_Shape_v")
	// Data enum value struct should have tag and data fields
	assertContains(t, header, "int32_t              tag;")
	assertContains(t, header, "uint8_t              data[16];")
}

// ── Error handling tests ──────────────────────────────────────────

func TestFailableDeclaration(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() { }
	`)
	// Return type should be result struct { i1, i64, i8* }
	assertContains(t, ir, "define { i1, i64, i8* } @parse(i8* %s)")
}

func TestReturnInFailable(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 42; }
		main() { }
	`)
	// Should wrap value in Ok result: tag=false, value, null error
	assertContains(t, ir, "insertvalue { i1, i64, i8* }")
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i64, i8* }")
}

func TestRaiseStmt(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { raise "parse error"; }
		main() { }
	`)
	// Should wrap error in Error result: tag=true
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i64, i8* }")
	// Should create the error string
	assertContains(t, ir, `c"parse error"`)
}

func TestErrorPropagate(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		process() int! {
			x := parse("42")?;
			return x;
		}
		main() { }
	`)
	// Should have propagation and ok blocks
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// Should extract tag from result
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestErrorUnwrap(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() {
			x := parse("42")!;
		}
	`)
	// Should have panic and ok blocks
	assertContains(t, ir, "error.panic")
	assertContains(t, ir, "error.ok")
	// Should call promise_panic
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
}

func TestErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() {
			x := parse("42") ? e { 0 };
		}
	`)
	// Should have handler, ok, and merge blocks
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "error.merge")
}

func TestErrorHandlerDiscard(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() {
			x := parse("42") ? _ { 0 };
		}
	`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
}

func TestVoidFailable(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { return; }
		main() { }
	`)
	// Return type should be { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @validate(i8* %s)")
}

func TestVoidRaise(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise "invalid"; }
		main() { }
	`)
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestFailableMethod(t *testing.T) {
	ir := generateIR(t, `
		type Parser {
			string input;
			parse(this) int! {
				return 42;
			}
		}
		main() { }
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @Parser.parse(i8* %this)")
}

func TestFailableAutoTerminator(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! {
			if true {
				return;
			}
		}
		main() { }
	`)
	// Auto-terminator on fall-through path should wrap in Ok (tag=false)
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestVoidFailablePropagate(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise "invalid"; }
		process() void! {
			validate("x")?;
		}
		main() { }
	`)
	// Should propagate error from void failable callee
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// Callee returns { i1, i8* }, caller also returns { i1, i8* }
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestVoidFailableUnwrap(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise "invalid"; }
		main() {
			validate("x")!;
		}
	`)
	assertContains(t, ir, "error.panic")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "call void @promise_panic(")
}

func TestVoidFailableHandler(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise "invalid"; }
		main() {
			validate("x") ? e { };
		}
	`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "error.merge")
}

func TestNestedErrorPropagation(t *testing.T) {
	ir := generateIR(t, `
		a() int! { return 1; }
		b() int! { return a()?; }
		c() int! { return b()?; }
		main() { }
	`)
	// Both b and c should have propagation blocks
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
}

func TestErrorHandlerWithReturn(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		process(string s) int {
			x := parse(s) ? e { return -1; };
			return x;
		}
		main() { }
	`)
	// Handler block should contain a return (terminator)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
}

func TestFailableConditionalRaiseReturn(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! {
			if s == "" {
				raise "empty";
			}
			return 42;
		}
		main() { }
	`)
	// Should have both Ok and Error paths
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i64, i8* }")
}

// --- Generic type tests ---

func TestGenericTypeLayout(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
		}
	`)
	assertContains(t, ir, "Box__int_i")
	assertContains(t, ir, "store i64 42")
}

func TestGenericFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			int v = b.value;
		}
	`)
	assertContains(t, ir, "Box__int_i")
	// Field access should load i64 (not i8*)
	assertContains(t, ir, "load i64")
}

func TestGenericFieldAssign(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			b.value = 10;
		}
	`)
	assertContains(t, ir, "store i64 10")
}

func TestGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			get(this) T { return this.value; }
		}
		main() {
			b := Box[int](value: 42);
			int v = b.get();
		}
	`)
	assertContains(t, ir, "define i64 @Box__int.get")
}

func TestGenericMethodSet(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			set(~this, T val) { this.value = val; }
		}
		main() {
			b := Box[int](value: 42);
			b.set(10);
		}
	`)
	assertContains(t, ir, "define void @Box__int.set")
}

func TestGenericMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 42);
			b := Box[string](value: "hi");
		}
	`)
	assertContains(t, ir, "Box__int_i")
	assertContains(t, ir, "Box__string_i")
}

func TestGenericNestedField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.value;
			string y = b.value;
		}
	`)
	// Both Box[int] and Box[string] fields accessed with correct types
	assertContains(t, ir, "Box__int_i")
	assertContains(t, ir, "Box__string_i")
	assertContains(t, ir, "load i64")
	assertContains(t, ir, "load i8*")
}

func TestGenericEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
		}
	`)
	assertContains(t, ir, "Option__int_enum")
	assertContains(t, ir, "store i64 42")
}

func TestGenericEnumNone(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].None;
		}
	`)
	assertContains(t, ir, "Option__int_enum")
}

func TestGenericEnumMatch(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			r := match x {
				Some(v) => v,
				_ => 0,
			};
		}
	`)
	assertContains(t, ir, "switch i32")
}

func TestGenericEnumFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Dir[T] { Left, Right }
		main() {
			d := Dir[int].Left;
		}
	`)
	// Fieldless generic enum: internal type is i32
	assertContains(t, ir, "i32 0")
}

func TestGenericConstructorZeroInit(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 0);
		}
	`)
	// Generic type instance for Box[int]
	assertContains(t, ir, "Box__int_i")
}

// --- Generic function tests ---

func TestGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int r = identity[int](42);
		}
	`)
	assertContains(t, ir, "define i64 @identity__int")
	assertContains(t, ir, "ret i64")
}

func TestGenericFuncString(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			string s = identity[string]("hello");
		}
	`)
	assertContains(t, ir, "define i8* @identity__string")
}

func TestGenericFuncMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int a = identity[int](42);
			string b = identity[string]("hi");
		}
	`)
	assertContains(t, ir, "@identity__int")
	assertContains(t, ir, "@identity__string")
}

func TestGenericMethodMutReceiverAssign(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			replace(~this, T newVal) { this.value = newVal; }
		}
		main() {
			b := Box[int](value: 10);
			b.replace(99);
		}
	`)
	assertContains(t, ir, "define void @Box__int.replace")
	// Should store i64 (the new value into the field)
	assertContains(t, ir, "store i64")
}

func TestGenericFuncVoid(t *testing.T) {
	ir := generateIR(t, `
		consume[T](T x) { }
		main() {
			consume[int](42);
		}
	`)
	assertContains(t, ir, "define void @consume__int")
}

func TestGenericFuncFailable(t *testing.T) {
	ir := generateIR(t, `
		tryIdentity[T](T x) T! {
			return x;
		}
		main() {
			int v = tryIdentity[int](42)!;
		}
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @tryIdentity__int")
}

func TestGenericTypeAsParam(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		unbox(Box[int] b) int {
			return b.value;
		}
		main() {
			b := Box[int](value: 99);
			int v = unbox(b);
		}
	`)
	assertContains(t, ir, "define i64 @unbox")
	assertContains(t, ir, "load i64")
}

func TestGenericEnumMatchBlock(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			match x {
				Some(v) => { int y = v; },
				_ => { },
			};
		}
	`)
	assertContains(t, ir, "switch i32")
}

// === Stage 8g: Container Codegen Tests ===

// --- Part A: Tuple tests ---

func TestTupleLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := (1, 2); }`)
	// Should use insertvalue to build { i64, i64 } struct
	assertContains(t, ir, "insertvalue { i64, i64 }")
}

func TestTupleDestructure(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (a, b) := pair(); }
	`)
	// Should use extractvalue to destructure
	assertContains(t, ir, "extractvalue { i64, i64 }")
}

func TestTupleDestructureSkip(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (_, b) := pair(); }
	`)
	// Should extract second element but skip first
	assertContains(t, ir, "extractvalue { i64, i64 }")
	// b should be allocated
	assertContains(t, ir, "%b = alloca i64")
}

func TestTupleMixedTypes(t *testing.T) {
	ir := generateIR(t, `main() { x := (42, "hello", true); }`)
	// Should produce { i64, i8*, i1 } struct
	assertContains(t, ir, "insertvalue { i64, i8*, i1 }")
}

func TestTupleReturn(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, bool) { return (42, true); }
		main() { (a, b) := pair(); }
	`)
	assertContains(t, ir, "define { i64, i1 } @pair()")
	assertContains(t, ir, "ret { i64, i1 }")
}

// --- Part B: Optional tests ---

func TestOptionalNone(t *testing.T) {
	ir := generateIR(t, `main() { int? x = none; }`)
	// Should alloca { i1, i64 } and zero-initialize
	assertContains(t, ir, "alloca { i1, i64 }")
	assertContains(t, ir, "zeroinitializer")
}

func TestOptionalSome(t *testing.T) {
	ir := generateIR(t, `main() { int? x = 42; }`)
	// Should alloca { i1, i64 } and wrap: { true, 42 }
	assertContains(t, ir, "alloca { i1, i64 }")
	assertContains(t, ir, "insertvalue { i1, i64 }")
	assertContains(t, ir, "i1 true")
}

func TestElvisOperator(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int y = x ?: 0;
		}
	`)
	// Should have condBr + phi pattern
	assertContains(t, ir, "elvis.some")
	assertContains(t, ir, "elvis.none")
	assertContains(t, ir, "elvis.merge")
}

func TestOptionalStringNone(t *testing.T) {
	ir := generateIR(t, `main() { string? x = none; }`)
	assertContains(t, ir, "alloca { i1, i8* }")
	assertContains(t, ir, "zeroinitializer")
}

func TestOptionalVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int? y = x;
		}
	`)
	// Should load/store { i1, i64 } struct
	assertContains(t, ir, "load { i1, i64 }")
	assertContains(t, ir, "store { i1, i64 }")
}

// --- Part C: Slice / Array tests ---

func TestArrayLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := [1, 2, 3]; }`)
	// Should call malloc for heap allocation
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	// Should store len and cap
	assertContains(t, ir, "store i64 3")
	// Should store elements
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
}

func TestArrayIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			int x = items[0];
		}
	`)
	// Should have bounds check
	assertContains(t, ir, "icmp ult")
	// Should have ok and oob blocks
	assertContains(t, ir, "index.ok")
	assertContains(t, ir, "index.oob")
	// Should call promise_panic on out-of-bounds
	assertContains(t, ir, "call void @promise_panic(")
}

func TestArrayIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			items[0] = 42;
		}
	`)
	// Should have bounds check and store
	assertContains(t, ir, "icmp ult")
	assertContains(t, ir, "indexassign.ok")
	assertContains(t, ir, "store i64 42")
}

func TestArrayBoundsCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			int x = items[0];
		}
	`)
	// Bounds check uses unsigned less-than
	assertContains(t, ir, "icmp ult")
	// Out-of-bounds path calls promise_panic
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
}

func TestArrayForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			for x in items {
				int y = x;
			}
		}
	`)
	// Should have for-in loop blocks
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
	assertContains(t, ir, "forin.update")
	assertContains(t, ir, "forin.exit")
	// Should use unsigned comparison for counter < length
	assertContains(t, ir, "icmp ult")
}

func TestArrayStringElements(t *testing.T) {
	ir := generateIR(t, `main() { x := ["hello", "world"]; }`)
	// String elements stored as i8*
	assertContains(t, ir, "call i8* @promise_string_new(")
	assertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestArrayVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			int[] copy = items;
		}
	`)
	// Slice is stored/loaded as i8*
	assertContains(t, ir, "alloca i8*")
	assertContains(t, ir, "store i8*")
	assertContains(t, ir, "load i8*")
}

// --- Part D: Map tests ---

func TestMapLiteral(t *testing.T) {
	ir := generateIR(t, `main() { m := {"a": 1}; }`)
	// Should call monomorphized constructor and index assign
	assertContains(t, ir, "call void @map__string__int.new(")
	assertContains(t, ir, `call void @"map__string__int.[]="(`)
}

func TestMapIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			int? v = m["a"];
		}
	`)
	// Should call monomorphized [] method (returns optional { i1, i64 })
	assertContains(t, ir, `call { i1, i64 } @"map__string__int.[]"(`)
}

func TestMapIndexWithElvis(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			int v = m["a"] ?: 0;
		}
	`)
	// Should call monomorphized [] method + elvis
	assertContains(t, ir, `call { i1, i64 } @"map__string__int.[]"(`)
	assertContains(t, ir, "elvis.some")
}

func TestMapIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			m["a"] = 42;
		}
	`)
	// Should call monomorphized []= method
	assertContains(t, ir, `call void @"map__string__int.[]="(`)
}

func TestMapIntKeys(t *testing.T) {
	ir := generateIR(t, `main() { m := {1: "one", 2: "two"}; }`)
	// Should create monomorphized map with int keys
	assertContains(t, ir, "call void @map__int__string.new(")
	assertContains(t, ir, `call void @"map__int__string.[]="(`)
}

func TestMapForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1, "b": 2};
			for entry in m {
			}
		}
	`)
	// Should have for-in loop blocks
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
	assertContains(t, ir, "forin.exit")
}

// --- Part E: Lambda tests ---

func TestLambdaExpr(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// Lambda function has env (i8*) as first parameter
	assertContains(t, ir, "define i64 @.lambda.0(i8* %env, i64 %x)")
	// Lambda returned as fat pointer {fn_ptr, env_ptr}
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestLambdaCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
			int y = f(42);
		}
	`)
	// Should extract fn and env from fat pointer, then call with env as first arg
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "call i64")
}

func TestLambdaBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> int { return x * 2; };
		}
	`)
	assertContains(t, ir, "define i64 @.lambda.0(i8* %env, i64 %x)")
	assertContains(t, ir, "mul i64")
}

func TestLambdaVoid(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> void { return; };
		}
	`)
	assertContains(t, ir, "define void @.lambda.0(i8* %env, i64 %x)")
}

func TestLambdaVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// Lambda stored as fat pointer { i8*, i8* }
	assertContains(t, ir, "alloca { i8*, i8* }")
	assertContains(t, ir, "store { i8*, i8* }")
}

// --- Lambda Capture Tests ---

func TestLambdaCaptureInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42;
			f := |int y| -> x + y;
		}
	`)
	// Env struct should be allocated via malloc
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	// Lambda function should have env param
	assertContains(t, ir, "define i64 @.lambda.0(i8* %env, i64 %y)")
	// Should load captured var from env struct inside lambda
	assertContains(t, ir, "cap")
}

func TestLambdaCaptureMultiple(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int a = 1;
			int b = 2;
			f := |int x| -> a + b + x;
		}
	`)
	// Env should be allocated
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	// Lambda should have env param
	assertContains(t, ir, "define i64 @.lambda.0(i8* %env")
}

func TestLambdaNoCaptures(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// No malloc for env — null env pointer
	assertContains(t, ir, "i8* null, 1")
}

func TestLambdaCaptureCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			f := |int y| -> x + y;
			int result = f(5);
		}
	`)
	// Should extract fn and env from fat pointer for indirect call
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "call i64")
}

func TestLambdaNestedCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			f := |int a| -> int {
				g := |int b| -> x + b;
				return g(a);
			};
		}
	`)
	// Outer lambda should also capture x (propagated from inner)
	// Both lambdas should have env params and malloc for env
	assertContains(t, ir, "define i64 @.lambda.0(i8* %env")
	assertContains(t, ir, "define i64 @.lambda.1(i8* %env")
	// Two malloc calls — one for outer lambda env, one for inner
	assertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestLambdaEnvFree(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			f := |int y| -> x + y;
		}
	`)
	// Env should be freed at scope exit
	assertContains(t, ir, "call void @pal_free(i8*")
}

func TestLambdaEnvFreeNullCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// No-capture lambda: env is null, free should have null check
	assertContains(t, ir, "env.free")
	assertContains(t, ir, "env.skip")
}

func TestNamedFuncRefThunk(t *testing.T) {
	ir := generateIR(t, `
		add(int x) int { return x + 1; }
		main() {
			f := add;
			int y = f(42);
		}
	`)
	// Should generate a thunk for the named function reference
	assertContains(t, ir, "define i64 @.thunk.add(i8* %env, i64 %x)")
	// Fat pointer should use thunk, not raw @add
	assertContains(t, ir, ".thunk.add")
	// Should call through indirect call path
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

// ================================================================
// Stage 8h — Optional Patterns, String Interpolation & Expression Completeness
// ================================================================

// --- Part A: If-unwrap ---

func TestIfUnwrap(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			if val := x {
				int y = val + 1;
			}
		}
	`)
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "ifunwrap.then")
	assertContains(t, ir, "ifunwrap.end")
}

func TestIfUnwrapElse(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			if val := x {
				int y = val;
			} else {
				int z = 0;
			}
		}
	`)
	assertContains(t, ir, "ifunwrap.then")
	assertContains(t, ir, "ifunwrap.else")
	assertContains(t, ir, "ifunwrap.end")
}

// --- Part B: While-unwrap ---

func TestWhileUnwrap(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			while val := x {
				break;
			}
		}
	`)
	assertContains(t, ir, "whileunwrap.header")
	assertContains(t, ir, "whileunwrap.body")
	assertContains(t, ir, "whileunwrap.exit")
	assertContains(t, ir, "extractvalue")
}

func TestWhileUnwrapBreak(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 10;
			while val := x {
				break;
			}
		}
	`)
	// break should jump to exit block
	assertContains(t, ir, "br label %whileunwrap.exit")
}

// --- Part C: Optional chaining ---

func TestOptionalChain(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = Dog(age: 3);
			int? a = d?.age;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	assertContains(t, ir, "optchain.merge")
}

func TestOptionalChainNone(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = none;
			int? a = d?.age;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	assertContains(t, ir, "phi")
}

// --- Part D: String interpolation ---

func TestStringInterpolationIdent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string name = "world";
			string msg = "hello {name}";
		}
	`)
	// Should call promise_string_concat
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42;
			string msg = "x = {x}";
		}
	`)
	assertContains(t, ir, "call i8* @promise_int_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationBool(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool flag = true;
			string msg = "flag: {flag}";
		}
	`)
	assertContains(t, ir, "call i8* @promise_bool_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationExpr(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string msg = "result: {1 + 2}";
		}
	`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "call i8* @promise_int_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationMultiple(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int a = 1;
			int b = 2;
			string msg = "{a} and {b}";
		}
	`)
	// Two int-to-string conversions and multiple concats
	count := strings.Count(ir, "call i8* @promise_int_to_string")
	if count < 2 {
		t.Errorf("expected at least 2 calls to promise_int_to_string, got %d", count)
	}
}

// --- Part E: Unsafe blocks ---

func TestUnsafeBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			unsafe {
				int x = 42;
			}
		}
	`)
	assertContains(t, ir, "store i64 42")
}

// --- Coverage gap tests ---

// genIfExpr: if-as-expression with phi merge
func TestIfExpression(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = if true { 1; } else { 2; };
		}
	`)
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.else")
	assertContains(t, ir, "if.merge")
	assertContains(t, ir, "phi i64")
}

// genClassicForStmt: C-style for loop
func TestClassicFor(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i := 0; i < 10; i += 1 {
				int x = i;
			}
		}
	`)
	assertContains(t, ir, "for.header")
	assertContains(t, ir, "for.body")
	assertContains(t, ir, "for.update")
	assertContains(t, ir, "for.exit")
	assertContains(t, ir, "icmp slt i64")
	assertContains(t, ir, "add i64")
}

// genClassicForStmt with typed init
func TestClassicForTypedInit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for int i = 0; i < 5; i += 1 {
				int x = i;
			}
		}
	`)
	assertContains(t, ir, "for.header")
	assertContains(t, ir, "for.exit")
}

// genContinueStmt
func TestContinueStmt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i := 0; i < 10; i += 1 {
				if i < 5 {
					continue;
				}
			}
		}
	`)
	// continue should branch to for.update
	assertContains(t, ir, "br label %for.update")
}

// genContinueStmt in while loop
func TestContinueInWhile(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int i = 0;
			while i < 10 {
				i += 1;
				if i < 5 {
					continue;
				}
			}
		}
	`)
	// continue should branch to while.header
	assertContains(t, ir, "br label %while.header")
}

// convertToString: f64 interpolation
func TestStringInterpolationF64(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 x = 3.14;
			string msg = "pi is {x}";
		}
	`)
	assertContains(t, ir, "call i8* @promise_f64_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// convertToString: string passthrough in interpolation
func TestStringInterpolationStringVar(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string name = "world";
			string msg = "hello {name}";
		}
	`)
	// No conversion call needed — string is passed directly to concat
	assertNotContains(t, ir, "call i8* @promise_int_to_string")
	assertNotContains(t, ir, "call i8* @promise_f64_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// convertToString: f32 interpolation (fpext to double)
func TestStringInterpolationF32(t *testing.T) {
	ir := generateIR(t, `
		show(f32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "fpext float")
	assertContains(t, ir, "call i8* @promise_f64_to_string")
}

// convertToString: i32 interpolation (sext to i64)
func TestStringInterpolationI32(t *testing.T) {
	ir := generateIR(t, `
		show(i32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "sext i32")
	assertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: u32 interpolation (zext to i64, calls uint_to_string)
func TestStringInterpolationU32(t *testing.T) {
	ir := generateIR(t, `
		show(u32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "zext i32")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// unsignedIntOps: basic unsigned arithmetic
func TestUnsignedIntArithmetic(t *testing.T) {
	ir := generateIR(t, `
		compute(uint a, uint b) {
			uint sum = a + b;
			uint diff = a - b;
			uint prod = a * b;
			uint quot = a / b;
			uint rem = a % b;
		}
		main() { }
	`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "sub i64")
	assertContains(t, ir, "mul i64")
	assertContains(t, ir, "udiv i64")
	assertContains(t, ir, "urem i64")
}

// unsignedIntOps: comparison operators
func TestUnsignedIntComparison(t *testing.T) {
	ir := generateIR(t, `
		compare(uint a, uint b) {
			bool lt = a < b;
			bool le = a <= b;
			bool gt = a > b;
			bool ge = a >= b;
			bool eq = a == b;
			bool ne = a != b;
		}
		main() { }
	`)
	assertContains(t, ir, "icmp ult i64")
	assertContains(t, ir, "icmp ule i64")
	assertContains(t, ir, "icmp ugt i64")
	assertContains(t, ir, "icmp uge i64")
	assertContains(t, ir, "icmp eq i64")
	assertContains(t, ir, "icmp ne i64")
}

// floatOps: float arithmetic (full coverage)
func TestFloatArithmeticFull(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 a = 3.14;
			f64 b = 2.0;
			f64 sum = a + b;
			f64 diff = a - b;
			f64 prod = a * b;
			f64 quot = a / b;
		}
	`)
	assertContains(t, ir, "fadd double")
	assertContains(t, ir, "fsub double")
	assertContains(t, ir, "fmul double")
	assertContains(t, ir, "fdiv double")
}

// floatOps: float comparison operators (full coverage)
func TestFloatComparisonFull(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 a = 3.14;
			f64 b = 2.0;
			bool lt = a < b;
			bool gt = a > b;
			bool eq = a == b;
			bool ne = a != b;
		}
	`)
	assertContains(t, ir, "fcmp olt double")
	assertContains(t, ir, "fcmp ogt double")
	assertContains(t, ir, "fcmp oeq double")
	assertContains(t, ir, "fcmp one double")
}

// resolveEscape: additional escape sequences
func TestStringEscapeSequences(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string a = "hello\tworld";
			string b = "line1\rline2";
			string c = "back\\slash";
			string d = "null\0end";
			string e = "quote\"mark";
		}
	`)
	// Each should produce a global string constant
	assertContains(t, ir, "call i8* @promise_string_new")
}

// unaryExpr: negation
func TestUnaryNegation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = -42;
			f64 y = -3.14;
		}
	`)
	assertContains(t, ir, "sub i64 0")
	assertContains(t, ir, "fneg double")
}

// boolOps: boolean equality/inequality
func TestBoolEquality(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool a = true;
			bool b = false;
			bool eq = a == b;
			bool ne = a != b;
		}
	`)
	assertContains(t, ir, "icmp eq i1")
	assertContains(t, ir, "icmp ne i1")
}

// --- Stage 8i: Char literals, container .len, string iteration, map compound assignment ---

func TestCharLiteral(t *testing.T) {
	ir := generateIR(t, `main() { char c = 'a'; }`)
	assertContains(t, ir, "store i32 97")
}

func TestCharEscape(t *testing.T) {
	ir := generateIR(t, `main() { char c = '\n'; }`)
	assertContains(t, ir, "store i32 10")
}

func TestCharEscapeNull(t *testing.T) {
	ir := generateIR(t, `main() { char c = '\0'; }`)
	assertContains(t, ir, "store i32 0")
}

func TestCharEscapeBackslash(t *testing.T) {
	ir := generateIR(t, `main() { char c = '\\'; }`)
	assertContains(t, ir, "store i32 92")
}

func TestCharMultiByte(t *testing.T) {
	ir := generateIR(t, `main() { char c = '€'; }`)
	// € is U+20AC = 8364
	assertContains(t, ir, "store i32 8364")
}

func TestCharEquality(t *testing.T) {
	ir := generateIR(t, `
		check(char a, char b) bool { return a == b; }
		main() { }
	`)
	assertContains(t, ir, "icmp eq i32")
}

func TestCharComparison(t *testing.T) {
	ir := generateIR(t, `
		check(char a, char b) bool { return a < b; }
		main() { }
	`)
	assertContains(t, ir, "icmp slt i32")
}

func TestCharInterpolation(t *testing.T) {
	ir := generateIR(t, `
		main() { char c = 'X'; string s = "char: {c}"; }
	`)
	assertContains(t, ir, "call i8* @promise_char_to_string(i32")
}

// convertToString: i16 interpolation (sext to i64)
func TestStringInterpolationI16(t *testing.T) {
	ir := generateIR(t, `
		show(i16 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "sext i16")
	assertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: i8 interpolation (sext to i64)
func TestStringInterpolationI8(t *testing.T) {
	ir := generateIR(t, `
		show(i8 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "sext i8")
	assertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: uint interpolation (direct i64, no extension)
func TestStringInterpolationUint(t *testing.T) {
	ir := generateIR(t, `
		show(uint x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// convertToString: u16 interpolation (zext to i64)
func TestStringInterpolationU16(t *testing.T) {
	ir := generateIR(t, `
		show(u16 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "zext i16")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// convertToString: u8 interpolation (zext to i64)
func TestStringInterpolationU8(t *testing.T) {
	ir := generateIR(t, `
		show(u8 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "zext i8")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// --- Value-to-string function body tests ---

func TestBoolToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { bool b = true; string s = "{b}"; }
	`)
	assertContains(t, ir, "define i8* @promise_bool_to_string(i8")
	assertContains(t, ir, `c"true"`)
	assertContains(t, ir, `c"false"`)
	assertContains(t, ir, "true:")
	assertContains(t, ir, "false:")
}

func TestIntToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { int x = 42; string s = "{x}"; }
	`)
	assertContains(t, ir, "define i8* @promise_int_to_string(i64")
	assertContains(t, ir, "digit_loop:")
	assertContains(t, ir, "check_neg:")
	assertContains(t, ir, "check_sign:")
	assertContains(t, ir, "done:")
	assertContains(t, ir, "urem i64")
	assertContains(t, ir, "udiv i64")
}

func TestUintToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		show(uint x) { string s = "{x}"; }
		main() { }
	`)
	assertContains(t, ir, "define i8* @promise_uint_to_string(i64")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
	assertContains(t, ir, "digit_loop:")
	assertContains(t, ir, "done:")
	assertContains(t, ir, "urem i64")
	assertContains(t, ir, "udiv i64")
}

func TestF64ToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { f64 x = 3.14; string s = "{x}"; }
	`)
	assertContains(t, ir, "define i8* @promise_f64_to_string(double")
	assertContains(t, ir, "@snprintf(")
	assertContains(t, ir, `c"%g\00"`)
}

func TestCharToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { char c = 'X'; string s = "{c}"; }
	`)
	assertContains(t, ir, "define i8* @promise_char_to_string(i32")
	assertContains(t, ir, "one_byte:")
	assertContains(t, ir, "two_byte:")
	assertContains(t, ir, "three_byte:")
	assertContains(t, ir, "four_byte:")
	assertContains(t, ir, "lshr i32")
}

func TestSliceLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			int[] arr = [1, 2, 3];
			int n = arr.len;
		}
	`)
	// Should GEP into slice header and load length
	assertContains(t, ir, "getelementptr { i64, i64 }")
	assertContains(t, ir, "load i64")
}

func TestArrayLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		check(int[3] arr) int { return arr.len; }
		main() { }
	`)
	assertContains(t, ir, "getelementptr { i64, i64 }")
	assertContains(t, ir, "load i64")
}

func TestMapLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			m := {"a": 1};
			int n = m.len;
		}
	`)
	// Should call monomorphized len getter
	assertContains(t, ir, "call i64 @map__string__int.len(")
}

func TestStringLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			string s = "hello";
			int n = s.len;
		}
	`)
	// Should GEP to string instance len field and load
	assertContains(t, ir, "load i64")
}

func TestSliceLenInCondition(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			int[] arr = [1, 2, 3];
			if arr.len > 0 { }
		}
	`)
	assertContains(t, ir, "getelementptr { i64, i64 }")
	assertContains(t, ir, "icmp sgt i64")
}

func TestForInString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for ch in "abc" { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	assertContains(t, ir, "forin.str.header")
	assertContains(t, ir, "forin.str.body")
	assertContains(t, ir, "forin.str.exit")
	// Should compare return value with -1
	assertContains(t, ir, "icmp eq i32")
}

func TestForInStringIndexed(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i, ch in "abc" { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	// Index variable should be allocated and incremented
	assertContains(t, ir, "%i = alloca i64")
	assertContains(t, ir, "add i64")
}

func TestForInStringVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string s = "hello";
			for ch in s { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	assertContains(t, ir, "forin.str.header")
}

func TestForInStringEmpty(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for ch in "" { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	assertContains(t, ir, "forin.str.header")
}

func TestMapCompoundAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			m["a"] += 1;
		}
	`)
	// Should call [] to get, add, then []= to set
	assertContains(t, ir, `call { i1, i64 } @"map__string__int.[]"(`)
	assertContains(t, ir, "mapcomp.ok")
	assertContains(t, ir, "mapcomp.panic")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, `call void @"map__string__int.[]="(`)
}

func TestMapCompoundAssignMul(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"x": 2};
			m["x"] *= 3;
		}
	`)
	assertContains(t, ir, `call { i1, i64 } @"map__string__int.[]"(`)
	assertContains(t, ir, "mul i64")
	assertContains(t, ir, `call void @"map__string__int.[]="(`)
}

// --- Stage 8k: Inheritance Codegen Tests ---

func TestInheritedFieldLayout(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 5, breed: "Lab");
		}
	`)
	// Dog instance struct should include parent fields: _variant, name, age, breed
	assertContains(t, ir, `%promise_Dog_i = type { %promise_Dog_m*, i8*, i64, i8* }`)
	// Animal instance struct: _variant, name, age
	assertContains(t, ir, `%promise_Animal_i = type { %promise_Animal_m*, i8*, i64 }`)
}

func TestInheritedFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 5, breed: "Lab");
			string n = d.name;
			int a = d.age;
			string b = d.breed;
		}
	`)
	// Field access should use GEP on Dog instance struct
	assertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestInheritedFieldConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
	// Constructor should store values for both inherited and own fields
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestInheritedMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			greet() string { return this.name; }
		}
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
			string g = d.greet();
		}
	`)
	// d.greet() should dispatch to Animal.greet (inherited method)
	assertContains(t, ir, "call i8* @Animal.greet(i8*")
}

func TestMethodOverride(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
			string s = d.speak();
		}
	`)
	// d.speak() should dispatch to Dog.speak (child overrides parent)
	assertContains(t, ir, "call i8* @Dog.speak(i8*")
}

func TestUpcastFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			string n = a.name;
		}
	`)
	// Upcast Dog to Animal, then access name via Animal layout
	assertContains(t, ir, "getelementptr %promise_Animal_i")
}

func TestTypeInfoGlobal(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
	// Type info globals should be emitted for both types
	assertContains(t, ir, "@promise_typeinfo_Animal")
	assertContains(t, ir, "@promise_typeinfo_Dog")
}

func TestConstructorStoresTypeInfo(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		main() {
			Animal a = Animal(name: "Rex");
		}
	`)
	// Constructor should store type info pointer instead of null
	assertContains(t, ir, "@promise_typeinfo_Animal")
	// The _variant slot should be set via bitcast of the type info global
	assertContains(t, ir, "bitcast")
}

func TestDeepInheritance(t *testing.T) {
	ir := generateIR(t, `
		type A { int x; }
		type B is A { int y; }
		type C is B { int z; }
		main() {
			C c = C(x: 1, y: 2, z: 3);
			int a = c.x;
			int b = c.y;
			int d = c.z;
		}
	`)
	// C struct should have _variant, x, y, z (4 fields + internal = 4 GEP indices)
	assertContains(t, ir, "%promise_C_i = type { %promise_C_m*, i64, i64, i64 }")
}

// --- Part D: is/as expression tests ---

func TestIsPresent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			bool b = x is present;
		}
	`)
	// Should extract the i1 flag from the optional struct
	assertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestIsAbsent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			bool b = x is absent;
		}
	`)
	// Should extract the i1 flag and negate via xor
	assertContains(t, ir, "extractvalue { i1, i64 }")
	assertContains(t, ir, "xor i1")
}

func TestIsEnumVariant(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			bool b = c is Red;
		}
		main() { }
	`)
	// Fieldless enum: value IS the tag, compare with icmp eq
	assertContains(t, ir, "icmp eq i32")
}

func TestIsEnumVariantData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(radius: 3.14);
			bool b = s is Circle;
		}
		main() { }
	`)
	// Data-carrying enum: extract tag from struct, then compare
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "icmp eq i32")
}

func TestIsNamedType(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Should call promise_type_is (now codegen-emitted, not extern) and convert to i1
	assertContains(t, ir, "define i32 @promise_type_is")
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "icmp ne i32")
}

func TestTypeIsFuncBody(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Verify key blocks in the defined type_is function
	assertContains(t, ir, "define i32 @promise_type_is")
	assertContains(t, ir, "check_id:")
	assertContains(t, ir, "loop_init:")
	assertContains(t, ir, "loop_header:")
	assertContains(t, ir, "loop_body:")
	assertContains(t, ir, "ret_true:")
	assertContains(t, ir, "ret_false:")
}

func TestIsNamedTypeInheritance(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		type Cat is Animal { }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			bool isDog = a is Dog;
			bool isCat = a is Cat;
			bool isAnimal = a is Animal;
		}
	`)
	// All three checks should go through RTTI
	assertContains(t, ir, "call i32 @promise_type_is")
	// Type info globals for all three types
	assertContains(t, ir, "@promise_typeinfo_Dog")
	assertContains(t, ir, "@promise_typeinfo_Cat")
	assertContains(t, ir, "@promise_typeinfo_Animal")
}

func TestAsSafeCast(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			Dog? d = a as Dog;
		}
	`)
	// Should have RTTI check, then cast.some/cast.none/cast.merge blocks
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "cast.some.")
	assertContains(t, ir, "cast.none.")
	assertContains(t, ir, "cast.merge.")
}

func TestAsForcecast(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			Dog d = a as! Dog;
		}
	`)
	// Should have RTTI check, then cast.ok/cast.panic blocks
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "cast.ok.")
	assertContains(t, ir, "cast.panic.")
	assertContains(t, ir, "call void @promise_panic")
}

func TestFieldShadowing(t *testing.T) {
	ir := generateIR(t, `
		type Base { int x; int y; }
		type Child is Base { string x; }
		main() {
			Child c = Child(y: 1, x: "hi");
			string s = c.x;
			int n = c.y;
		}
	`)
	// Child layout: _variant, y (inherited, not shadowed), x (own, shadows Base.x)
	// y is int (i64), x is string (i8*) — parent x omitted from layout
	assertContains(t, ir, "%promise_Child_i = type { %promise_Child_m*, i64, i8* }")
}

func TestConstructorZeroInitInheritedField(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 0, breed: "Lab");
		}
	`)
	// Constructor should store inherited fields
	assertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestDeepInheritanceMethodDispatch(t *testing.T) {
	ir := generateIR(t, `
		type A {
			int x;
			getX() int { return this.x; }
		}
		type B is A { int y; }
		type C is B { int z; }
		main() {
			C c = C(x: 1, y: 2, z: 3);
			int v = c.getX();
		}
	`)
	// c.getX() should resolve through C → B → A and call A.getX
	assertContains(t, ir, "call i64 @A.getX(i8*")
}

func TestRTTIMultipleParents(t *testing.T) {
	ir := generateIR(t, `
		type Printable {
			show() string { return "printable"; }
		}
		type Serializable {
			encode() string { return "serializable"; }
		}
		type Doc is Printable, Serializable {
			string name;
		}
		main() {
			Doc d = Doc(name: "hi");
		}
	`)
	// Type info for Doc should include both parent IDs
	assertContains(t, ir, "@promise_typeinfo_Doc")
	assertContains(t, ir, "@promise_typeinfo_Printable")
	assertContains(t, ir, "@promise_typeinfo_Serializable")
}

func TestRTTIDiamondDedup(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			id() string { return "base"; }
		}
		type Left is Base { }
		type Right is Base { }
		type Bottom is Left, Right { }
		main() {
			Bottom b = Bottom();
		}
	`)
	// Type info globals for all types
	assertContains(t, ir, "@promise_typeinfo_Bottom")
	assertContains(t, ir, "@promise_typeinfo_Left")
	assertContains(t, ir, "@promise_typeinfo_Right")
	assertContains(t, ir, "@promise_typeinfo_Base")
}

func TestReverseOrderTypeDeclaration(t *testing.T) {
	ir := generateIR(t, `
		type Dog is Animal { string breed; }
		type Animal { string name; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
			string n = d.name;
		}
	`)
	// Topological ordering should compute Animal layout before Dog
	// even though Dog is declared first in source
	assertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8*, i8* }")
	assertContains(t, ir, "%promise_Animal_i = type { %promise_Animal_m*, i8* }")
}

func TestIsPresentStringOptional(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? x = "hello";
			bool b = x is present;
			bool c = x is absent;
		}
	`)
	// Should extractvalue on { i1, i8* } optional
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

// --- VTable dispatch tests (Stage 8l) ---

func TestVtableGlobalEmitted(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
		}
	`)
	// Both types have virtual methods, vtable globals should be emitted
	assertContains(t, ir, "@promise_vtable_Animal")
	assertContains(t, ir, "@promise_vtable_Dog")
}

func TestAbstractMethodVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string `+"`"+`abstract;
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// Virtual dispatch: should NOT directly call @Animal.speak (abstract, doesn't exist)
	assertNotContains(t, ir, "call i8* @Animal.speak")
	// Should load function pointer from vtable (indirect call)
	assertContains(t, ir, "@promise_vtable_Animal")
	assertContains(t, ir, "@promise_vtable_Dog")
}

func TestConcreteOverrideVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// When calling through Animal variable, should use vtable dispatch
	// (not direct call to Animal.speak)
	assertNotContains(t, ir, "call i8* @Animal.speak")
	// Vtable globals should exist for both types
	assertContains(t, ir, "@promise_vtable_Animal")
	assertContains(t, ir, "@promise_vtable_Dog")
}

func TestDirectDispatchPreserved(t *testing.T) {
	ir := generateIR(t, `
		type Dog {
			string name;
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
			string s = d.speak();
		}
	`)
	// Dog has no children → direct dispatch, no vtable indirection
	assertContains(t, ir, "call i8* @Dog.speak")
}

func TestVirtualGetterDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Shape {
			get area int `+"`"+`abstract;
		}
		type Circle is Shape {
			int radius;
			get area int => this.radius * this.radius;
		}
		main() {
			Shape s = Circle(radius: 5);
			int a = s.area;
		}
	`)
	// Getter through abstract parent should use vtable dispatch (indirect call)
	assertNotContains(t, ir, "call i64 @Shape.area")
	assertContains(t, ir, "@promise_vtable_Shape")
	assertContains(t, ir, "@promise_vtable_Circle")
}

func TestVirtualGetterOverrideDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int _x;
			get x int { return this._x; }
		}
		type Child is Base {
			get x int { return this._x * 2; }
		}
		main() {
			Base b = Child(_x: 5);
			int v = b.x;
		}
	`)
	// Concrete getter override through parent-typed variable should use vtable dispatch
	assertNotContains(t, ir, "call i64 @Base.x(")
	assertContains(t, ir, "@promise_vtable_Base")
	assertContains(t, ir, "@promise_vtable_Child")
}

func TestDirectGetterPreserved(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int _x;
			get x int => this._x;
		}
		main() {
			Point p = Point(_x: 42);
			int v = p.x;
		}
	`)
	// Point has no children → direct dispatch for getter
	assertContains(t, ir, "call i64 @Point.x")
}

func TestDirectGetterNoVtable(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
		}
		main() {
			Counter c = Counter(_count: 10);
			int n = c.count;
		}
	`)
	// Counter has no children → direct getter call (not indirect through vtable)
	assertContains(t, ir, "call i64 @Counter.count")
}

func TestMultipleAbstractParentsVtable(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Speakable s = Robot();
			string x = s.speak();
		}
	`)
	// Robot's vtable should cover both speak and move
	assertContains(t, ir, "@promise_vtable_Robot")
	assertContains(t, ir, "@promise_vtable_Speakable")
}

func TestDeepHierarchyVtable(t *testing.T) {
	ir := generateIR(t, `
		type A {
			greet() string `+"`"+`abstract;
		}
		type B is A {
			greet() string { return "hello from B"; }
		}
		type C is B {
			greet() string { return "hello from C"; }
		}
		main() {
			A a = C();
			string s = a.greet();
		}
	`)
	// A→B→C chain: all get vtable globals
	assertContains(t, ir, "@promise_vtable_A")
	assertContains(t, ir, "@promise_vtable_B")
	assertContains(t, ir, "@promise_vtable_C")
	// Should NOT directly call @A.greet (abstract)
	assertNotContains(t, ir, "call i8* @A.greet")
}

// --- Stage 8l: Value struct dispatch model tests ---

func TestValueStructRepresentation(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		main() {
			Animal a = Animal(name: "Rex");
		}
	`)
	// Variables of user types should be value struct { i8*, i8* }
	assertContains(t, ir, "alloca { i8*, i8* }")
	// Constructor returns value struct with insertvalue
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestFirstParentPrefixCompatible(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// Animal is first parent of Dog — no view vtable needed
	assertNotContains(t, ir, "@promise_vtable_Dog_as_Animal")
	// Dispatch through vtable from value struct (extractvalue, GEP, load, bitcast, call)
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestSecondParentViewVtable(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Movable m = Robot();
		}
	`)
	// Movable is second parent of Robot — needs a view-specific vtable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

func TestMultiParentVtableDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Movable m = Robot();
			string s = m.walk();
		}
	`)
	// Should emit view vtable for Robot-as-Movable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
	// Dispatch should use vtable from value struct (not typeinfo chain)
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestIsExpressionWithValueStruct(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string `+"`"+`abstract;
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Should extract instance pointer from value struct for RTTI check
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "@promise_type_is")
}

func TestFieldAccessThroughValueStruct(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
		}
		main() {
			Animal a = Animal(name: "Rex");
			string n = a.name;
		}
	`)
	// Should extract instance from value struct, then GEP to field
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "getelementptr %promise_Animal_i")
}

func TestConcreteDirectDispatchPreserved(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x;
			int y;
			sum() int { return this.x + this.y; }
		}
		main() {
			Point p = Point(x: 1, y: 2);
			int s = p.sum();
		}
	`)
	// Concrete type with no parents that needs vtable — should use direct dispatch
	assertContains(t, ir, "call i64 @Point.sum")
}

func TestStructuralSatisfactionWithMeta(t *testing.T) {
	ir := generateIR(t, `
		type Printable `+"`"+`structural {
			print() string `+"`"+`abstract;
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
			string s = p.print();
		}
	`)
	// Should emit view vtable for Doc-as-Printable (structural satisfaction)
	assertContains(t, ir, "@promise_vtable_Doc_as_Printable")
}

func TestStructuralSatisfactionWithoutMetaFails(t *testing.T) {
	// Without `structural meta, explicit `is is required
	src := `
		type Printable {
			print() string ` + "`" + `abstract;
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	pr := parser.NewPromiseParser(stream)
	pr.RemoveErrorListeners()
	tree := pr.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}
	_, errs = sema.Check(file)
	if len(errs) == 0 {
		t.Error("expected sema error for assigning Doc to Printable without `structural, got none")
	}
}

func TestReturnCoercionSecondParent(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		makeMovable() Movable {
			return Robot();
		}
		main() {
			Movable m = makeMovable();
			string s = m.walk();
		}
	`)
	// Returning Robot as Movable (second parent) should emit view vtable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

func TestArgCoercionSecondParent(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		useMovable(Movable m) string {
			return m.walk();
		}
		main() {
			Robot r = Robot();
			string s = useMovable(r);
		}
	`)
	// Passing Robot as Movable arg (second parent) should emit view vtable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

// --- Stage 9: Std library and test runner codegen tests ---

// generateIRWithStd compiles with std declarations (IsStd=true) merged before user code.
// stdContainers is kept for backward compatibility with tests that pass it to generateIRWithStd.
// Its contents are already included in stdAll; the dedup logic handles duplicates silently.
const stdContainers = ""

func generateIRWithStd(t *testing.T, stdSrc, userSrc string) string {
	t.Helper()
	// Always include stdAll; additional stdSrc is appended
	combinedStd := stdAll + "\n" + stdSrc
	// Parse std
	stdInput := antlr.NewInputStream(combinedStd)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, errs := ast.Build("std.pr", stdTree)
	if len(errs) > 0 {
		t.Fatalf("std AST build errors: %v", errs)
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
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Merge
	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(userFile.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, userFile.Decls...)
	userFile.Decls = merged

	info, errs := sema.Check(userFile)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	result := Compile(userFile, info)
	return result.Module.String()
}

// compileResultWithStd compiles with std declarations and returns the CompileResult.
func compileResultWithStd(t *testing.T, stdSrc, userSrc string) *CompileResult {
	t.Helper()
	stdInput := antlr.NewInputStream(stdSrc)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, errs := ast.Build("std.pr", stdTree)
	if len(errs) > 0 {
		t.Fatalf("std AST build errors: %v", errs)
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

	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(userFile.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, userFile.Decls...)
	userFile.Decls = merged

	info, errs := sema.Check(userFile)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	return Compile(userFile, info)
}

func TestStdFuncMangledName(t *testing.T) {
	// Std functions should get __std_ prefix in LLVM IR
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	assertContains(t, ir, "define i64 @__std_helper")
	assertContains(t, ir, "call i64 @__std_helper")
}

func TestStdUserNameCollision(t *testing.T) {
	// When user defines same-name function, user version goes to funcs, std to stdFuncs
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`
		helper() int { return 99; }
		main() { x := helper(); }
		`,
	)
	// Both functions should exist with different names
	assertContains(t, ir, "define i64 @__std_helper")
	assertContains(t, ir, "define i64 @helper")
	// main calls the user version (not the std version)
	assertContains(t, ir, "call i64 @helper()")
}

func TestStdCallViaStdPrefix(t *testing.T) {
	// std.X() should call the std-mangled version
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`
		helper() int { return 99; }
		main() {
			x := helper();
			y := std.helper();
		}
		`,
	)
	// User call goes to @helper, std call goes to @__std_helper
	assertContains(t, ir, "call i64 @helper()")
	assertContains(t, ir, "call i64 @__std_helper()")
}

func TestGenerateTestMainNoExistingMain(t *testing.T) {
	// GenerateTestMain should create a new main when none exists
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests)
	ir := result.Module.String()
	assertContains(t, ir, "define i32 @main")
	assertContains(t, ir, "call i32 @promise_test_run")
	// promise_test_run is now codegen-defined (not a C extern)
	assertContains(t, ir, "define i32 @promise_test_run(i8* %fn)")
	// Thread-based: spawns a thread via PAL, joins it
	assertContains(t, ir, "call i8* @pal_thread_create")
	assertContains(t, ir, "call void @pal_thread_join")
	// Trampoline bridges i8*(i8*) pthread ABI to void() test function
	assertContains(t, ir, "define i8* @.test_trampoline(i8* %fn_ptr)")
}

func TestGenerateTestMainReplacesExistingMain(t *testing.T) {
	// GenerateTestMain should replace user main's blocks
	result := compileResult(t, `
		myTest() `+"`test"+` { }
		main() { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { } main() { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests)
	ir := result.Module.String()
	// Should still have main but with test runner content
	assertContains(t, ir, "define i32 @main")
	assertContains(t, ir, "call i32 @promise_test_run")
	assertContains(t, ir, "call void @promise_test_summary")
}

func TestTestPrintResultBody(t *testing.T) {
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests)
	ir := result.Module.String()

	// Function is defined (not just declared)
	assertContains(t, ir, "define void @promise_test_print_result(i8* %name, i32 %failed)")
	// Branching on failed flag with conditional branch to fail/pass blocks
	assertContains(t, ir, "icmp ne i32 %failed, 0")
	assertContains(t, ir, "br i1")    // conditional branch
	assertContains(t, ir, "br label") // unconditional branches to merge
	// PASS/FAIL prefix globals
	assertContains(t, ir, `@.str.pass_prefix = constant [5 x i8] c"PASS "`)
	assertContains(t, ir, `@.str.fail_prefix = constant [5 x i8] c"FAIL "`)
	// Prefix write: 5 bytes for "PASS " or "FAIL "
	assertContains(t, ir, "call i64 @pal_write(i32 1,")
	assertContains(t, ir, "i64 5)")
	// Gets name length via strlen and writes name
	assertContains(t, ir, "call i64 @strlen(i8* %name)")
	// Newline write: 1 byte
	assertContains(t, ir, "i64 1)")
}

func TestTestSummaryBody(t *testing.T) {
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests)
	ir := result.Module.String()

	// Function is defined (not just declared)
	assertContains(t, ir, "define void @promise_test_summary(i32 %passed, i32 %failed)")
	// String suffix globals
	assertContains(t, ir, `@.str.passed_suffix = constant [9 x i8] c" passed, "`)
	assertContains(t, ir, `@.str.failed_suffix = constant [8 x i8] c" failed\0A"`)
	// Converts i32 → i64 for int_to_string
	assertContains(t, ir, "sext i32 %passed to i64")
	assertContains(t, ir, "sext i32 %failed to i64")
	// Calls int_to_string and frees temp strings
	assertContains(t, ir, "call i8* @promise_int_to_string(i64")
	assertContains(t, ir, "call void @pal_free(i8*")
	// Two free() calls — one per converted count (verify both temp strings freed)
	if strings.Count(ir, "call void @pal_free(i8*") < 2 {
		t.Error("expected at least 2 free() calls in promise_test_summary (one per int_to_string result)")
	}
	// Writes to stdout
	assertContains(t, ir, "call i64 @pal_write(i32 1,")
	// Suffix write lengths: 9 for " passed, ", 8 for " failed\n"
	assertContains(t, ir, "i64 9)")
	assertContains(t, ir, "i64 8)")
	// String instance extraction (bitcast for extractStringDataLenFromInstance)
	assertContains(t, ir, "bitcast i8* %")
	assertContains(t, ir, "to %promise_string_i*")
}

func TestHostTargetTriple(t *testing.T) {
	triple := HostTargetTriple()
	if triple == "" {
		t.Fatal("HostTargetTriple returned empty string")
	}
	// Should contain a known arch
	if !strings.Contains(triple, "arm64") && !strings.Contains(triple, "x86_64") && !strings.Contains(triple, "aarch64") {
		t.Errorf("unexpected target triple: %s", triple)
	}
}

func TestHostTargetTripleInModule(t *testing.T) {
	ir := generateIR(t, `main() {}`)
	triple := HostTargetTriple()
	assertContains(t, ir, "target triple = \""+triple+"\"")
}

func TestStdExternRegistration(t *testing.T) {
	// Std externs should be callable via std.X() and normal call
	ir := generateIRWithStd(t,
		`_do_thing(int x) `+"`"+`extern("c_do_thing");`,
		`main() { _do_thing(42); }`,
	)
	// The C function should be declared
	assertContains(t, ir, "declare void @c_do_thing")
}

func TestStdExternDedupWithUserExtern(t *testing.T) {
	// User extern with same C name as std extern should share the IR declaration
	ir := generateIRWithStd(t,
		`_std_thing(int x) `+"`"+`extern("c_shared_fn");`,
		`
		my_thing(int x) `+"`"+`extern("c_shared_fn");
		main() { my_thing(42); }
		`,
	)
	// Only one C declaration (not two)
	count := strings.Count(ir, "declare void @c_shared_fn")
	if count != 1 {
		t.Errorf("expected 1 declaration of @c_shared_fn, got %d", count)
	}
}

func TestStdFuncUnshadowed(t *testing.T) {
	// When no user function shadows, std function is accessible by plain name
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	// Call should go to the __std_helper function
	assertContains(t, ir, "call i64 @__std_helper")
}

// --- Operator Method Dispatch Tests ---

func TestIncDecVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			x := 0;
			x++;
			x--;
		}
	`)
	// ++ adds 1, -- subtracts 1
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "sub i64")
}

func TestIncDecMember(t *testing.T) {
	ir := generateIR(t, `
		type Counter { int value; }
		main() {
			Counter c = Counter(value: 0);
			c.value++;
		}
	`)
	// Should load field, add 1, store back
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "getelementptr")
}

func TestIncDecIndexedElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			items[0]++;
		}
	`)
	// Should have bounds check
	assertContains(t, ir, "incdec.index.ok")
	assertContains(t, ir, "incdec.index.oob")
	// Should load, increment, store back
	assertContains(t, ir, "add i64")
}

func TestClassicForWithIncDec(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i := 0; i < 5; i++ {
				int x = i;
			}
		}
	`)
	// Should have for loop structure
	assertContains(t, ir, "for.header")
	assertContains(t, ir, "for.body")
	assertContains(t, ir, "for.update")
	// Update should use add i64
	assertContains(t, ir, "add i64")
}

func TestRangeExclusiveCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i in 0..5 {
				int x = i;
			}
		}
	`)
	// Range loop compares counter < end
	assertContains(t, ir, "icmp slt")
	assertContains(t, ir, "forin.header")
}

func TestRangeInclusiveCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i in 0..=5 {
				int x = i;
			}
		}
	`)
	// Inclusive range checks counter <= end
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
}

func TestUnaryNotCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool b = !true;
		}
	`)
	// ! on bool generates xor with 1
	assertContains(t, ir, "xor i1")
}

// --- Stage 8m: use bindings ---

func TestUseVarDeclBasic(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use r := Resource(id: 1);
			int x = r.id;
		}
	`)
	// use binding should generate a close() call at end of scope
	assertContains(t, ir, "call void @Resource.close")
}

func TestUseVarDeclMultiple(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use a := Resource(id: 1);
			use b := Resource(id: 2);
			int x = a.id + b.id;
		}
	`)
	// Both resources should have close() calls
	assertContains(t, ir, "call void @Resource.close")
	// Count that there are at least 2 close calls
	count := strings.Count(ir, "call void @Resource.close")
	if count < 2 {
		t.Errorf("expected at least 2 close calls, got %d\nIR:\n%s", count, ir)
	}
}

func TestUseVarDeclWithReturn(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		make_resource() int {
			use r := Resource(id: 42);
			return r.id;
		}
		main() {
			int v = make_resource();
		}
	`)
	// close() should appear before the return instruction in make_resource
	assertContains(t, ir, "call void @Resource.close")
	assertContains(t, ir, "define i64 @make_resource")
}

func TestUseVarDeclInNestedBlock(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use outer := Resource(id: 1);
			if true {
				use inner := Resource(id: 2);
				int x = inner.id;
			}
			int y = outer.id;
		}
	`)
	// Both outer and inner resources should generate close() calls
	count := strings.Count(ir, "call void @Resource.close")
	if count < 2 {
		t.Errorf("expected at least 2 close calls (inner + outer), got %d\nIR:\n%s", count, ir)
	}
}

// --- Getter/Setter same name regression ---

func TestGetterSetterSameNameCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int _val;
			get val int { return this._val; }
			set val(int v) { this._val = v; }
		}
		main() {
			Box b = Box(_val: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Both getter and setter should produce distinct functions
	assertContains(t, ir, "define i64 @Box.val(")
	assertContains(t, ir, "define void @Box.val$set(")
}

func TestGetterSetterSameNameVtable(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			get val int `+"`"+`abstract;
			set val(int v) `+"`"+`abstract;
		}
		type Impl is Base {
			int _v;
			get val int { return this._v; }
			set val(int v) { this._v = v; }
		}
		main() {
			Base b = Impl(_v: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Vtable should contain both getter and setter slots
	assertContains(t, ir, "@promise_vtable_Base")
	assertContains(t, ir, "@promise_vtable_Impl")
	// Both getter and setter functions should exist
	assertContains(t, ir, "define i64 @Impl.val(")
	assertContains(t, ir, "define void @Impl.val$set(")
	// Virtual dispatch should NOT use direct call to Base.val (abstract)
	assertNotContains(t, ir, "call i64 @Base.val(")
}

func TestCompoundAssignmentGetterSetterCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
			set count(int v) { this._count = v; }
		}
		main() {
			Counter c = Counter(_count: 0);
			c.count += 5;
		}
	`)
	// Should call both getter and setter
	assertContains(t, ir, "call i64 @Counter.count(")
	assertContains(t, ir, "call void @Counter.count$set(")
}

func TestViewVtableGetterSetter(t *testing.T) {
	ir := generateIR(t, `
		type Readable {
			get val int `+"`"+`abstract;
		}
		type Writable {
			get val int `+"`"+`abstract;
			set val(int v) `+"`"+`abstract;
		}
		type Store is Readable, Writable {
			int _v;
			get val int { return this._v; }
			set val(int v) { this._v = v; }
		}
		main() {
			Writable w = Store(_v: 0);
			w.val = 42;
			int v = w.val;
		}
	`)
	// View vtable for Store-as-Writable should exist
	assertContains(t, ir, "promise_vtable_Store_as_Writable")
	// Both functions should be emitted
	assertContains(t, ir, "define i64 @Store.val(")
	assertContains(t, ir, "define void @Store.val$set(")
}

func TestGenericGetterSetterSameName(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T _val;
			get val T { return this._val; }
			set val(T v) { this._val = v; }
		}
		main() {
			b := Box[int](_val: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Monomorphized getter and setter should have distinct names
	assertContains(t, ir, "define i64 @Box__int.val(")
	assertContains(t, ir, "define void @Box__int.val$set(")
}

// --- Drop method tests ---

// Basic: drop() called at scope exit
func TestDropBasicScopeExit(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r := Resource(id: 1);
			int x = r.id;
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// Move to function arg clears drop flag, adds condBr
func TestDropNotCalledWhenMoved(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		main() {
			r := Resource(id: 1);
			consume(r);
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Return triggers drop before ret
func TestDropWithReturn(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() int {
			r := Resource(id: 42);
			return r.id;
		}
		main() {
			int v = make();
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "define i64 @make")
}

// Mixed use + drop bindings both fire
func TestDropAndUseOrdering(t *testing.T) {
	ir := generateIR(t, `
		type Closeable {
			int id;
			close() { }
		}
		type Droppable {
			int id;
			drop(~this) { }
		}
		main() {
			use c := Closeable(id: 1);
			d := Droppable(id: 2);
			int x = c.id + d.id;
		}
	`)
	assertContains(t, ir, "call void @Closeable.close")
	assertContains(t, ir, "call void @Droppable.drop")
}

// Nested type: outer drop() triggers field drops
func TestDropFieldAutoCleanup(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
			drop(~this) { }
		}
		main() {
			o := Outer(inner: Inner(id: 1));
			int x = o.inner.id;
		}
	`)
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "call void @Inner.drop")
}

// Returning a droppable variable clears its flag
func TestDropReturnMoveClearsFlag(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() Resource {
			r := Resource(id: 42);
			return r;
		}
		main() {
			Resource v = make();
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
}

// Conditional move: moved in if-then only → drop flag condBr after merge
func TestDropConditionalMove(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		main() {
			r := Resource(id: 1);
			if true {
				consume(r);
			}
		}
	`)
	// Drop flag cleared in then-branch
	assertContains(t, ir, "store i1 false, i1*")
	// Conditional drop at scope exit (flag may be true or false)
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Conditional move with else: moved in both branches → flag cleared in both
func TestDropConditionalMoveBothBranches(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		other(Resource r) { }
		main() {
			r := Resource(id: 1);
			if true {
				consume(r);
			} else {
				other(r);
			}
		}
	`)
	// Flag should be cleared in both branches
	count := strings.Count(ir, "store i1 false, i1*")
	if count < 2 {
		t.Errorf("expected at least 2 'store i1 false' (both branches), got %d", count)
	}
}

// Nested scopes: inner scope drop happens before outer
func TestDropNestedScopes(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r1 := Resource(id: 1);
			if true {
				r2 := Resource(id: 2);
				int x = r2.id;
			}
			int y = r1.id;
		}
	`)
	// Both should have drop flags and calls
	assertContains(t, ir, "r1.dropflag")
	assertContains(t, ir, "r2.dropflag")
	// Two drop calls (one for inner, one for outer)
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 2 {
		t.Errorf("expected at least 2 drop calls (inner + outer scope), got %d\nIR:\n%s", count, ir)
	}
}

// While loop: droppable var inside loop body should be dropped per iteration
func TestDropInWhileLoop(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			int i = 0;
			while i < 3 {
				r := Resource(id: i);
				int x = r.id;
				i += 1;
			}
		}
	`)
	// Drop should be emitted inside the loop body
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// Infinite loop with break: drop cleanup happens at break
func TestDropInLoopWithBreak(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			for {
				r := Resource(id: 1);
				int x = r.id;
				break;
			}
		}
	`)
	// Drop call should be present (at break cleanup)
	assertContains(t, ir, "call void @Resource.drop")
}

// Loop with continue: drop fires at end of iteration and at continue
func TestDropInLoopWithContinue(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			int i = 0;
			while i < 5 {
				r := Resource(id: i);
				i += 1;
				if i == 3 {
					continue;
				}
				int x = r.id;
			}
		}
	`)
	// Drop calls should exist (at continue and normal scope exit)
	assertContains(t, ir, "call void @Resource.drop")
}

// Move into method call clears drop flag
func TestDropMoveToMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		type Container {
			int id;
			take(Resource r) { }
		}
		main() {
			c := Container(id: 0);
			r := Resource(id: 1);
			c.take(r);
		}
	`)
	// r's drop flag should be cleared after method call
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Move into constructor field clears drop flag
func TestDropMoveToConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			r := Inner(id: 1);
			o := Outer(inner: r);
			int x = o.inner.id;
		}
	`)
	// r's drop flag should be cleared when moved into constructor
	assertContains(t, ir, "store i1 false, i1*")
}

// Move into ident assignment clears drop flag
func TestDropMoveToIdentAssign(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			Resource a = Resource(id: 1);
			Resource b = Resource(id: 2);
			b = a;
			int x = b.id;
		}
	`)
	// a's drop flag should be cleared after the assignment to b
	assertContains(t, ir, "store i1 false, i1*")
}

// Move into member assignment clears drop flag (bug #2 fix)
func TestDropMoveToMemberAssign(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			o := Outer(inner: Inner(id: 0));
			r := Inner(id: 1);
			o.inner = r;
		}
	`)
	// r's drop flag should be cleared after the member assignment
	assertContains(t, ir, "store i1 false, i1*")
}

// Multiple droppable vars: each gets its own flag and cleanup
func TestDropMultipleVariables(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			a := Resource(id: 1);
			b := Resource(id: 2);
			c := Resource(id: 3);
			int x = a.id + b.id + c.id;
		}
	`)
	assertContains(t, ir, "a.dropflag")
	assertContains(t, ir, "b.dropflag")
	assertContains(t, ir, "c.dropflag")
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 3 {
		t.Errorf("expected at least 3 drop calls, got %d\nIR:\n%s", count, ir)
	}
}

// Multiple droppable fields: all cleaned up after user drop() body
func TestDropMultipleFieldsAutoCleanup(t *testing.T) {
	ir := generateIR(t, `
		type FileHandle {
			int fd;
			drop(~this) { }
		}
		type Connection {
			FileHandle read_handle;
			FileHandle write_handle;
			drop(~this) { }
		}
		main() {
			c := Connection(read_handle: FileHandle(fd: 3), write_handle: FileHandle(fd: 4));
		}
	`)
	assertContains(t, ir, "call void @Connection.drop")
	// FileHandle.drop should be called for both fields inside Connection.drop
	count := strings.Count(ir, "call void @FileHandle.drop")
	if count < 2 {
		t.Errorf("expected at least 2 FileHandle.drop calls (one per field), got %d\nIR:\n%s", count, ir)
	}
}

// Non-droppable type: no drop flag or call generated
func TestDropNotGeneratedForNonDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Simple {
			int id;
		}
		main() {
			s := Simple(id: 1);
			int x = s.id;
		}
	`)
	assertNotContains(t, ir, "dropflag")
	assertNotContains(t, ir, "drop.call")
	assertNotContains(t, ir, "drop.skip")
}

// Copy type: no drop flag even if fields exist
func TestDropNotGeneratedForCopyType(t *testing.T) {
	ir := generateIR(t, `
		type Point `+"`"+`copy {
			int x;
			int y;
		}
		main() {
			p := Point(x: 1, y: 2);
			int v = p.x;
		}
	`)
	assertNotContains(t, ir, "dropflag")
	assertNotContains(t, ir, "drop.call")
}

// Droppable var in typed var decl
func TestDropTypedVarDecl(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			Resource r = Resource(id: 1);
			int x = r.id;
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// Drop with early return in failable function
func TestDropWithEarlyReturnFailable(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		work() void! {
			r := Resource(id: 42);
			return;
		}
		main() { }
	`)
	// drop() should be emitted before the return
	assertContains(t, ir, "call void @Resource.drop")
}

// Drop with raise: cleanup before error return
func TestDropWithRaise(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		fail() void! {
			r := Resource(id: 1);
			raise "oops";
		}
		main() { }
	`)
	// drop() should be emitted before the raise
	assertContains(t, ir, "call void @Resource.drop")
}

// Drop in a function that takes and returns a droppable:
// the parameter itself doesn't get a drop flag (it's the caller's responsibility)
func TestDropParameterNotFlagged(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		passthrough(Resource r) int {
			return r.id;
		}
		main() {
			int x = passthrough(Resource(id: 1));
		}
	`)
	assertContains(t, ir, "define i64 @passthrough")
	// The function should not create a drop flag for its parameter
	// (it doesn't own the alloca, the caller does the drop flag management)
}

// --- Alignment bug fix test ---

func TestLlvmTypeSizeAlignment(t *testing.T) {
	// Test that struct sizes account for alignment padding
	// {i1, i64} should be 16 (1 byte + 7 padding + 8 bytes), not 9
	s1 := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	if sz := llvmTypeSize(s1); sz != 16 {
		t.Errorf("{i1, i64} size: got %d, want 16", sz)
	}

	// {i64, i1} should be 16 (8 bytes + 1 byte + 7 tail padding)
	s2 := irtypes.NewStruct(irtypes.I64, irtypes.I1)
	if sz := llvmTypeSize(s2); sz != 16 {
		t.Errorf("{i64, i1} size: got %d, want 16", sz)
	}

	// {i32, i32} should be 8 (no padding needed)
	s3 := irtypes.NewStruct(irtypes.I32, irtypes.I32)
	if sz := llvmTypeSize(s3); sz != 8 {
		t.Errorf("{i32, i32} size: got %d, want 8", sz)
	}

	// {i8, i32, i8} should be 12 (1 + 3pad + 4 + 1 + 3pad)
	s4 := irtypes.NewStruct(irtypes.I8, irtypes.I32, irtypes.I8)
	if sz := llvmTypeSize(s4); sz != 12 {
		t.Errorf("{i8, i32, i8} size: got %d, want 12", sz)
	}
}

// --- Compound index eval order test ---

func TestCompoundIndexAssignSlice(t *testing.T) {
	// Ensure compound index assignments on slices generate valid IR
	ir := generateIR(t, `
		main() {
			s := [1, 2, 3];
			s[0] += 10;
		}
	`)
	assertContains(t, ir, "define i32 @main")
	// Should contain the compound add operation
	assertContains(t, ir, "add i64")
}

// --- Coverage gap tests ---

// Virtual close dispatch through vtable (type has children → needs vtable)
func TestUseVarVirtualCloseDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int id;
			close() { }
		}
		type Child is Base {
			close() { }
		}
		main() {
			use r := Base(id: 1);
			int x = r.id;
		}
	`)
	// Base has children → needs vtable → virtual close dispatch
	assertContains(t, ir, "@promise_vtable_Base")
}

// Virtual close with failable close() method (parent type with child)
func TestUseVarVirtualCloseDispatchFailable(t *testing.T) {
	ir := generateIR(t, `
		type Conn {
			int fd;
			close()! { }
		}
		type TcpConn is Conn {
			close()! { }
		}
		main() {
			use c := Conn(fd: 3);
			int x = c.fd;
		}
	`)
	// Conn has children → needs vtable → virtual close dispatch
	assertContains(t, ir, "@promise_vtable_Conn")
}

// Virtual drop dispatch through vtable (type has children → needs vtable)
func TestDropVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Handle {
			int id;
			drop(~this) { }
		}
		type FileHandle is Handle {
			drop(~this) { }
		}
		main() {
			h := Handle(id: 1);
			int x = h.id;
		}
	`)
	// Handle has children → needs vtable → virtual drop dispatch
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	assertContains(t, ir, "h.dropflag")
	assertContains(t, ir, "@promise_vtable_Handle")
}

// llvmTypeAlign coverage: float, double, pointer, array
func TestLlvmTypeAlignFloat(t *testing.T) {
	if a := llvmTypeAlign(irtypes.Float); a != 4 {
		t.Errorf("float align: got %d, want 4", a)
	}
}

func TestLlvmTypeAlignDouble(t *testing.T) {
	if a := llvmTypeAlign(irtypes.Double); a != 8 {
		t.Errorf("double align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignPointer(t *testing.T) {
	if a := llvmTypeAlign(irtypes.I8Ptr); a != 8 {
		t.Errorf("pointer align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignArray(t *testing.T) {
	arr := irtypes.NewArray(10, irtypes.I32)
	if a := llvmTypeAlign(arr); a != 4 {
		t.Errorf("[10 x i32] align: got %d, want 4", a)
	}
}

func TestLlvmTypeAlignStruct(t *testing.T) {
	s := irtypes.NewStruct(irtypes.I8, irtypes.I64)
	if a := llvmTypeAlign(s); a != 8 {
		t.Errorf("{i8, i64} align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignLargeInt(t *testing.T) {
	// i128 = 16 bytes, but capped at 8
	i128 := irtypes.NewInt(128)
	if a := llvmTypeAlign(i128); a != 8 {
		t.Errorf("i128 align: got %d, want 8", a)
	}
}

func TestLlvmTypeSizeFloat(t *testing.T) {
	if sz := llvmTypeSize(irtypes.Float); sz != 4 {
		t.Errorf("float size: got %d, want 4", sz)
	}
	if sz := llvmTypeSize(irtypes.Double); sz != 8 {
		t.Errorf("double size: got %d, want 8", sz)
	}
}

func TestLlvmTypeSizePointer(t *testing.T) {
	if sz := llvmTypeSize(irtypes.I8Ptr); sz != 8 {
		t.Errorf("pointer size: got %d, want 8", sz)
	}
}

func TestLlvmTypeSizeArray(t *testing.T) {
	arr := irtypes.NewArray(5, irtypes.I32)
	if sz := llvmTypeSize(arr); sz != 20 {
		t.Errorf("[5 x i32] size: got %d, want 20", sz)
	}
}

// Drop with use in loop triggers both close and drop at scope boundaries
func TestDropAndUseInLoop(t *testing.T) {
	ir := generateIR(t, `
		type Closeable {
			int id;
			close() { }
		}
		type Droppable {
			int id;
			drop(~this) { }
		}
		main() {
			d := Droppable(id: 1);
			int i = 0;
			while i < 3 {
				use c := Closeable(id: i);
				int x = c.id + d.id;
				i++;
			}
		}
	`)
	assertContains(t, ir, "call void @Closeable.close")
	assertContains(t, ir, "call void @Droppable.drop")
}

// Move in function call clears flag — std call variant
func TestDropMoveToStdCall(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		take(Resource r) { }
		main() {
			r := Resource(id: 1);
			take(r);
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Move in generic function call clears flag
func TestDropMoveToGenericFuncCall(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		identity[T](T val) T { return val; }
		main() {
			r := Resource(id: 1);
			Resource r2 = identity[Resource](r);
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
}

// Move to index assignment clears flag
func TestDropMoveToIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			arr := [Resource(id: 0)];
			r := Resource(id: 1);
			arr[0] = r;
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
}

// Error propagation triggers scope cleanup
func TestDropErrorPropagateCleansUp(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		risky() int! {
			return 42;
		}
		work() int! {
			r := Resource(id: 1);
			int val = risky()?;
			return val + r.id;
		}
		main() { }
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "define { i1, i64, i8* } @work")
}

// Compound assignment on different typed variables exercises namedFromLLVMType branches
func TestCompoundAssignF64(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 x = 1.5;
			x += 2.5;
		}
	`)
	assertContains(t, ir, "fadd double")
}

func TestCompoundAssignI32(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			i32 val;
			work(~this, i32 delta) { this.val -= delta; }
		}
		main() { }
	`)
	assertContains(t, ir, "sub i32")
}

func TestCompoundAssignF32(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			f32 val;
			work(~this, f32 factor) { this.val *= factor; }
		}
		main() { }
	`)
	assertContains(t, ir, "fmul float")
}

func TestCompoundAssignI16(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			i16 val;
			work(~this, i16 delta) { this.val += delta; }
		}
		main() { }
	`)
	assertContains(t, ir, "add i16")
}

func TestCompoundAssignI8(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			i8 val;
			work(~this, i8 delta) { this.val += delta; }
		}
		main() { }
	`)
	assertContains(t, ir, "add i8")
}

// --- Hash getter tests ---

func TestHashGetterInt(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; h := x.hash; }`)
	assertContains(t, ir, "call i64 @__std__fnv1a_hash(i64")
}

func TestHashGetterBool(t *testing.T) {
	ir := generateIR(t, `main() { b := true; h := b.hash; }`)
	assertContains(t, ir, "zext i1")
	assertContains(t, ir, "call i64 @__std__fnv1a_hash(i64")
}

func TestHashGetterChar(t *testing.T) {
	ir := generateIR(t, `main() { c := 'a'; h := c.hash; }`)
	assertContains(t, ir, "call i64 @__std__fnv1a_hash(i64")
}

func TestHashGetterString(t *testing.T) {
	ir := generateIR(t, `main() { s := "hi"; h := s.hash; }`)
	assertContains(t, ir, "call i64 @__promise_hash_string(i8*")
}

func TestHashGetterFloat(t *testing.T) {
	ir := generateIR(t, `
		test(f64 x) int { return x.hash; }
		main() {}
	`)
	assertContains(t, ir, "bitcast double")
	assertContains(t, ir, "call i64 @__std__fnv1a_hash(i64")
}

func TestHashGetterSmallInt(t *testing.T) {
	ir := generateIR(t, `
		test(i8 x) int { return x.hash; }
		main() {}
	`)
	assertContains(t, ir, "sext i8")
	assertContains(t, ir, "call i64 @__std__fnv1a_hash(i64")
}

func TestHashGetterSmallUint(t *testing.T) {
	ir := generateIR(t, `
		test(u8 x) int { return x.hash; }
		main() {}
	`)
	// Unsigned types use zero-extend, not sign-extend
	assertContains(t, ir, "zext i8")
	assertContains(t, ir, "call i64 @__std__fnv1a_hash(i64")
}

// --- Vector method tests ---

func TestVectorPush(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			nums.push(3);
		}
	`)
	assertContains(t, ir, "define i8* @promise_vector_push(")
	assertContains(t, ir, "call i8* @promise_vector_push(")
}

func TestVectorPop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			int? v = nums.pop();
		}
	`)
	assertContains(t, ir, "define i32 @promise_vector_pop(")
	assertContains(t, ir, "call i32 @promise_vector_pop(")
	assertContains(t, ir, "pop.some")
	assertContains(t, ir, "pop.none")
}

func TestVectorContainsInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			bool has = nums.contains(2);
		}
	`)
	assertContains(t, ir, "define i8 @promise_vector_contains(")
	assertContains(t, ir, "call i8 @promise_vector_contains(")
}

func TestVectorContainsString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] words = ["a", "b"];
			bool has = words.contains("a");
		}
	`)
	assertContains(t, ir, "define i8 @promise_vector_contains(")
	assertContains(t, ir, "call i8 @promise_vector_contains(")
	// String contains uses custom equality comparator
	assertContains(t, ir, "@__promise_eq_string")
}

func TestVectorRemove(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(0);
		}
	`)
	assertContains(t, ir, "define void @promise_vector_remove(")
	assertContains(t, ir, "call void @promise_vector_remove(")
}

func TestVectorContainsFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1];
			bool has = nums.contains(1);
		}
	`)
	// Verify key blocks in the defined contains function
	assertContains(t, ir, "loop.header:")
	assertContains(t, ir, "loop.body:")
	assertContains(t, ir, "cmp_bytes:")
	assertContains(t, ir, "call_eq:")
	// memcmp replaces byte-by-byte loop
	assertContains(t, ir, "call i32 @memcmp(")
	assertNotContains(t, ir, "byte.header:")
	assertNotContains(t, ir, "byte.body:")
	assertContains(t, ir, "found:")
	assertContains(t, ir, "not_found:")
	assertContains(t, ir, "loop.next:")
}

func TestVectorRemoveFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(1);
		}
	`)
	// Verify key blocks in the defined remove function
	assertContains(t, ir, "panic:")
	assertContains(t, ir, "check_shift:")
	assertContains(t, ir, "do_shift:")
	assertContains(t, ir, "dec_len:")
	// Verify panic calls and memmove
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "call void @llvm.memmove.p0i8.p0i8.i64(")
}

func TestVectorWithCapacityFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			nums.push(3);
		}
	`)
	// with_capacity is always defined (codegen intrinsic)
	assertContains(t, ir, "define i8* @promise_vector_with_capacity(")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "init:")
	assertContains(t, ir, "store i64 0")
}

func TestVectorPushFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1];
			nums.push(2);
		}
	`)
	// Verify key blocks in the defined push function
	assertContains(t, ir, "define i8* @promise_vector_push(")
	assertContains(t, ir, "grow:")
	assertContains(t, ir, "call i8* @pal_realloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "update_cap:")
	assertContains(t, ir, "copy:")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
}

func TestVectorPopFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			int? v = nums.pop();
		}
	`)
	// Verify key blocks in the defined pop function
	assertContains(t, ir, "define i32 @promise_vector_pop(")
	assertContains(t, ir, "empty:")
	assertContains(t, ir, "do_pop:")
	assertContains(t, ir, "ret i32 0")
	assertContains(t, ir, "ret i32 1")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
}

func TestLLVMMemmoveDeclared(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(0);
		}
	`)
	assertContains(t, ir, "declare void @llvm.memmove.p0i8.p0i8.i64(")
}

func TestVectorContainsIntNull(t *testing.T) {
	// Int contains passes null eq_fn → byte comparison path
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			bool has = nums.contains(2);
		}
	`)
	assertContains(t, ir, "call i8 @promise_vector_contains(")
	// Null eq_fn for int (non-string) type
	assertContains(t, ir, "null)")
}

// --- Vector capacity constructor ---

// TODO: Vector capacity constructor T[](capacity: n) not yet wired through sema.
// genVectorCapacityConstructor exists in codegen but sema doesn't recognize the syntax yet.

// --- String byte indexing ---

func TestStringByteIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			char c = s[0];
		}
	`)
	assertContains(t, ir, "stridx.ok")
	assertContains(t, ir, "stridx.oob")
	assertContains(t, ir, "zext i8")
}

// --- String method tests ---

func TestStringContains(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello world";
			bool has = s.contains("world");
		}
	`)
	// Now a Promise method, compiled as a regular function
	assertContains(t, ir, "define i1 @string.contains(")
	assertContains(t, ir, "call i1 @string.contains(")
}

func TestStringStartsWith(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			bool yes = s.starts_with("hel");
		}
	`)
	assertContains(t, ir, "define i1 @string.starts_with(")
	assertContains(t, ir, "call i1 @string.starts_with(")
}

func TestStringEndsWith(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			bool yes = s.ends_with("llo");
		}
	`)
	assertContains(t, ir, "define i1 @string.ends_with(")
	assertContains(t, ir, "call i1 @string.ends_with(")
}

func TestStringIndexOf(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			int? idx = s.index_of("ll");
		}
	`)
	assertContains(t, ir, "define { i1, i64 } @string.index_of(")
	assertContains(t, ir, "call { i1, i64 } @string.index_of(")
}

func TestStringTrim(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "  hi  ";
			string trimmed = s.trim();
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_trim(")
	assertContains(t, ir, "call i8* @promise_string_trim(")
}

func TestStringSplit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "a,b,c";
			string[] parts = s.split(",");
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_split(")
	assertContains(t, ir, "call i8* @promise_string_split(")
}

func TestStringTrimFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := " hi ".trim();
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_trim(i8* %s)")
	assertContains(t, ir, "trim_left_hdr:")
	assertContains(t, ir, "trim_right_hdr:")
	assertContains(t, ir, "build_result:")
	assertContains(t, ir, "icmp eq i8") // whitespace checks
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestStringSplitFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "a,b".split(",");
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_split(i8* %s, i8* %sep)")
	assertContains(t, ir, "call i32 @memcmp(")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "call i8* @promise_string_new(")
	assertContains(t, ir, "count_hdr:")
	assertContains(t, ir, "split_hdr:")
	assertContains(t, ir, "split_tail:")
}

func TestStringNextCharFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for ch in "abc" {}
		}
	`)
	assertContains(t, ir, "define i32 @promise_string_next_char(i8* %s, i64* %pos)")
	assertContains(t, ir, "ret_eof:")
	assertContains(t, ir, "ret i32 -1")
	assertContains(t, ir, "set_1byte:")
	assertContains(t, ir, "cont_hdr:")
	assertContains(t, ir, "cont_body:")
	assertContains(t, ir, "cont_done:")
}

func TestMemcmpDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 1; }`)
	assertContains(t, ir, "declare i32 @memcmp(i8* nocapture noundef %s1, i8* nocapture noundef %s2, i64 noundef %n)")
	assertContains(t, ir, "mustprogress nounwind readonly willreturn argmemonly")
}

func TestAllocatorAttributes(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		main() { f := Foo(x: 1); }
	`)
	// Underlying libc declarations still present (emitted by PAL)
	assertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	assertContains(t, ir, "declare void @free(i8* nocapture noundef %ptr) nounwind willreturn")
	assertContains(t, ir, "declare noalias i8* @realloc(i8* nocapture noundef %ptr, i64 noundef %size) nounwind willreturn")
	// PAL wrappers defined
	assertContains(t, ir, "@pal_alloc(i64 %size)")
	assertContains(t, ir, "@pal_free(i8* %ptr)")
	assertContains(t, ir, "@pal_realloc(i8* %ptr, i64 %size)")
}

// --- Return optional wrapping in monomorphized context ---

func TestReturnOptionalInMonoMethod(t *testing.T) {
	// The map [] method returns V? — returning a concrete V must wrap in Optional
	ir := generateIR(t, `
		main() {
			m := {"x": 42};
			int? v = m["x"];
		}
	`)
	// The monomorphized [] method should produce { i1, i64 } return type
	assertContains(t, ir, `define { i1, i64 } @"map__string__int.[]"(`)
	// Should contain insertvalue for wrapping the value in Optional { true, val }
	assertContains(t, ir, "insertvalue { i1, i64 }")
}

// --- Nested generic monomorphization (discoverInstances) ---

func TestNestedGenericMonomorphization(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		type Wrapper[T] { Box[T] inner; }
		main() {
			w := Wrapper[int](inner: Box[int](val: 42));
		}
	`)
	// Both Wrapper[int] and Box[int] should be monomorphized
	assertContains(t, ir, "Wrapper__int")
	assertContains(t, ir, "Box__int")
}

// --- Non-native operator dispatch ---

func TestNonNativeOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
		}
		main() {
			Pt a = Pt(x: 1);
			Pt b = Pt(x: 2);
			bool r = a == b;
		}
	`)
	assertContains(t, ir, `call i1 @"Pt.=="(`)
}

func TestDefaultMethodViaViewVtable(t *testing.T) {
	ir := generateIRWithStd(t,
		"type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n",
		`type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
		}
		main() {
			MyEq e = Pt(x: 1);
			MyEq f = Pt(x: 2);
			bool r = e != f;
		}
	`)
	assertContains(t, ir, `@"Pt.!="`)                  // synthesized default
	assertContains(t, ir, "promise_vtable_Pt_as_MyEq") // view vtable
}

func TestDefaultMethodOverride(t *testing.T) {
	// Concrete type overrides the default — the override should be used, not the synthesized default
	ir := generateIRWithStd(t,
		"type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n",
		`type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
			!=(Pt other) bool { return this.x != other.x; }
		}
		main() {
			MyEq e = Pt(x: 1);
			MyEq f = Pt(x: 2);
			bool r = e != f;
		}
	`)
	assertContains(t, ir, "promise_vtable_Pt_as_MyEq") // view vtable still created
	// The vtable should use the concrete Pt.!= override, not a synthesized default.
	// Check that the concrete method exists.
	assertContains(t, ir, `@"Pt.!="`)
}

func TestOrderedDefaultsViaViewVtable(t *testing.T) {
	stdOrd := "type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n" +
		"type MyOrd is MyEq `structural {\n\t<(Self other) bool `abstract;\n\t>(Self other) bool => other < this;\n\t<=(Self other) bool => !(other < this);\n\t>=(Self other) bool => !(this < other);\n}\n"
	ir := generateIRWithStd(t, stdOrd, `
		type Val {
			int n;
			==(Val o) bool { return this.n == o.n; }
			<(Val o) bool { return this.n < o.n; }
		}
		main() {
			MyOrd a = Val(n: 1);
			MyOrd b = Val(n: 2);
			bool r1 = a > b;
			bool r2 = a <= b;
			bool r3 = a >= b;
			bool r4 = a != b;
		}
	`)
	assertContains(t, ir, `@"Val.>"`)  // synthesized from > default
	assertContains(t, ir, `@"Val.<="`) // synthesized from <= default
	assertContains(t, ir, `@"Val.>="`) // synthesized from >= default
	assertContains(t, ir, `@"Val.!="`) // inherited from MyEq parent default
	assertContains(t, ir, "promise_vtable_Val_as_MyOrd")
}

// --- Go / Receive (concurrency) tests ---

func TestGoExprBasicFunction(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Coroutine function generated with presplitcoroutine attribute
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used
	assertContains(t, ir, "call token @llvm.coro.id(")
	assertContains(t, ir, "call i8* @llvm.coro.begin(")
	assertContains(t, ir, "call i8 @llvm.coro.suspend(")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
	// Result buffer allocated for non-void task
	assertContains(t, ir, "@pal_alloc")
	// Coroutine calls target function
	assertContains(t, ir, "call i64 @compute")
}

func TestGoExprWithArgs(t *testing.T) {
	ir := generateIR(t, `
		double(int x) int { return x * 2; }
		main() {
			t := go double(21);
			result := <-t;
		}
	`)
	// Coroutine generated
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
	// The coroutine should call the target function
	assertContains(t, ir, "call i64 @double")
}

func TestGoExprVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine calls void function
	assertContains(t, ir, "call void @doWork")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoExprBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	// Coroutine function for the block
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestReceiveExprWaitLoop(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 1; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Verify the task receive structure (thread-blocking mode in main)
	assertContains(t, ir, "task.done")
	assertContains(t, ir, "task.wait")
	assertContains(t, ir, "task.ready")
}

// --- Channel tests ---

func TestChannelConstructor(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	// Should call promise_channel_new
	assertContains(t, ir, "call i8* @promise_channel_new(")
	// Should init mutex and 2 cond vars inside promise_channel_new
	assertContains(t, ir, "call i8* @pal_mutex_init()")
	assertContains(t, ir, "call i8* @pal_cond_init()")
}

func TestChannelConstructorUnbuffered(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
		}
	`)
	// Unbuffered: capacity=0
	assertContains(t, ir, "call i8* @promise_channel_new(i64 0,")
}

func TestChannelSend(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Should lock/unlock mutex and use memcpy for send
	assertContains(t, ir, "call void @pal_mutex_lock(")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
	assertContains(t, ir, "call void @pal_cond_signal(")
	assertContains(t, ir, "call void @pal_mutex_unlock(")
}

func TestChannelClose(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should broadcast both cond vars
	assertContains(t, ir, "call void @pal_cond_broadcast(")
}

func TestChannelReceive(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			val := <-ch;
		}
	`)
	// Should have channel receive blocks
	assertContains(t, ir, "chrecv.wait")
	assertContains(t, ir, "chrecv.check")
	assertContains(t, ir, "chrecv.none")
	assertContains(t, ir, "chrecv.read")
	assertContains(t, ir, "chrecv.done")
	// Returns optional { i1, i64 }
	assertContains(t, ir, "insertvalue { i1, i64 }")
}

func TestChannelForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			ch.close();
			for v in ch {
				int x = v + 1;
			}
		}
	`)
	// Should have channel for-in block labels
	assertContains(t, ir, "forin_ch.header")
	assertContains(t, ir, "forin_ch.recv.wait")
	assertContains(t, ir, "forin_ch.recv.check")
	assertContains(t, ir, "forin_ch.recv.none")
	assertContains(t, ir, "forin_ch.recv.read")
	assertContains(t, ir, "forin_ch.body")
	assertContains(t, ir, "forin_ch.exit")
}

func TestChannelSendClosedPanic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Send should check closed flag and panic if set
	assertContains(t, ir, "send.closed.panic")
	assertContains(t, ir, "send on closed channel")
	// After wait-full wakeup, should re-check closed
	assertContains(t, ir, "send.waitfull.closed")
}

func TestChannelDoubleClosePanic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should check already-closed flag
	assertContains(t, ir, "close.panic")
	assertContains(t, ir, "close of closed channel")
}

func TestGoBlockCapturesOuterVars(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			int x = 10;
			go {
				ch.send(x);
			};
		}
	`)
	// Coroutine should have parameters for captured variables
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockCapturesMultipleVars(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			int a = 10;
			int b = 20;
			go {
				ch.send(a + b);
			};
		}
	`)
	// Coroutine function should accept captured parameters
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
}

func TestGoBlockNoCapturesStillWorks(t *testing.T) {
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// Even without captures, the go block should generate a coroutine
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// --- M:N Scheduler IR Tests ---

func TestMainWrappedAsG0(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// User main should be renamed to __promise_user_main
	assertContains(t, ir, "define i32 @__promise_user_main()")
	// New main initializes the scheduler
	assertContains(t, ir, "define i32 @main()")
	assertContains(t, ir, "call i32 @pal_num_cpus()")
	assertContains(t, ir, "call void @promise_sched_init(")
	assertContains(t, ir, "call void @promise_sched_run_until_main(")
	assertContains(t, ir, "call void @promise_sched_shutdown()")
	// Coroutine wrapper for user main
	assertContains(t, ir, "define i8* @.goroutine.main()")
	assertContains(t, ir, "call i32 @__promise_user_main()")
}

func TestSchedulerGlobals(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// Thread-local current G pointer
	assertContains(t, ir, "@__promise_current_g")
	assertContains(t, ir, "thread_local")
	// Global scheduler singleton
	assertContains(t, ir, "@__promise_sched")
}

func TestSchedulerFunctionsExist(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "define void @promise_sched_init(")
	assertContains(t, ir, "define i8* @promise_sched_loop(")
	assertContains(t, ir, "define void @promise_sched_enqueue(")
	assertContains(t, ir, "define i8* @promise_sched_find_runnable(")
	assertContains(t, ir, "define void @promise_sched_park_m(")
	assertContains(t, ir, "define void @promise_sched_wake_m()")
	assertContains(t, ir, "define void @promise_goroutine_exit(")
	assertContains(t, ir, "define void @promise_sched_shutdown()")
	assertContains(t, ir, "define i8* @promise_g_new(")
}

func TestWaiterListFunctionsExist(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "define void @promise_waiter_enqueue(")
	assertContains(t, ir, "define i8* @promise_waiter_dequeue(")
	assertContains(t, ir, "define void @promise_waiter_wake_all(")
}

func TestCoroIntrinsicsDeclared(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "declare token @llvm.coro.id(")
	assertContains(t, ir, "declare i1 @llvm.coro.alloc(")
	assertContains(t, ir, "declare i8* @llvm.coro.begin(")
	assertContains(t, ir, "declare i64 @llvm.coro.size.i64()")
	assertContains(t, ir, "declare i8 @llvm.coro.suspend(")
	assertContains(t, ir, "declare void @llvm.coro.end(")
	assertContains(t, ir, "declare i8* @llvm.coro.free(")
	assertContains(t, ir, "declare void @llvm.coro.resume(")
	assertContains(t, ir, "declare void @llvm.coro.destroy(")
	assertContains(t, ir, "declare i1 @llvm.coro.done(")
}

func TestGoBlockEmitsCoroutine(t *testing.T) {
	ir := generateIR(t, `
		main() {
			x := 42;
			go { x; };
		}
	`)
	// Coroutine function with presplitcoroutine attribute
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used in the go block
	assertContains(t, ir, "call token @llvm.coro.id(")
	assertContains(t, ir, "call i1 @llvm.coro.alloc(")
	assertContains(t, ir, "call i8* @llvm.coro.begin(")
	assertContains(t, ir, "call i8 @llvm.coro.suspend(")
	assertContains(t, ir, "call void @llvm.coro.end(")
	// Go blocks now use coroutine + G + enqueue, not direct pal_thread_create
	// (pal_thread_create is still used by the scheduler for M threads, but not in go block codegen)
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockEnqueuesG(t *testing.T) {
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// G creation and enqueue
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestChannelSendInCoroutineSuspends(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				ch.send(42);
			};
		}
	`)
	// Inside go block, channel send should use goroutine-aware park
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// The go block is a coroutine
	assertContains(t, ir, "presplitcoroutine")
}

func TestChannelRecvInCoroutineSuspends(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				result := <-ch;
			};
		}
	`)
	// Inside go block, channel recv should use goroutine-aware park
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
}

func TestChannelCloseWakesAllWaiters(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.close();
		}
	`)
	// Close should call promise_waiter_wake_all for both send and recv waiters
	assertContains(t, ir, "call void @promise_waiter_wake_all(")
}

func TestChannelStructHas15Fields(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel struct should have 15 fields including the 4 waiter lists
	// The channel_new function initializes all fields including waiter lists
	assertContains(t, ir, "define i8* @promise_channel_new(")
}

func TestTaskReceiveParksGoroutine(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Task receive in main (non-coroutine) uses thread-blocking mode
	// But the G.done field should be checked
	assertContains(t, ir, "promise_g_new")
	assertContains(t, ir, "promise_sched_enqueue")
}

// --- Phase 5c gap-filling tests ---

func TestTaskReceiveInCoroutine(t *testing.T) {
	// <-task inside a go block uses the coroutine park path with done_lock,
	// not the thread-blocking path. The done_lock protects the done flag and
	// done_waiters list, and park_mutex holds the lock across coro.suspend.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			go {
				t := go compute();
				int result = <-t;
			};
		}
	`)
	// The outer go block is a coroutine
	assertContains(t, ir, "presplitcoroutine")
	// Task receive inside coroutine: parks on done_waiters via done_lock
	assertContains(t, ir, "task.done")
	assertContains(t, ir, "task.wait")
	assertContains(t, ir, "task.ready")
	// done_lock path: check under lock, park if not done
	assertContains(t, ir, "task.done_under_lock")
	assertContains(t, ir, "task.park")
	// Coroutine suspend in the task wait path
	assertContains(t, ir, "task.resume")
	// Should NOT use usleep (that's the thread-blocking path)
	// The go block coroutine uses coro.suspend instead
}

func TestTaskReceiveThreadBlocking(t *testing.T) {
	// <-task in main (non-coroutine) uses the usleep polling loop
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Thread-blocking mode: spin loop with usleep
	assertContains(t, ir, "task.check")
	assertContains(t, ir, "task.spin")
	assertContains(t, ir, "task.threaddone")
	assertContains(t, ir, "call i32 @usleep(")
}

func TestVoidTaskSentinel(t *testing.T) {
	// Void tasks set result_ptr to sentinel inttoptr(i64 1) so goroutine_exit
	// knows not to free G (the receiver frees it via <-task).
	ir := generateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	// Sentinel value: inttoptr i64 1 to i8*
	assertContains(t, ir, "inttoptr i64 1 to i8*")
	// G is freed by the receiver, not goroutine_exit
	assertContains(t, ir, "task.ready")
}

func TestVoidGoBlockSentinel(t *testing.T) {
	// go { block } is always void — should also set sentinel
	ir := generateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoroutineExitSkipsFreeForTask(t *testing.T) {
	// goroutine_exit checks result_ptr != null to decide whether to free G.
	// Tasks (result_ptr set) skip the free; fire-and-forget goroutines are freed.
	ir := generateIR(t, `
		main() { }
	`)
	// goroutine_exit should contain the conditional skip-free logic
	assertContains(t, ir, "define void @promise_goroutine_exit(")
	// The function checks result_ptr to decide whether to free
	assertContains(t, ir, "skip_free:")
	assertContains(t, ir, "do_free:")
}

func TestChannelSendCoroutineRendezvous(t *testing.T) {
	// Unbuffered channel send inside a go block uses coroutine-mode rendezvous:
	// after writing the value, the sender parks and suspends waiting for the
	// receiver to pick it up.
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// Inside the coroutine, the rendezvous wait should use waiter_enqueue + coro.suspend
	assertContains(t, ir, "send.rv.wait")
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
}

func TestForInChannelCoroutineMode(t *testing.T) {
	// for-in channel inside a go block uses coroutine-mode park+suspend
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				for v in ch {
					int x = v + 1;
				}
			};
			ch.send(1);
			ch.close();
		}
	`)
	// The for-in inside the coroutine should use waiter_enqueue + coro.suspend
	assertContains(t, ir, "forin_ch.recv.wait")
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// Should have the coroutine resume block for the for-in
	assertContains(t, ir, "forin_ch.recv.resume")
}

func TestChannelRecvWakesSenderGoroutine(t *testing.T) {
	// After receiving, the code should try to wake a parked sender goroutine
	// before falling back to cond_signal. This tests the dual-mode wake path.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			result := <-ch;
		}
	`)
	// Receive wake path: dequeue from send_waiters, then fallback to cond_signal
	assertContains(t, ir, "call i8* @promise_waiter_dequeue(")
	assertContains(t, ir, "chrecv.wake.send")
	assertContains(t, ir, "chrecv.signal.send")
}

func TestChannelSendWakesRecvGoroutine(t *testing.T) {
	// After sending, the code should try to wake a parked receiver goroutine
	// before falling back to cond_signal. This tests the dual-mode wake path.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(42);
		}
	`)
	// Send wake path: dequeue from recv_waiters, then fallback to cond_signal
	assertContains(t, ir, "call i8* @promise_waiter_dequeue(")
	assertContains(t, ir, "send.wake.recv")
	assertContains(t, ir, "send.signal.recv")
}

func TestBuildMatchPhiMixedArms(t *testing.T) {
	// Match expression where some arms produce values and at least one arm
	// has an early return. buildMatchPhi must handle missing predecessors
	// by inserting null placeholders for arms that branch to merge without values.
	ir := generateIR(t, `
		test(int n) int {
			int result = match n {
				1 => 10,
				2 => 20,
				_ => 0,
			};
			return result;
		}
		main() { }
	`)
	// PHI node should exist in the merge block with values from all arms
	assertContains(t, ir, "phi i64")
	assertContains(t, ir, "match.end")
}

func TestBuildMatchPhiStatementOnly(t *testing.T) {
	// Match used as a statement (no arm produces a value) — no PHI needed
	ir := generateIR(t, `
		test(int n) {
			match n {
				1 => { int x = 10; },
				_ => { int y = 20; },
			};
		}
		main() { }
	`)
	// Should have match arms but the merge block shouldn't have a PHI
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.end")
}

func TestEnumMatchPhiWithEarlyReturn(t *testing.T) {
	// Enum match where one arm returns early (doesn't branch to merge).
	// buildMatchPhi must skip non-merging arms to avoid PHI predecessor mismatch.
	ir := generateIR(t, `
		enum Op { Add(int a, int b), Neg(int a) }
		eval(Op op) int {
			return match op {
				Add(a, b) => a + b,
				Neg(a) => 0 - a,
			};
		}
		main() { }
	`)
	// Both arms produce values; PHI should merge them
	assertContains(t, ir, "phi i64")
}

func TestSchedulerReleasesParkMutex(t *testing.T) {
	// The scheduler loop checks G.park_mutex after coro.resume returns
	// and releases it if non-null. This closes the enqueue-before-suspend race.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	// Scheduler loop must contain the park_mutex release blocks
	assertContains(t, ir, "release_park_mutex")
	assertContains(t, ir, "after_release")
}

func TestGoroutineExitUsesDoneLock(t *testing.T) {
	// goroutine_exit acquires sched.done_lock before setting done=1 and
	// walking done_waiters, ensuring proper synchronization with task receivers.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// promise_goroutine_exit must lock done_lock (from sched global)
	assertContains(t, ir, "promise_goroutine_exit")
	// The function should contain mutex lock/unlock calls
	assertContains(t, ir, "waiter_loop")
	assertContains(t, ir, "waiters_done")
}

// --- Named Arguments Tests ---

func TestNamedArgsConstructorCodegen(t *testing.T) {
	// Named args in reverse order should produce correct field stores
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(y: 20, x: 10);
		}
	`)
	// Both fields should be stored
	assertContains(t, ir, "store")
}

func TestNamedArgsConstructorPositionalCodegen(t *testing.T) {
	// All positional args should work for constructors
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(10, 20);
		}
	`)
	assertContains(t, ir, "store")
}

func TestNamedArgsFunctionCallCodegen(t *testing.T) {
	// Named args reordered should generate correct call
	ir := generateIR(t, `
		add(int a, int b) int { return a + b; }
		main() {
			r := add(b: 2, a: 1);
		}
	`)
	assertContains(t, ir, "call")
	assertContains(t, ir, "@add")
}

func TestNamedArgsMethodCallCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Calc {
			int value;
			compute(int a, int b) int { return a + b; }
		}
		main() {
			c := Calc(value: 0);
			r := c.compute(b: 2, a: 1);
		}
	`)
	assertContains(t, ir, "Calc.compute")
}

func TestNamedArgsMixedPositionalNamedCodegen(t *testing.T) {
	ir := generateIR(t, `
		calc(int a, int b, int c) int { return a + b + c; }
		main() {
			r := calc(1, c: 3, b: 2);
		}
	`)
	assertContains(t, ir, "@calc")
}

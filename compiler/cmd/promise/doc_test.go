package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// docFromSource parses a .pr source string, injects std, runs DeclareAndDefine,
// and returns the emitDoc output.
func docFromSource(t *testing.T, source string, opts docOpts) string {
	t.Helper()

	// Parse the source
	input := antlr.NewInputStream(source)
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

	// Inject std as a glob import and load module scopes
	file = injectStdImport(file)
	moduleScopes, _, _, _ := loadModuleScopes("test.pr", file, sema.HostTargetInfo())

	// Run DeclareAndDefine with module scopes
	info, errs := sema.DeclareAndDefineWithModules(file, moduleScopes)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}

	var buf bytes.Buffer
	emitDoc(&buf, file, info, opts, "test.pr")
	return buf.String()
}

// assertContainsDoc checks that the output contains the expected substring.
func assertContainsDoc(t *testing.T, output, substr string) {
	t.Helper()
	if !strings.Contains(output, substr) {
		t.Errorf("expected output to contain %q\ngot:\n%s", substr, output)
	}
}

// assertNotContainsDoc checks that the output does NOT contain the substring.
func assertNotContainsDoc(t *testing.T, output, substr string) {
	t.Helper()
	if strings.Contains(output, substr) {
		t.Errorf("expected output to NOT contain %q\ngot:\n%s", substr, output)
	}
}

// === Basic output structure ===

func TestDocFileHeading(t *testing.T) {
	out := docFromSource(t, `
		type Foo `+"`public"+` { int x; }
	`, docOpts{publicOnly: true})
	assertContainsDoc(t, out, "# test.pr")
}

func TestDocTypeBasic(t *testing.T) {
	out := docFromSource(t, `
		type Server `+"`public `doc(\"An HTTP server.\")"+` {
			int port `+"`public"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "## Types")
	assertContainsDoc(t, out, "### Server")
	assertContainsDoc(t, out, "An HTTP server.")
	assertContainsDoc(t, out, "int port")
}

// Grammar: typeDecl = TYPE IDENT typeParams? inheritance? metaAnnotation* LBRACE ...
// inheritance must come BEFORE meta annotations.
func TestDocTypeWithParent(t *testing.T) {
	out := docFromSource(t, `
		type Animal `+"`public"+` {}
		type Dog is Animal `+"`public"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Dog is Animal")
	assertContainsDoc(t, out, "type Dog is Animal")
}

func TestDocTypeWithMethod(t *testing.T) {
	out := docFromSource(t, `
		type Calc `+"`public"+` {
			add(int a, int b) int `+"`public `doc(\"Adds two numbers.\")"+` {
				return a + b;
			}
		}
	`, docOpts{publicOnly: true})

	// Instance methods include `this` as receiver
	assertContainsDoc(t, out, "add(this, int a, int b) int")
	assertContainsDoc(t, out, "#### Calc.add")
	assertContainsDoc(t, out, "Adds two numbers.")
}

func TestDocTypeGetter(t *testing.T) {
	out := docFromSource(t, `
		type Rect `+"`public"+` {
			int w `+"`public"+`;
			int h `+"`public"+`;
			get area int `+"`public `doc(\"Area of the rectangle.\")"+` => this.w * this.h;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "get area int")
	assertContainsDoc(t, out, "#### Rect.area (getter)")
	assertContainsDoc(t, out, "Area of the rectangle.")
}

func TestDocDropMethod(t *testing.T) {
	out := docFromSource(t, `
		type Resource `+"`public"+` {
			drop(~this) `+"`public `doc(\"Releases the resource.\")"+` {}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "drop(~this)")
	assertContainsDoc(t, out, "Releases the resource.")
	assertContainsDoc(t, out, "Called automatically at scope exit.")
}

func TestDocDropMethodNoDoc(t *testing.T) {
	out := docFromSource(t, `
		type Resource `+"`public"+` {
			drop(~this) `+"`public"+` {}
		}
	`, docOpts{publicOnly: true})

	// drop with no doc still appears in the summary block
	assertContainsDoc(t, out, "drop(~this)")
	// But no per-method section is emitted (no doc to show beyond the summary)
}

// === Enum documentation ===

func TestDocEnumFlat(t *testing.T) {
	out := docFromSource(t, `
		enum Direction `+"`public `doc(\"Cardinal directions.\")"+` {
			North, South, East, West,
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "## Enums")
	assertContainsDoc(t, out, "### Direction")
	assertContainsDoc(t, out, "Cardinal directions.")
	assertContainsDoc(t, out, "Variants:")
	assertContainsDoc(t, out, "`North`")
	assertContainsDoc(t, out, "`West`")
}

func TestDocEnumWithPayload(t *testing.T) {
	out := docFromSource(t, `
		enum Result `+"`public"+` {
			Ok(int value) `+"`doc(\"Success.\")"+`,
			Err(string msg) `+"`doc(\"Failure.\")"+`,
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Result")
	assertContainsDoc(t, out, "Variants:")
	assertContainsDoc(t, out, "Success.")
	assertContainsDoc(t, out, "Failure.")
}

func TestDocEnumCopy(t *testing.T) {
	out := docFromSource(t, `
		enum Color `+"`public `copy"+` {
			Red, Green, Blue,
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Color `copy")
}

// === Function documentation ===

func TestDocFuncBasic(t *testing.T) {
	out := docFromSource(t, `
		greet(string name) string `+"`public `doc(\"Greets someone.\")"+` {
			return "hello " + name;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "## Functions")
	assertContainsDoc(t, out, "### greet")
	assertContainsDoc(t, out, "Greets someone.")
	assertContainsDoc(t, out, "greet(string name) string")
}

func TestDocFuncWithParamDoc(t *testing.T) {
	out := docFromSource(t, `
		fetch(string url `+"`doc(\"The URL to fetch.\")"+`) string `+"`public `doc(\"Fetches a URL.\")"+` {
			return url;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "Fetches a URL.")
	assertContainsDoc(t, out, "Parameters:")
	assertContainsDoc(t, out, "`url` — The URL to fetch.")
}

// === Filtering ===

func TestDocPublicFiltering(t *testing.T) {
	// Members of a public type are public by default; _ prefix = private.
	out := docFromSource(t, `
		type Public `+"`public"+` {
			int visible;
			int _hidden;
			show() {}
			_hide() {}
		}
		type Private { int x; }
		internal_func() {}
		public_func() `+"`public"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Public")
	assertNotContainsDoc(t, out, "### Private")
	assertContainsDoc(t, out, "int visible")
	assertNotContainsDoc(t, out, "int _hidden")
	// Instance methods include `this` receiver
	assertContainsDoc(t, out, "show(this)")
	assertNotContainsDoc(t, out, "_hide(")
	assertContainsDoc(t, out, "### public_func")
	assertNotContainsDoc(t, out, "### internal_func")
}

func TestDocAllMode(t *testing.T) {
	out := docFromSource(t, `
		type Public `+"`public"+` { int x; }
		type Private { int y; }
	`, docOpts{publicOnly: false})

	assertContainsDoc(t, out, "### Public")
	assertContainsDoc(t, out, "### Private")
}

// === Signatures-only mode ===

func TestDocSignaturesMode(t *testing.T) {
	out := docFromSource(t, `
		type Server `+"`public `doc(\"HTTP server.\")"+` {
			int port `+"`public"+`;
			start() `+"`public `doc(\"Starts the server.\")"+` {}
		}
		greet() `+"`public `doc(\"Says hello.\")"+` {}
	`, docOpts{publicOnly: true, sigOnly: true})

	// Should have summary blocks
	assertContainsDoc(t, out, "type Server {")
	assertContainsDoc(t, out, "int port")
	assertContainsDoc(t, out, "start(this)")

	// Should NOT have section headers or doc strings
	assertNotContainsDoc(t, out, "## Types")
	assertNotContainsDoc(t, out, "HTTP server.")
	assertNotContainsDoc(t, out, "Starts the server.")
	assertNotContainsDoc(t, out, "Says hello.")
	assertNotContainsDoc(t, out, "### Server")
	assertNotContainsDoc(t, out, "#### Server.start")
}

// === Skip test/main functions ===

func TestDocSkipsMainAndTest(t *testing.T) {
	out := docFromSource(t, `
		helper() int `+"`public"+` { return 1; }
		main() {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### helper")
	assertNotContainsDoc(t, out, "### main")
}

// === Field defaults ===

func TestDocFieldDefault(t *testing.T) {
	out := docFromSource(t, `
		type Config `+"`public"+` {
			int port `+"`public"+` = 8080;
			string host `+"`public"+` = "localhost";
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "int port = 8080")
	assertContainsDoc(t, out, `string host = "localhost"`)
}

// === Field doc strings ===

func TestDocFieldDoc(t *testing.T) {
	out := docFromSource(t, `
		type Config `+"`public"+` {
			int port `+"`public `doc(\"The port to listen on.\")"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "int port")
	assertContainsDoc(t, out, "The port to listen on.")
}

// === Deprecated ===

func TestDocDeprecatedType(t *testing.T) {
	out := docFromSource(t, `
		type OldThing `+"`public `deprecated(\"use NewThing\")"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "DEPRECATED")
	assertContainsDoc(t, out, "use NewThing")
}

func TestDocDeprecatedFunc(t *testing.T) {
	out := docFromSource(t, `
		old_func() `+"`public `deprecated(\"use new_func\")"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### old_func")
	assertContainsDoc(t, out, "DEPRECATED")
}

// === Enum compact mode ===

func TestDocEnumCompactFlat(t *testing.T) {
	out := docFromSource(t, `
		enum Dir `+"`public"+` { N, S, E, W }
	`, docOpts{publicOnly: true, sigOnly: true})

	assertContainsDoc(t, out, "enum Dir { N, S, E, W }")
}

func TestDocEnumCompactPayload(t *testing.T) {
	out := docFromSource(t, `
		enum Shape `+"`public"+` {
			Circle(f64 r),
			Rect(f64 w, f64 h),
		}
	`, docOpts{publicOnly: true, sigOnly: true})

	assertContainsDoc(t, out, "enum Shape { Circle(f64 r), Rect(f64 w, f64 h) }")
}

// === Enum compact mode: methods distinguish getters from methods (T0868) ===

func TestDocEnumCompactWithMethods(t *testing.T) {
	// In -signatures mode, an enum with methods must render a block that shows
	// getters (no parens) and methods (parens) so the access form is
	// unambiguous — the exact confusion the contributor flagged
	// (`is_string` getter vs `as_string()` method on JsonValue).
	out := docFromSource(t, `
		enum Json `+"`public"+` {
			Null,
			Str(string value),

			get is_null bool `+"`public"+` {
				match this { Json.Null => { return true; }, _ => { return false; } }
			}
			as_string(this) string? `+"`public"+` {
				match this { Json.Str(s) => { return s; }, _ => { return none; } }
			}
		}
	`, docOpts{publicOnly: true, sigOnly: true})

	// Block form (multi-line), not the one-liner.
	assertContainsDoc(t, out, "enum Json {")
	assertContainsDoc(t, out, "Null, Str(string value)")
	// Getter renders with `get`, no parens.
	assertContainsDoc(t, out, "get is_null bool")
	// Method renders with parens.
	assertContainsDoc(t, out, "as_string(this) string?")
	// The getter must NOT appear in call form.
	assertNotContainsDoc(t, out, "is_null()")
}

func TestDocEnumCompactMethodsFiltered(t *testing.T) {
	// publicOnly must filter private enum methods out of the compact block too.
	out := docFromSource(t, `
		enum Status `+"`public"+` {
			Active, Inactive,

			get label int `+"`public"+` { return 1; }
			_secret() int { return 0; }
		}
	`, docOpts{publicOnly: true, sigOnly: true})

	assertContainsDoc(t, out, "enum Status {")
	assertContainsDoc(t, out, "get label int")
	assertNotContainsDoc(t, out, "_secret")
}

// === Type summary cost-signal distinction (T0868) ===

func TestDocTypeSummaryGetterVsMethod(t *testing.T) {
	// The canonical cost-signal scenario the task is about: in a type's
	// -signatures summary block a cheap/pure getter renders WITHOUT parens
	// (`get len int`) while a cost-bearing method renders WITH parens
	// (`to_string() string`). This is what makes the access form — and the
	// implied call cost — unambiguous in the generated API reference.
	out := docFromSource(t, `
		type Buffer `+"`public"+` {
			int _len;

			get len int `+"`public"+` { return this._len; }
			to_string(this) string `+"`public"+` { return "buf"; }
		}
	`, docOpts{publicOnly: true, sigOnly: true})

	assertContainsDoc(t, out, "type Buffer {")
	// Getter: no parens (cheap, field-like).
	assertContainsDoc(t, out, "get len int")
	// Method: parens (allocates) — the cost signal.
	assertContainsDoc(t, out, "to_string(this) string")
	// The getter must NOT be rendered in call form anywhere.
	assertNotContainsDoc(t, out, "len()")
}

// === Operators ===

func TestDocOperators(t *testing.T) {
	out := docFromSource(t, `
		type Vec2 `+"`public"+` {
			f64 x `+"`public"+`;
			f64 y `+"`public"+`;
			+(Vec2 a, Vec2 b) Vec2 `+"`public"+` {
				return Vec2(x: a.x + b.x, y: a.y + b.y);
			}
			==(Vec2 a, Vec2 b) bool `+"`public"+` {
				return a.x == b.x && a.y == b.y;
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "Operators:")
	assertContainsDoc(t, out, "+")
	assertContainsDoc(t, out, "==")
}

// === Structural interface ===

func TestDocStructuralInterface(t *testing.T) {
	out := docFromSource(t, `
		type Printable `+"`public `structural"+` {
			to_string() string `+"`abstract"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "`structural")
	assertContainsDoc(t, out, "Structural interface")
	assertContainsDoc(t, out, "to_string(") // method is listed
}

// === exprToString ===

func TestExprToString(t *testing.T) {
	// We test exprToString indirectly through field/param defaults
	out := docFromSource(t, `
		type Config `+"`public"+` {
			int count `+"`public"+` = 42;
			bool enabled `+"`public"+` = true;
			bool disabled `+"`public"+` = false;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "int count = 42")
	assertContainsDoc(t, out, "bool enabled = true")
	assertContainsDoc(t, out, "bool disabled = false")
}

// === Empty file ===

func TestDocEmptyFile(t *testing.T) {
	out := docFromSource(t, `
		main() {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "# test.pr")
	// Should not have any section headers
	assertNotContainsDoc(t, out, "## Types")
	assertNotContainsDoc(t, out, "## Enums")
	assertNotContainsDoc(t, out, "## Functions")
}

// === Method with param defaults ===

func TestDocMethodParamDefault(t *testing.T) {
	out := docFromSource(t, `
		type Server `+"`public"+` {
			listen(int port = 8080) `+"`public"+` {}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "listen(this, int port = 8080)")
}

// === Failable method ===

func TestDocFailableMethod(t *testing.T) {
	out := docFromSource(t, `
		type Client `+"`public"+` {
			fetch!(string url) string `+"`public"+` {
				return url;
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "fetch!(this, string url) string")
}

// === Combined test: all features ===

func TestDocComprehensive(t *testing.T) {
	// Grammar: inheritance comes BEFORE meta annotations
	out := docFromSource(t, `
		type Animal `+"`public `doc(\"Base animal type.\")"+` {
			string name `+"`public `doc(\"The animal's name.\")"+`;
			speak() string `+"`public `doc(\"Returns the sound.\")"+` {
				return "...";
			}
		}
		type Dog is Animal `+"`public `doc(\"A dog.\")"+` {
			bark() string `+"`public"+` {
				return "woof";
			}
		}
		enum Color `+"`public `copy `doc(\"A color.\")"+` {
			Red, Green, Blue,
		}
		greet(string name `+"`doc(\"Who to greet.\")"+`) string `+"`public `doc(\"Greets someone.\")"+` {
			return "hello " + name;
		}
	`, docOpts{publicOnly: true})

	// File heading
	assertContainsDoc(t, out, "# test.pr")

	// Type sections
	assertContainsDoc(t, out, "## Types")
	assertContainsDoc(t, out, "### Animal")
	assertContainsDoc(t, out, "Base animal type.")
	assertContainsDoc(t, out, "The animal's name.")
	assertContainsDoc(t, out, "### Dog is Animal")
	assertContainsDoc(t, out, "A dog.")

	// Enum
	assertContainsDoc(t, out, "## Enums")
	assertContainsDoc(t, out, "### Color `copy")
	assertContainsDoc(t, out, "A color.")

	// Function
	assertContainsDoc(t, out, "## Functions")
	assertContainsDoc(t, out, "### greet")
	assertContainsDoc(t, out, "Greets someone.")
	assertContainsDoc(t, out, "Parameters:")
	assertContainsDoc(t, out, "`name` — Who to greet.")
}

// === Inherited drop from parent ===

func TestDocInheritedDrop(t *testing.T) {
	// Grammar: inheritance comes BEFORE meta annotations
	out := docFromSource(t, `
		type Base `+"`public"+` {
			drop(~this) `+"`public `doc(\"Cleans up.\")"+` {}
		}
		type Child is Base `+"`public"+` {}
	`, docOpts{publicOnly: true})

	// The Child type should show the inherited drop method
	childIdx := strings.Index(out, "### Child")
	if childIdx == -1 {
		t.Fatal("expected ### Child heading")
	}
	childSection := out[childIdx:]
	if !strings.Contains(childSection, "drop(~this)") {
		t.Errorf("expected inherited drop in Child section\ngot:\n%s", childSection)
	}
}

// === No sections when no matching declarations ===

func TestDocNoTypesSection(t *testing.T) {
	out := docFromSource(t, `
		helper() int `+"`public"+` { return 1; }
	`, docOpts{publicOnly: true})

	assertNotContainsDoc(t, out, "## Types")
	assertNotContainsDoc(t, out, "## Enums")
	assertContainsDoc(t, out, "## Functions")
}

func TestDocNoFunctionsSection(t *testing.T) {
	out := docFromSource(t, `
		type Foo `+"`public"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "## Types")
	assertNotContainsDoc(t, out, "## Functions")
	assertNotContainsDoc(t, out, "## Enums")
}

// === Receiver ref mods ===

func TestDocReceiverRefMods(t *testing.T) {
	out := docFromSource(t, `
		type Obj `+"`public"+` {
			int x `+"`public"+`;
			mutate(~this) `+"`public"+` {
				this.x = 1;
			}
			inspect(this) int `+"`public"+` {
				return this.x;
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "mutate(~this)")
	assertContainsDoc(t, out, "inspect(this) int")
}

// === Divider between types ===

func TestDocTypeDivider(t *testing.T) {
	out := docFromSource(t, `
		type A `+"`public"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "---")
}

// === Func signatures-only mode ===

func TestDocFuncSignaturesMode(t *testing.T) {
	out := docFromSource(t, `
		compute(int a, int b) int `+"`public `doc(\"Does math.\")"+` {
			return a + b;
		}
	`, docOpts{publicOnly: true, sigOnly: true})

	// Should have just the signature
	assertContainsDoc(t, out, "compute(int a, int b) int")
	// Should NOT have section headers or doc
	assertNotContainsDoc(t, out, "### compute")
	assertNotContainsDoc(t, out, "Does math.")
}

// === Deprecated enum ===

func TestDocDeprecatedEnum(t *testing.T) {
	out := docFromSource(t, `
		enum OldEnum `+"`public `deprecated(\"use NewEnum\")"+` {
			A, B,
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "DEPRECATED")
	assertContainsDoc(t, out, "use NewEnum")
}

// === Enum copy compact mode ===

func TestDocEnumCompactCopy(t *testing.T) {
	out := docFromSource(t, `
		enum Status `+"`public `copy"+` { Active, Inactive }
	`, docOpts{publicOnly: true, sigOnly: true})

	assertContainsDoc(t, out, "enum Status `copy { Active, Inactive }")
}

// === Multiple param docs ===

func TestDocMultipleParamDocs(t *testing.T) {
	out := docFromSource(t, `
		connect(string host `+"`doc(\"The hostname.\")"+`, int port `+"`doc(\"The port number.\")"+`) `+"`public `doc(\"Connect to a server.\")"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "Connect to a server.")
	assertContainsDoc(t, out, "Parameters:")
	assertContainsDoc(t, out, "`host` — The hostname.")
	assertContainsDoc(t, out, "`port` — The port number.")
}

// === Generic types and functions (formatTypeParams coverage) ===

func TestDocGenericType(t *testing.T) {
	out := docFromSource(t, `
		type Box[T] `+"`public"+` {
			T value `+"`public"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Box")
	assertContainsDoc(t, out, "type Box[T]")
	assertContainsDoc(t, out, "T value")
}

func TestDocGenericTypeWithConstraint(t *testing.T) {
	out := docFromSource(t, `
		type Printable `+"`public `structural"+` {
			to_string() string `+"`abstract"+`;
		}
		type Wrapper[T: Printable] `+"`public"+` {
			T item `+"`public"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "type Wrapper[T: Printable]")
}

func TestDocGenericFunc(t *testing.T) {
	out := docFromSource(t, `
		identity[T](T x) T `+"`public `doc(\"Returns x unchanged.\")"+` {
			return x;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### identity")
	assertContainsDoc(t, out, "identity[T](T x) T")
	assertContainsDoc(t, out, "Returns x unchanged.")
}

func TestDocGenericEnum(t *testing.T) {
	out := docFromSource(t, `
		enum Maybe[T] `+"`public"+` {
			Some(T value),
			None,
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Maybe[T]")
	assertContainsDoc(t, out, "Some(T value)")
}

func TestDocGenericEnumCompact(t *testing.T) {
	out := docFromSource(t, `
		enum Pair[A, B] `+"`public"+` {
			Both(A first, B second),
			Empty,
		}
	`, docOpts{publicOnly: true, sigOnly: true})

	assertContainsDoc(t, out, "enum Pair[A, B]")
}

// === Factory method (emitMethodSection factory branch) ===

func TestDocFactoryMethod(t *testing.T) {
	out := docFromSource(t, `
		type MyBuilder `+"`public"+` {
			int value `+"`public"+`;
			create() MyBuilder `+"`public `factory `doc(\"Creates a new MyBuilder.\")"+` {
				return MyBuilder(value: 0);
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "#### MyBuilder.create")
	assertContainsDoc(t, out, "`factory")
	assertContainsDoc(t, out, "Creates a new MyBuilder.")
}

// === Enum with private method (collectEnumMethods filtering) ===

func TestDocEnumMethodFiltering(t *testing.T) {
	out := docFromSource(t, `
		enum Status `+"`public"+` {
			Active, Inactive,
			label() string `+"`doc(\"Human-readable label.\")"+` {
				return "status";
			}
			_internal() int {
				return 0;
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "label(")
	assertNotContainsDoc(t, out, "_internal")
}

// === exprToString: none literal and unary minus ===

func TestDocFieldDefaultNone(t *testing.T) {
	out := docFromSource(t, `
		type Node `+"`public"+` {
			int? value `+"`public"+` = none;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "int? value = none")
}

func TestDocFieldDefaultNegative(t *testing.T) {
	out := docFromSource(t, `
		type Offset `+"`public"+` {
			int x `+"`public"+` = -1;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "int x = -1")
}

// === Func with ref param (formatFuncSig ref modifier branch) ===

func TestDocFuncRefParam(t *testing.T) {
	out := docFromSource(t, `
		inspect(int val) int `+"`public"+` {
			return val;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "inspect(int val) int")
}

// === Subscript operators shown in summary, not operators line ===

func TestDocSubscriptOps(t *testing.T) {
	out := docFromSource(t, `
		type Grid `+"`public"+` {
			int _size;
			[](int idx) int `+"`public"+` {
				return idx;
			}
			+(Grid a, Grid b) Grid `+"`public"+` {
				return a;
			}
		}
	`, docOpts{publicOnly: true})

	// [] should be in the summary block (inside type { ... })
	assertContainsDoc(t, out, "[](")
	// + should be in the operators line
	assertContainsDoc(t, out, "Operators:")
	assertContainsDoc(t, out, "+")
}

// === Failable function ===

func TestDocFuncFailable(t *testing.T) {
	out := docFromSource(t, `
		parse!(string s) int `+"`public `doc(\"Parses a string to int.\")"+` {
			return 0;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "parse!(string s) int")
	assertContainsDoc(t, out, "Parses a string to int.")
}

// === Void return (typeString nil branch) ===

func TestDocVoidFunc(t *testing.T) {
	out := docFromSource(t, `
		noop() `+"`public"+` {}
	`, docOpts{publicOnly: true})

	// Void functions should not have a return type shown
	assertContainsDoc(t, out, "noop()")
	// The signature should end at ")" (no trailing type)
}

// === Function with default param (formatFuncSig default branch) ===

func TestDocFuncParamDefault(t *testing.T) {
	out := docFromSource(t, `
		greet(string name = "world") `+"`public"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, `greet(string name = "world")`)
}

// === Setter method ===

func TestDocSetterMethod(t *testing.T) {
	out := docFromSource(t, `
		type Counter `+"`public"+` {
			int _count;
			get count int `+"`public"+` => this._count;
			set count(int v) `+"`public"+` {
				this._count = v;
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "get count int")
	assertContainsDoc(t, out, "set count(")
}

// === Module-level getter ===

func TestDocModuleLevelGetter(t *testing.T) {
	out := docFromSource(t, `
		get hostname string `+"`public `doc(\"Returns the hostname.\")"+` {
			return "test";
		}
	`, docOpts{publicOnly: true})

	// Should show getter syntax, not function syntax
	assertContainsDoc(t, out, "get hostname string")
	assertContainsDoc(t, out, "(getter)")
	assertContainsDoc(t, out, "Returns the hostname.")
	// Should NOT show function-call syntax
	assertNotContainsDoc(t, out, "hostname()")
}

func TestDocModuleLevelGetterSignaturesMode(t *testing.T) {
	out := docFromSource(t, `
		get hostname string `+"`public `doc(\"Returns the hostname.\")"+` {
			return "test";
		}
	`, docOpts{publicOnly: true, sigOnly: true})

	// Compact mode should show getter syntax
	assertContainsDoc(t, out, "get hostname string")
	assertNotContainsDoc(t, out, "hostname()")
}

func TestDocModuleLevelGetterFailable(t *testing.T) {
	out := docFromSource(t, `
		get working_dir! string `+"`public"+` {
			return "test";
		}
	`, docOpts{publicOnly: true})

	// Should include ! for failable getter
	assertContainsDoc(t, out, "get working_dir! string")
	assertNotContainsDoc(t, out, "working_dir()")
}

func TestDocMethodGetterFailable(t *testing.T) {
	out := docFromSource(t, `
		type Conn `+"`public"+` {
			int _fd;
			get status! string `+"`public"+` {
				return "ok";
			}
		}
	`, docOpts{publicOnly: true})

	// Should include ! for failable method getter
	assertContainsDoc(t, out, "get status! string")
}

// === Field placement and final annotations ===

func TestDocFieldValueAnnotation(t *testing.T) {
	out := docFromSource(t, `
		type Vec2 `+"`public"+` {
			f64 x `+"`public `value"+`;
			f64 y `+"`public `value"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "f64 x `value")
	assertContainsDoc(t, out, "f64 y `value")
}

func TestDocFieldFinalAnnotation(t *testing.T) {
	out := docFromSource(t, `
		type Token `+"`public"+` {
			string raw `+"`public `final"+`;
			int line `+"`public `final"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "string raw `final")
	assertContainsDoc(t, out, "int line `final")
}

func TestDocFieldValueAndFinal(t *testing.T) {
	out := docFromSource(t, `
		type Point `+"`public"+` {
			f64 x `+"`public `value `final"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "f64 x `value `final")
}

// === Module-level setter ===

func TestDocModuleLevelSetter(t *testing.T) {
	out := docFromSource(t, `
		set log_level(int level) `+"`public"+` {}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "set log_level(int level)")
	// Should NOT show without "set" prefix (check start of signature line)
	assertNotContainsDoc(t, out, "    log_level(int level)")
}

func TestDocModuleLevelSetterSignaturesMode(t *testing.T) {
	out := docFromSource(t, `
		set log_level(int level) `+"`public"+` {}
	`, docOpts{publicOnly: true, sigOnly: true})

	assertContainsDoc(t, out, "set log_level(int level)")
}

// === Variadic parameters ===

func TestDocFuncVariadicParam(t *testing.T) {
	out := docFromSource(t, `
		sum(...int nums) int `+"`public"+` {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "sum(...int nums) int")
}

func TestDocMethodVariadicParam(t *testing.T) {
	out := docFromSource(t, `
		type Logger `+"`public"+` {
			log(~this, ...string messages) `+"`public"+` {}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "log(~this, ...string messages)")
}

// === Abstract method tag in signature ===

func TestDocAbstractMethodTag(t *testing.T) {
	out := docFromSource(t, `
		type Shape `+"`public"+` {
			area() f64 `+"`public `abstract"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "area(this) f64 `abstract")
}

// === Multiple parents ===

func TestDocMultipleParents(t *testing.T) {
	out := docFromSource(t, `
		type Named `+"`public"+` { string name `+"`public"+`; }
		type Audible `+"`public"+` {
			speak() string `+"`public `abstract"+`;
		}
		type Dog is Named, Audible `+"`public"+` {
			speak() string `+"`public"+` { return "woof"; }
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Dog is Named, Audible")
	assertContainsDoc(t, out, "type Dog is Named, Audible")
}

// === Void failable function ===

func TestDocVoidFailableFunc(t *testing.T) {
	out := docFromSource(t, `
		validate!(string input) `+"`public"+` {
			if input.is_empty { raise error(message: "empty"); }
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "validate!(string input)")
}

// === Float default in exprToString ===

func TestDocFieldDefaultFloat(t *testing.T) {
	out := docFromSource(t, `
		type Config `+"`public"+` {
			f64 rate `+"`public"+` = 0.5;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "f64 rate = 0.5")
}

// === Copy type heading (non-enum) ===

func TestDocCopyTypeHeading(t *testing.T) {
	out := docFromSource(t, `
		type Color `+"`public `copy"+` {
			int r `+"`public"+`;
			int g `+"`public"+`;
			int b `+"`public"+`;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### Color `copy")
	assertContainsDoc(t, out, "type Color `copy {")
}

// === Enum with getter method ===

func TestDocEnumGetter(t *testing.T) {
	out := docFromSource(t, `
		enum Shape `+"`public"+` {
			Circle(f64 radius),
			Point,

			get is_flat bool `+"`doc(\"True if the shape has no area.\")"+` {
				match this {
					Shape.Point => { return true; },
					_ => { return false; },
				}
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "get is_flat bool")
	assertContainsDoc(t, out, "True if the shape has no area.")
}

// === Test function skipping ===

func TestDocSkipsTestFunctions(t *testing.T) {
	out := docFromSource(t, `
		helper() int `+"`public"+` { return 1; }
		test_add() `+"`test"+` { assert(1 + 1 == 2); }
	`, docOpts{publicOnly: false})

	assertContainsDoc(t, out, "### helper")
	assertNotContainsDoc(t, out, "test_add")
}

// === Enum public filtering ===

func TestDocEnumPublicFiltering(t *testing.T) {
	out := docFromSource(t, `
		enum PublicEnum `+"`public"+` { A, B }
		enum PrivateEnum { X, Y }
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "### PublicEnum")
	assertNotContainsDoc(t, out, "PrivateEnum")
}

// === Planned module fallback (plan.md / readme.md) ===

// docFromModuleDir calls runDocModuleInDir with a pre-populated temp directory
// and returns the output as a string.
func docFromModuleDir(t *testing.T, files map[string]string, opts docOpts) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	var buf bytes.Buffer
	runDocModuleInDir(&buf, "testmod", dir, opts)
	return buf.String()
}

func TestDocModulePlanOnly(t *testing.T) {
	out := docFromModuleDir(t, map[string]string{
		"plan.md": "# testmod Plan\n\nThis is the design document.\n",
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "# testmod")
	assertContainsDoc(t, out, "Planned module")
	assertContainsDoc(t, out, "# testmod Plan")
	assertContainsDoc(t, out, "This is the design document.")
}

func TestDocModuleReadmeOnly(t *testing.T) {
	out := docFromModuleDir(t, map[string]string{
		"readme.md": "# testmod\n\nGeneral overview.\n",
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "# testmod")
	assertContainsDoc(t, out, "General overview.")
	// No "Planned module" callout when only readme (no plan)
	assertNotContainsDoc(t, out, "Planned module")
}

func TestDocModuleBothPlanAndReadme(t *testing.T) {
	out := docFromModuleDir(t, map[string]string{
		"readme.md": "# Readme\n\nOverview text.\n",
		"plan.md":   "# Plan\n\nAPI design.\n",
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "# testmod")
	assertContainsDoc(t, out, "Overview text.")
	assertContainsDoc(t, out, "Planned module")
	assertContainsDoc(t, out, "API design.")
	// Readme should appear before plan
	readmeIdx := strings.Index(out, "Overview text.")
	planIdx := strings.Index(out, "API design.")
	if readmeIdx >= planIdx {
		t.Errorf("expected readme content before plan content\ngot:\n%s", out)
	}
}

func TestDocModuleEmptyNoMarkdown(t *testing.T) {
	out := docFromModuleDir(t, map[string]string{}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "# testmod")
	// Should produce a non-empty helpful message, not blank
	assertContainsDoc(t, out, "planned but not yet implemented")
}

func TestDocModuleSourceButNoPubDecls(t *testing.T) {
	out := docFromModuleDir(t, map[string]string{
		"testmod.pr": "type _Private { int x; }\n",
		"plan.md":    "# testmod Plan\n\nDesign notes.\n",
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "# testmod")
	assertContainsDoc(t, out, "Planned module")
	assertContainsDoc(t, out, "Design notes.")
	// Private type should NOT appear in public-only output
	assertNotContainsDoc(t, out, "_Private")
}

func TestDocModuleReadmeCaseInsensitive(t *testing.T) {
	out := docFromModuleDir(t, map[string]string{
		"README.md": "# Readme uppercase\n\nContent here.\n",
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "Content here.")
}

func TestDocModuleFallbackNoTrailingNewline(t *testing.T) {
	// plan.md without trailing newline — exercises the HasSuffix branch in planFile path.
	out := docFromModuleDir(t, map[string]string{
		"plan.md": "No trailing newline here",
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "No trailing newline here")
	assertContainsDoc(t, out, "Planned module")
}

func TestDocModuleReadmeNoTrailingNewline(t *testing.T) {
	// readme.md without trailing newline — exercises the HasSuffix branch in readmeFile path.
	out := docFromModuleDir(t, map[string]string{
		"readme.md": "No trailing newline in readme",
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "No trailing newline in readme")
}

func TestDocModuleInDirWithPublicDecls(t *testing.T) {
	// Module dir with a .pr file that has public declarations — exercises the full
	// type/enum/func collection and emit path in runDocModuleInDir.
	out := docFromModuleDir(t, map[string]string{
		"testmod.pr": `
type Widget ` + "`public `doc(\"A UI widget.\")" + ` { int id ` + "`public" + `; }
enum Color ` + "`public" + ` { Red, Green, Blue, }
draw(int x, int y) ` + "`public `doc(\"Draws at position.\")" + ` {}
`,
	}, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "# testmod")
	assertContainsDoc(t, out, "## Types")
	assertContainsDoc(t, out, "### Widget")
	assertContainsDoc(t, out, "A UI widget.")
	assertContainsDoc(t, out, "## Enums")
	assertContainsDoc(t, out, "### Color")
	assertContainsDoc(t, out, "## Functions")
	assertContainsDoc(t, out, "### draw")
	assertContainsDoc(t, out, "Draws at position.")
	// Should NOT show the fallback message
	assertNotContainsDoc(t, out, "planned but not yet implemented")
	assertNotContainsDoc(t, out, "Planned module")
}

func TestDocModuleInDirSubdirsIgnored(t *testing.T) {
	// Subdirectories in the module dir should not be treated as source files.
	dir := t.TempDir()
	// Create a subdirectory — it should be silently skipped
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte("# Plan\nContent.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	runDocModuleInDir(&buf, "testmod", dir, docOpts{publicOnly: true})
	out := buf.String()
	assertContainsDoc(t, out, "# testmod")
	assertContainsDoc(t, out, "Content.")
}

// === Member ordering (T0243) ===

func TestDocMemberOrdering(t *testing.T) {
	out := docFromSource(t, `
		type Widget `+"`public"+` {
			int id `+"`public"+`;
			string name `+"`public"+`;

			create() Widget `+"`public `factory"+` {
				return Widget(id: 0, name: "w");
			}
			drop(~this) `+"`public"+` {}
			get alpha int `+"`public"+` { return 0; }
			beta() string `+"`public"+` { return "b"; }
			zebra() int `+"`public"+` { return 0; }
		}
	`, docOpts{publicOnly: true})

	// Find the summary block for Widget
	widgetIdx := strings.Index(out, "type Widget {")
	if widgetIdx == -1 {
		t.Fatalf("expected type Widget summary block\ngot:\n%s", out)
	}
	closingIdx := strings.Index(out[widgetIdx:], "    }")
	if closingIdx == -1 {
		t.Fatalf("expected closing brace\ngot:\n%s", out)
	}
	summary := out[widgetIdx : widgetIdx+closingIdx]

	// Factory should come before drop, drop before alpha/beta/zebra
	createIdx := strings.Index(summary, "create(")
	dropIdx := strings.Index(summary, "drop(")
	alphaIdx := strings.Index(summary, "alpha")
	betaIdx := strings.Index(summary, "beta(")
	zebraIdx := strings.Index(summary, "zebra(")

	if createIdx == -1 || dropIdx == -1 || alphaIdx == -1 || betaIdx == -1 || zebraIdx == -1 {
		t.Fatalf("missing expected methods in summary\ngot:\n%s", summary)
	}

	// Order: factory < drop < alphabetical remaining
	if createIdx >= dropIdx {
		t.Errorf("factory create() should come before drop()\ngot:\n%s", summary)
	}
	if dropIdx >= alphaIdx {
		t.Errorf("drop() should come before alpha\ngot:\n%s", summary)
	}
	if alphaIdx >= betaIdx {
		t.Errorf("alpha should come before beta()\ngot:\n%s", summary)
	}
	if betaIdx >= zebraIdx {
		t.Errorf("beta() should come before zebra()\ngot:\n%s", summary)
	}
}

func TestDocEnumMemberOrdering(t *testing.T) {
	out := docFromSource(t, `
		enum Direction `+"`public"+` {
			North, South, East, West,

			create() Direction `+"`public `factory `doc(\"Creates default.\")"+` {
				return Direction.North;
			}
			zebra() int `+"`public `doc(\"Zebra method.\")"+` { return 0; }
			alpha() int `+"`public `doc(\"Alpha method.\")"+` { return 0; }
		}
	`, docOpts{publicOnly: true})

	// Enum method sections appear as #### headings; check their order
	createIdx := strings.Index(out, "#### Direction.create")
	alphaIdx := strings.Index(out, "#### Direction.alpha")
	zebraIdx := strings.Index(out, "#### Direction.zebra")

	if createIdx == -1 || alphaIdx == -1 || zebraIdx == -1 {
		t.Fatalf("missing expected method sections\ngot:\n%s", out)
	}

	// Factory first, then alphabetical
	if createIdx >= alphaIdx {
		t.Errorf("factory create should come before alpha\ngot:\n%s", out)
	}
	if alphaIdx >= zebraIdx {
		t.Errorf("alpha should come before zebra\ngot:\n%s", out)
	}
}

func TestDocGetterSetterAdjacent(t *testing.T) {
	out := docFromSource(t, `
		type Config `+"`public"+` {
			int _value;
			get value int `+"`public"+` { return this._value; }
			set value(int v) `+"`public"+` { this._value = v; }
			alpha() int `+"`public"+` { return 0; }
			zebra() int `+"`public"+` { return 0; }
		}
	`, docOpts{publicOnly: true})

	configIdx := strings.Index(out, "type Config {")
	if configIdx == -1 {
		t.Fatalf("expected type Config summary block\ngot:\n%s", out)
	}
	closingIdx := strings.Index(out[configIdx:], "    }")
	if closingIdx == -1 {
		t.Fatalf("expected closing brace\ngot:\n%s", out)
	}
	summary := out[configIdx : configIdx+closingIdx]

	alphaIdx := strings.Index(summary, "alpha(")
	valueGetIdx := strings.Index(summary, "get value")
	valueSetIdx := strings.Index(summary, "set value")
	zebraIdx := strings.Index(summary, "zebra(")

	if alphaIdx == -1 || valueGetIdx == -1 || valueSetIdx == -1 || zebraIdx == -1 {
		t.Fatalf("missing expected methods in summary\ngot:\n%s", summary)
	}

	// alpha < value getter/setter < zebra (alphabetical)
	if alphaIdx >= valueGetIdx {
		t.Errorf("alpha() should come before get value\ngot:\n%s", summary)
	}
	// getter and setter adjacent (both named "value", stable sort keeps declaration order)
	if valueGetIdx >= valueSetIdx {
		t.Errorf("get value should come before set value\ngot:\n%s", summary)
	}
	if valueSetIdx >= zebraIdx {
		t.Errorf("set value should come before zebra()\ngot:\n%s", summary)
	}
}

func TestDocMemberOrderingMultipleFactoriesAndDropClose(t *testing.T) {
	out := docFromSource(t, `
		type Resource `+"`public"+` {
			int id `+"`public"+`;

			make_a() Resource `+"`public `factory"+` {
				return Resource(id: 1);
			}
			make_b() Resource `+"`public `factory"+` {
				return Resource(id: 2);
			}
			drop(~this) `+"`public"+` {}
			close(~this) `+"`public"+` {}
			zebra() int `+"`public"+` { return 0; }
			alpha() int `+"`public"+` { return 0; }
		}
	`, docOpts{publicOnly: true})

	resIdx := strings.Index(out, "type Resource {")
	if resIdx == -1 {
		t.Fatalf("expected type Resource summary block\ngot:\n%s", out)
	}
	closingIdx := strings.Index(out[resIdx:], "    }")
	if closingIdx == -1 {
		t.Fatalf("expected closing brace\ngot:\n%s", out)
	}
	summary := out[resIdx : resIdx+closingIdx]

	makeAIdx := strings.Index(summary, "make_a(")
	makeBIdx := strings.Index(summary, "make_b(")
	dropIdx := strings.Index(summary, "drop(")
	closeIdx := strings.Index(summary, "close(")
	alphaIdx := strings.Index(summary, "alpha(")
	zebraIdx := strings.Index(summary, "zebra(")

	if makeAIdx == -1 || makeBIdx == -1 || dropIdx == -1 || closeIdx == -1 || alphaIdx == -1 || zebraIdx == -1 {
		t.Fatalf("missing expected methods in summary\ngot:\n%s", summary)
	}

	// Factories first (declaration order preserved), then drop/close (declaration order), then alphabetical
	if makeAIdx >= makeBIdx {
		t.Errorf("make_a() should come before make_b() (declaration order)\ngot:\n%s", summary)
	}
	if makeBIdx >= dropIdx {
		t.Errorf("make_b() should come before drop()\ngot:\n%s", summary)
	}
	if dropIdx >= closeIdx {
		t.Errorf("drop() should come before close() (declaration order)\ngot:\n%s", summary)
	}
	if closeIdx >= alphaIdx {
		t.Errorf("close() should come before alpha()\ngot:\n%s", summary)
	}
	if alphaIdx >= zebraIdx {
		t.Errorf("alpha() should come before zebra()\ngot:\n%s", summary)
	}
}

func TestDocUsageContainsModules(t *testing.T) {
	var buf bytes.Buffer
	printDocUsage(&buf)
	out := buf.String()

	// Must contain usage line
	if !strings.Contains(out, "usage: promise doc") {
		t.Errorf("expected usage line, got:\n%s", out)
	}

	// Must list options
	if !strings.Contains(out, "-public") {
		t.Errorf("expected -public option, got:\n%s", out)
	}
	if !strings.Contains(out, "-signatures") {
		t.Errorf("expected -signatures option, got:\n%s", out)
	}

	// Must list available modules (from embedded catalog)
	if len(embeddedCatalog) > 0 {
		if !strings.Contains(out, "Available modules:") {
			t.Errorf("expected 'Available modules:' section, got:\n%s", out)
		}
		// Check a few known modules
		for _, mod := range []string{"std", "io", "json", "os", "path"} {
			if !strings.Contains(out, mod) {
				t.Errorf("expected module %q in output, got:\n%s", mod, out)
			}
		}
	}

	// Must contain examples
	if !strings.Contains(out, "Examples:") {
		t.Errorf("expected 'Examples:' section, got:\n%s", out)
	}
}

// === T0699: module-name resolution for local directories ===

func TestResolveLocalModuleNameFromToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"my_module\"\nepoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolveLocalModuleName(dir)
	if got != "my_module" {
		t.Errorf("expected 'my_module' from promise.toml, got %q", got)
	}
}

func TestResolveLocalModuleNameFromBasename(t *testing.T) {
	// Directory without promise.toml — falls back to absolute-path basename.
	dir := t.TempDir() // e.g. /tmp/TestResolveLocal.../001
	base := filepath.Base(dir)
	got := resolveLocalModuleName(dir)
	if got != base {
		t.Errorf("expected %q (basename), got %q", base, got)
	}
}

func TestResolveLocalModuleNameDotResolvesToCwdBasename(t *testing.T) {
	// "." should resolve to the cwd's basename, never the literal "." string.
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	got := resolveLocalModuleName(".")
	if got == "." || got == "" {
		t.Errorf("expected basename, got %q", got)
	}
	// Use EvalSymlinks because t.TempDir() on macOS may return a /var/folders
	// path whose absolute form goes through /private; the dir argument is "."
	// so filepath.Abs(".") and the original `dir` may differ in symlink form.
	wantAbs, _ := filepath.EvalSymlinks(dir)
	want := filepath.Base(wantAbs)
	if got != want {
		t.Errorf("expected %q (cwd basename), got %q", want, got)
	}
}

func TestDocModuleInDirUsesResolvedHeading(t *testing.T) {
	// Sanity check: runDocModuleInDir honors the displayName passed in,
	// independent of the directory path. This is the contract runDocModule relies on.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.pr"),
		[]byte("hello() `public `doc(\"says hi\") { }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	runDocModuleInDir(&buf, "resolved_name", dir, docOpts{publicOnly: true})
	out := buf.String()
	assertContainsDoc(t, out, "# resolved_name")
	assertNotContainsDoc(t, out, "planned but not yet implemented")
}

// TestRunInitDocRegression is the T0699 acceptance-criteria regression: a
// freshly-initialized project must produce non-empty docs when run through
// `promise doc <path>`, with a heading that uses the resolved module name
// (not the literal "." / ".." passed on the CLI) and at least one declaration
// from the init template.
func TestRunInitDocRegression(t *testing.T) {
	parent := t.TempDir()
	projectName := "myproj_t0699"
	projectDir := filepath.Join(parent, projectName)

	// Silence stdout during init.
	oldStdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	runInit([]string{projectDir})
	os.Stdout = oldStdout
	devnull.Close()

	// Verify the template includes a documented public declaration.
	mainPr, err := os.ReadFile(filepath.Join(projectDir, "main.pr"))
	if err != nil {
		t.Fatalf("main.pr not created: %v", err)
	}
	if !strings.Contains(string(mainPr), "greet") || !strings.Contains(string(mainPr), "`public") || !strings.Contains(string(mainPr), "`doc(") {
		t.Fatalf("init template missing documented public function; got:\n%s", string(mainPr))
	}

	// Run promise doc against the project from multiple path forms; each
	// must produce a non-stub heading derived from promise.toml + render greet.
	cases := []struct {
		label string
		setup func(t *testing.T) (target string, cleanup func())
	}{
		{
			label: "absolute path",
			setup: func(t *testing.T) (string, func()) { return projectDir, func() {} },
		},
		{
			label: "dot from inside",
			setup: func(t *testing.T) (string, func()) {
				orig, _ := os.Getwd()
				if err := os.Chdir(projectDir); err != nil {
					t.Fatal(err)
				}
				return ".", func() { os.Chdir(orig) }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			target, cleanup := tc.setup(t)
			defer cleanup()

			var buf bytes.Buffer
			runDocModule(&buf, target, docOpts{publicOnly: true})
			out := buf.String()

			// Heading uses the resolved module name from promise.toml,
			// never the raw CLI argument.
			assertContainsDoc(t, out, "# "+projectName)
			assertNotContainsDoc(t, out, "# .\n")
			assertNotContainsDoc(t, out, "# ..\n")

			// Init template's documented public function is rendered.
			assertContainsDoc(t, out, "### greet")
			assertContainsDoc(t, out, "Returns a friendly greeting for the given name.")

			// The stub fallback must NOT fire — there are real source decls here.
			assertNotContainsDoc(t, out, "planned but not yet implemented")
			assertNotContainsDoc(t, out, "Planned module")
		})
	}
}

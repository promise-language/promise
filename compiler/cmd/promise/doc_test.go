package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
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
	moduleScopes, _, _ := loadModuleScopes("test.pr", file, sema.TargetInfo{})

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

// === Operators ===

func TestDocOperators(t *testing.T) {
	out := docFromSource(t, `
		type Vec2 `+"`public"+` {
			f64 x `+"`public"+`;
			f64 y `+"`public"+`;
			+(Vec2 &a, Vec2 &b) Vec2 `+"`public"+` {
				return Vec2(x: a.x + b.x, y: a.y + b.y);
			}
			==(Vec2 &a, Vec2 &b) bool `+"`public"+` {
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
			fetch(string url) string! `+"`public"+` {
				return url;
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "fetch(this, string url) string!")
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
			inspect(&this) int `+"`public"+` {
				return this.x;
			}
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "mutate(~this)")
	assertContainsDoc(t, out, "inspect(&this) int")
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
		inspect(int &val) int `+"`public"+` {
			return val;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "inspect(int& val) int")
}

// === Subscript operators shown in summary, not operators line ===

func TestDocSubscriptOps(t *testing.T) {
	out := docFromSource(t, `
		type Grid `+"`public"+` {
			int _size;
			[](int idx) int `+"`public"+` {
				return idx;
			}
			+(Grid &a, Grid &b) Grid `+"`public"+` {
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
		parse(string s) int! `+"`public `doc(\"Parses a string to int.\")"+` {
			return 0;
		}
	`, docOpts{publicOnly: true})

	assertContainsDoc(t, out, "parse(string s) int!")
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

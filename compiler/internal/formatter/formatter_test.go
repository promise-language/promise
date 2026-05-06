package formatter

import (
	"testing"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty file",
			input:    "",
			expected: "",
		},
		{
			name:     "trailing whitespace removed",
			input:    "int x = 1;   \n",
			expected: "int x = 1;\n",
		},
		{
			name:     "multiple trailing newlines normalized",
			input:    "int x = 1;\n\n\n\n",
			expected: "int x = 1;\n",
		},
		{
			name:     "simple variable declaration",
			input:    "int   x  =  1 ;",
			expected: "int x = 1;\n",
		},
		{
			name:     "function declaration",
			input:    "main(){\nprint_line(\"hello\");\n}",
			expected: "main() {\n  print_line(\"hello\");\n}\n",
		},
		{
			name:     "2-space indent",
			input:    "main() {\n    int x = 1;\n    int y = 2;\n}",
			expected: "main() {\n  int x = 1;\n  int y = 2;\n}\n",
		},
		{
			name:     "nested indentation",
			input:    "main() {\nif true {\nint x = 1;\n}\n}",
			expected: "main() {\n  if true {\n    int x = 1;\n  }\n}\n",
		},
		{
			name:     "binary operators spaced",
			input:    "int x=a+b*c;",
			expected: "int x = a + b * c;\n",
		},
		{
			name:     "comparison operators",
			input:    "if a==b&&c!=d{\n}",
			expected: "if a == b && c != d {\n}\n",
		},
		{
			name:     "no space around dot",
			input:    "foo . bar . baz ( )",
			expected: "foo.bar.baz()\n",
		},
		{
			name:     "no space inside parens",
			input:    "foo( a , b , c )",
			expected: "foo(a, b, c)\n",
		},
		{
			name:     "no space inside brackets",
			input:    "arr[ 0 ]",
			expected: "arr[0]\n",
		},
		{
			name:     "walrus operator",
			input:    "x:=5;",
			expected: "x := 5;\n",
		},
		{
			name:     "compound assignment",
			input:    "x+=1;",
			expected: "x += 1;\n",
		},
		{
			name:     "unary minus",
			input:    "int x = -1;",
			expected: "int x = -1;\n",
		},
		{
			name:     "unary not",
			input:    "if !flag {\n}",
			expected: "if !flag {\n}\n",
		},
		{
			name:     "unary tilde",
			input:    "int x = ~bits;",
			expected: "int x = ~bits;\n",
		},
		{
			name:     "receive operator",
			input:    "x := <- ch;",
			expected: "x := <-ch;\n",
		},
		{
			name:     "postfix question",
			input:    "result ? ;",
			expected: "result?;\n",
		},
		{
			name:     "postfix bang",
			input:    "result ! ;",
			expected: "result!;\n",
		},
		{
			name:     "arrow type",
			input:    "(int)->int",
			expected: "(int) -> int\n",
		},
		{
			name:     "fat arrow",
			input:    "x=>y",
			expected: "x => y\n",
		},
		{
			name:     "backtick meta",
			input:    "type Foo  `native  `public  {",
			expected: "type Foo `native `public {\n",
		},
		{
			name:     "semicolon no space before",
			input:    "return x ;",
			expected: "return x;\n",
		},
		{
			name:     "comma spacing",
			input:    "foo(a,b,c)",
			expected: "foo(a, b, c)\n",
		},
		{
			name:     "line comment preserved",
			input:    "int x = 1; // value\n",
			expected: "int x = 1; // value\n",
		},
		{
			name:     "line comment normalized spacing",
			input:    "//comment\n",
			expected: "// comment\n",
		},
		{
			name:     "own-line comment indented",
			input:    "main() {\n// hello\nint x = 1;\n}",
			expected: "main() {\n  // hello\n  int x = 1;\n}\n",
		},
		{
			name:     "max one blank line",
			input:    "int x = 1;\n\n\n\nint y = 2;\n",
			expected: "int x = 1;\n\nint y = 2;\n",
		},
		{
			name:     "no blank lines at block start",
			input:    "main() {\n\nint x = 1;\n}",
			expected: "main() {\n  int x = 1;\n}\n",
		},
		{
			name:     "no blank lines before closing brace",
			input:    "main() {\nint x = 1;\n\n}",
			expected: "main() {\n  int x = 1;\n}\n",
		},
		{
			name:     "type declaration",
			input:    "type  Foo {\nint x;\nint y;\n\nmethod(){\n}\n}",
			expected: "type Foo {\n  int x;\n  int y;\n\n  method() {\n  }\n}\n",
		},
		{
			name:     "named args",
			input:    "Foo(x:1,y:2)",
			expected: "Foo(x: 1, y: 2)\n",
		},
		{
			name:     "range operator no space",
			input:    "1 .. 10",
			expected: "1..10\n",
		},
		{
			name:     "inclusive range no space",
			input:    "1 ..= 10",
			expected: "1..=10\n",
		},
		{
			name:     "for in statement",
			input:    "for  item  in  list {\n}",
			expected: "for item in list {\n}\n",
		},
		{
			name:     "classic for",
			input:    "for i:=0;i<10;i++ {\n}",
			expected: "for i := 0; i < 10; i++ {\n}\n",
		},
		{
			name:     "lambda expression",
			input:    "|x|->x*2",
			expected: "|x| -> x * 2\n",
		},
		{
			name:     "move lambda",
			input:    "move|x|->x",
			expected: "move |x| -> x\n",
		},
		{
			name:     "string literal preserved",
			input:    "string s = \"hello world\";",
			expected: "string s = \"hello world\";\n",
		},
		{
			name:     "triple string preserved",
			input:    "string s = \"\"\"hello\nworld\"\"\";",
			expected: "string s = \"\"\"hello\nworld\"\"\";\n",
		},
		{
			name:     "char literal",
			input:    "char c = 'a';",
			expected: "char c = 'a';\n",
		},
		{
			name:     "if else",
			input:    "if x {\ny();\n} else {\nz();\n}",
			expected: "if x {\n  y();\n} else {\n  z();\n}\n",
		},
		{
			name:     "match expression",
			input:    "match x {\n1 => \"one\",\n2 => \"two\",\n_ => \"other\",\n}",
			expected: "match x {\n  1 => \"one\",\n  2 => \"two\",\n  _ => \"other\",\n}\n",
		},
		{
			name:     "generic type",
			input:    "Vector[int] v = Vector[int]();",
			expected: "Vector[int] v = Vector[int]();\n",
		},
		{
			name:     "method call chain",
			input:    "arr.iter().filter(pred).map(fn).collect()",
			expected: "arr.iter().filter(pred).map(fn).collect()\n",
		},
		{
			name:     "optional chain",
			input:    "obj ?. field ?. method()",
			expected: "obj?.field?.method()\n",
		},
		{
			name:     "elvis operator",
			input:    "x?:default",
			expected: "x ?: default\n",
		},
		{
			name:     "use declaration",
			input:    "use json;\nuse math;\n",
			expected: "use json;\nuse math;\n",
		},
		{
			name:     "use declarations sorted",
			input:    "use json;\nuse io;\nuse path;\n",
			expected: "use io;\nuse json;\nuse path;\n",
		},
		{
			name:     "use declarations already sorted",
			input:    "use io;\nuse json;\n",
			expected: "use io;\nuse json;\n",
		},
		{
			name:     "use declarations with alias sorted",
			input:    "use json as j;\nuse io;\n",
			expected: "use io;\nuse json as j;\n",
		},
		{
			name:     "use declarations with sourced import sorted",
			input:    "use parser \"github.com/acme/parser\";\nuse io;\n",
			expected: "use io;\nuse parser \"github.com/acme/parser\";\n",
		},
		{
			name:     "use declarations blank line preserves groups",
			input:    "use json;\nuse io;\n\nuse path;\nuse math;\n",
			expected: "use io;\nuse json;\n\nuse math;\nuse path;\n",
		},
		{
			name:     "use resource binding not sorted",
			input:    "use conn := open();\n",
			expected: "use conn := open();\n",
		},
		{
			name:     "use declarations with trailing comment",
			input:    "use json; // parsing\nuse io;\n",
			expected: "use io;\nuse json; // parsing\n",
		},
		{
			name:     "use with underscore alias sorted",
			input:    "use std as _;\nuse io;\n",
			expected: "use io;\nuse std as _;\n",
		},
		{
			name:     "use sourced with underscore binding sorted",
			input:    "use _ \"github.com/acme/lib\";\nuse parser \"github.com/acme/parser\";\n",
			expected: "use _ \"github.com/acme/lib\";\nuse parser \"github.com/acme/parser\";\n",
		},
		{
			name:     "use mixed forms sorted",
			input:    "use json;\nuse _ \"github.com/init\";\nuse io;\nuse parser \"github.com/acme/parser\";\n",
			expected: "use _ \"github.com/init\";\nuse io;\nuse json;\nuse parser \"github.com/acme/parser\";\n",
		},
		{
			name:     "is expression",
			input:    "if x is Dog {\n}",
			expected: "if x is Dog {\n}\n",
		},
		{
			name:     "as expression",
			input:    "x as int",
			expected: "x as int\n",
		},
		{
			name:     "return statement",
			input:    "return x;",
			expected: "return x;\n",
		},
		{
			name:     "enum declaration",
			input:    "enum Color {\nRed,\nGreen,\nBlue,\n}",
			expected: "enum Color {\n  Red,\n  Green,\n  Blue,\n}\n",
		},
		{
			name:     "while loop",
			input:    "while x<10 {\nx+=1;\n}",
			expected: "while x < 10 {\n  x += 1;\n}\n",
		},
		{
			name:     "block comment",
			input:    "/* hello */ int x = 1;",
			expected: "/* hello */ int x = 1;\n",
		},
		{
			name:     "increment decrement",
			input:    "i ++ ;",
			expected: "i++;\n",
		},
		{
			name:     "type with inheritance",
			input:    "type Dog is Animal {\nstring name;\n}",
			expected: "type Dog is Animal {\n  string name;\n}\n",
		},
		{
			name:     "shift operators",
			input:    "x<<2",
			expected: "x << 2\n",
		},
		{
			name:     "bitwise AND spaced",
			input:    "0b1100&0b1010",
			expected: "0b1100 & 0b1010\n",
		},
		{
			name:     "empty map literal stays inline",
			input:    "map[string, int] m = {:};",
			expected: "map[string, int] m = {:};\n",
		},
		{
			name:     "map literal int keys has space after colon",
			input:    `map[int, string] m = {1:"one", 2:"two"};`,
			expected: "map[int, string] m = {\n  1: \"one\", 2: \"two\"\n};\n",
		},
		{
			name:     "map literal single line preserved",
			input:    `map[string, int] m = {"a": 1, "b": 2, "c": 3};`,
			expected: "map[string, int] m = {\n  \"a\": 1, \"b\": 2, \"c\": 3\n};\n",
		},
		{
			name:     "slice type",
			input:    "int[] arr;",
			expected: "int[] arr;\n",
		},
		{
			name:     "map type",
			input:    "Map[string,int] m;",
			expected: "Map[string, int] m;\n",
		},
		{
			name:     "ref modifier",
			input:    "method( & this ) {",
			expected: "method(&this) {\n",
		},
		{
			name:     "move modifier",
			input:    "method( ~ this ) {",
			expected: "method(~this) {\n",
		},
		{
			name:     "select statement",
			input:    "select {\nch.send(v):\nx();\ndefault:\ny();\n}",
			expected: "select {\n  ch.send(v):\n    x();\n  default:\n    y();\n}\n",
		},
		{
			name:     "select empty default same line",
			input:    "select {\nv := <-ch: default:\n}",
			expected: "select {\n  v := <-ch: default:\n}\n",
		},
		{
			name:     "select with if in body",
			input:    "select {\nv := <-ch:\nif val := v {\nuse(val);\n}\ndefault:\ny();\n}",
			expected: "select {\n  v := <-ch:\n    if val := v {\n      use(val);\n    }\n  default:\n    y();\n}\n",
		},
		{
			name:     "select case arm with nested call",
			input:    "select {\nch.send(foo(bar[0])):\nx();\ndefault:\ny();\n}",
			expected: "select {\n  ch.send(foo(bar[0])):\n    x();\n  default:\n    y();\n}\n",
		},
		{
			name:     "go expression",
			input:    "go {\nwork();\n}",
			expected: "go {\n  work();\n}\n",
		},
		{
			name:     "unsafe block",
			input:    "unsafe {\nptr = null;\n}",
			expected: "unsafe {\n  ptr = null;\n}\n",
		},
		{
			name:     "yield statement",
			input:    "yield x;",
			expected: "yield x;\n",
		},
		{
			name:     "array literal",
			input:    "[1,2,3]",
			expected: "[1, 2, 3]\n",
		},
		{
			name:     "empty array",
			input:    "int[] x = [];",
			expected: "int[] x = [];\n",
		},
		{
			name:     "tuple literal",
			input:    "(1,\"two\",3.0)",
			expected: "(1, \"two\", 3.0)\n",
		},
		{
			name:     "raise statement",
			input:    "raise Error(\"oops\");",
			expected: "raise Error(\"oops\");\n",
		},
		{
			name:     "break continue",
			input:    "break;\ncontinue;",
			expected: "break;\ncontinue;\n",
		},
		{
			name:     "if unwrap",
			input:    "if val := opt {\nuse(val);\n}",
			expected: "if val := opt {\n  use(val);\n}\n",
		},
		{
			name:     "numeric literal with suffix",
			input:    "int x = 42i32;",
			expected: "int x = 42i32;\n",
		},
		{
			name:     "bare i suffix",
			input:    "int x = 42i;",
			expected: "int x = 42i;\n",
		},
		{
			name:     "bare u suffix",
			input:    "uint x = 42u;",
			expected: "uint x = 42u;\n",
		},
		{
			name:     "hex literal",
			input:    "int x = 0xFF;",
			expected: "int x = 0xFF;\n",
		},
		{
			name:     "float literal",
			input:    "f64 x = 3.14;",
			expected: "f64 x = 3.14;\n",
		},
		{
			name:     "idempotent simple",
			input:    "main() {\n  int x = 1;\n}\n",
			expected: "main() {\n  int x = 1;\n}\n",
		},
		{
			name:     "return unary minus",
			input:    "return -x;",
			expected: "return -x;\n",
		},
		{
			name:     "assign unary tilde",
			input:    "y := ~x;",
			expected: "y := ~x;\n",
		},
		{
			name:     "binary minus vs unary",
			input:    "a-b",
			expected: "a - b\n",
		},
		{
			name:     "if else if",
			input:    "if x {\na();\n} else if y {\nb();\n} else {\nc();\n}",
			expected: "if x {\n  a();\n} else if y {\n  b();\n} else {\n  c();\n}\n",
		},
		{
			name:     "named args inline",
			input:    "Foo(x: 1, y: 2)",
			expected: "Foo(x: 1, y: 2)\n",
		},
		{
			name:     "pipe as bitwise or",
			input:    "x|y",
			expected: "x | y\n",
		},
		{
			name:     "move before lambda",
			input:    "move |x, y| -> x + y",
			expected: "move |x, y| -> x + y\n",
		},
		{
			name:     "nested function calls",
			input:    "foo(bar(baz(1, 2), 3), 4)",
			expected: "foo(bar(baz(1, 2), 3), 4)\n",
		},
		{
			name:     "chained comparison",
			input:    "a<=b||c>=d",
			expected: "a <= b || c >= d\n",
		},
		{
			name:     "receive in expression",
			input:    "x := <-ch + 1;",
			expected: "x := <-ch + 1;\n",
		},
		{
			name:     "return tuple needs space before paren",
			input:    "return(a, b);",
			expected: "return (a, b);\n",
		},
		{
			name:     "return parenthesized expr",
			input:    "return(x + 1);",
			expected: "return (x + 1);\n",
		},
		{
			name:     "raise with paren",
			input:    "raise(err);",
			expected: "raise (err);\n",
		},
		{
			name:     "yield with paren",
			input:    "yield(val);",
			expected: "yield (val);\n",
		},
		{
			name:     "negation of parenthesized expr",
			input:    "if !(a && b) {\n}",
			expected: "if !(a && b) {\n}\n",
		},
		{
			name:     "escaped backslash char",
			input:    "char c = '\\\\';",
			expected: "char c = '\\\\';\n",
		},
		{
			name:     "escaped quote char",
			input:    "char c = '\\'';",
			expected: "char c = '\\'';\n",
		},
		{
			name:     "nested lambda",
			input:    "|x|->|y|->x+y",
			expected: "|x| -> |y| -> x + y\n",
		},
		{
			name:     "lambda in assignment",
			input:    "f:=|x|->x+1;",
			expected: "f := |x| -> x + 1;\n",
		},
		{
			name:     "empty block",
			input:    "fn() {\n}",
			expected: "fn() {\n}\n",
		},
		{
			name:     "consecutive comments",
			input:    "// first\n// second\nint x = 1;",
			expected: "// first\n// second\nint x = 1;\n",
		},
		{
			name:     "operator method []= no space",
			input:    "[]=(int index, T value) `native;",
			expected: "[]=(int index, T value) `native;\n",
		},
		{
			name:     "operator method [:]= no space",
			input:    "[:]=(int start, int end, T[] values) {}",
			expected: "[:]=(int start, int end, T[] values) {\n}\n",
		},
		{
			name:     "negative number in range",
			input:    "for i in -3..3 {\n}",
			expected: "for i in -3..3 {\n}\n",
		},
		{
			name:     "slice no space around colon",
			input:    `"hello"[: 3]`,
			expected: "\"hello\"[:3]\n",
		},
		{
			name:     "slice with start and end no space",
			input:    `"hello"[1: 2]`,
			expected: "\"hello\"[1:2]\n",
		},
		{
			name:     "slice with expression no space around colon",
			input:    "p[i + 1:end]",
			expected: "p[i + 1:end]\n",
		},
		{
			name:     "slice with nested index no space around colon",
			input:    "arr[m[key]:end]",
			expected: "arr[m[key]:end]\n",
		},
		{
			name:     "negative number after comma",
			input:    "int[] v = [3, -1, 4];",
			expected: "int[] v = [3, -1, 4];\n",
		},
		{
			name:     "index assignment has space around =",
			input:    `m["a"]=1;`,
			expected: "m[\"a\"] = 1;\n",
		},
		{
			name:     "index assignment with paren expr",
			input:    "v[i]=(i + 1) * 10;",
			expected: "v[i] = (i + 1) * 10;\n",
		},
		{
			name:     "variadic param preserves space before ellipsis",
			input:    "join(string base, string child, ...string rest) {}",
			expected: "join(string base, string child, ...string rest) {\n}\n",
		},
		{
			name:     "comment after open brace",
			input:    "fn() {\n// body\nx();\n}",
			expected: "fn() {\n  // body\n  x();\n}\n",
		},
		{
			name:     "standalone comment after statement stays on own line",
			input:    "int x = 1;\n// This is a comment\nint y = 2;",
			expected: "int x = 1;\n// This is a comment\nint y = 2;\n",
		},
		{
			name:     "trailing comment on same line stays trailing",
			input:    "int x = 1; // trailing\nint y = 2;",
			expected: "int x = 1; // trailing\nint y = 2;\n",
		},
		{
			name:     "get accessor",
			input:    "get len int `native;",
			expected: "get len int `native;\n",
		},
		{
			name:     "set accessor",
			input:    "set val(int v){this._val=v;}",
			expected: "set val(int v) {\n  this._val = v;\n}\n",
		},
		{
			name:     "module-level get with body",
			input:    "get answer int{return 42;}",
			expected: "get answer int {\n  return 42;\n}\n",
		},
		{
			name:     "operator method declaration",
			input:    "+(int other) int `native;",
			expected: "+(int other) int `native;\n",
		},
		{
			name:     "failable method with bang",
			input:    "parse() int! {\n}",
			expected: "parse() int! {\n}\n",
		},
		{
			name:     "string interpolation preserved",
			input:    "string s = \"hello ${name}\";",
			expected: "string s = \"hello ${name}\";\n",
		},
		{
			name:     "interpolation with nested string literal",
			input:    "string s = \"{match x { 3 => \"three\", _ => \"other\", }}\";",
			expected: "string s = \"{match x { 3 => \"three\", _ => \"other\", }}\";\n",
		},
		{
			name:     "interpolation with simple expression",
			input:    "print_line(\"{x + 1}\");",
			expected: "print_line(\"{x + 1}\");\n",
		},
		{
			name:     "interpolation with nested braces",
			input:    "string s = \"{if x { \"yes\" } else { \"no\" }}\";",
			expected: "string s = \"{if x { \"yes\" } else { \"no\" }}\";\n",
		},
		{
			name:     "multiple interpolations in one string",
			input:    "string s = \"{a} and {b}\";",
			expected: "string s = \"{a} and {b}\";\n",
		},
		{
			name:     "escaped brace not interpolation",
			input:    "string s = \"\\{not interpolation}\";",
			expected: "string s = \"\\{not interpolation}\";\n",
		},
		{
			name:     "interpolation only string",
			input:    "string s = \"{x}\";",
			expected: "string s = \"{x}\";\n",
		},
		{
			name:     "empty interpolation",
			input:    "string s = \"{}\";",
			expected: "string s = \"{}\";\n",
		},
		{
			name:     "adjacent interpolations",
			input:    "string s = \"{a}{b}\";",
			expected: "string s = \"{a}{b}\";\n",
		},
		{
			name:     "deeply nested braces in interpolation",
			input:    "string s = \"{if a { if b { c } else { d } }}\";",
			expected: "string s = \"{if a { if b { c } else { d } }}\";\n",
		},
		{
			name:     "char literal inside interpolation",
			input:    "string s = \"{match c { 'x' => 1, _ => 0, }}\";",
			expected: "string s = \"{match c { 'x' => 1, _ => 0, }}\";\n",
		},
		{
			name:     "escaped char literal inside interpolation",
			input:    "string s = \"{match c { '\\n' => 1, _ => 0, }}\";",
			expected: "string s = \"{match c { '\\n' => 1, _ => 0, }}\";\n",
		},
		{
			name:     "escape inside nested string in interpolation",
			input:    "string s = \"{get(\"key\\\"val\")}\";",
			expected: "string s = \"{get(\"key\\\"val\")}\";\n",
		},
		{
			name:     "recursive interpolation in nested string",
			input:    "string s = \"{fmt(\"{x}\")}\";",
			expected: "string s = \"{fmt(\"{x}\")}\";\n",
		},
		{
			name:     "raw string inside interpolation",
			input:    "string s = \"{r\"path\\\\end\"}\";",
			expected: "string s = \"{r\"path\\\\end\"}\";\n",
		},
		{
			name:     "line comment with brace inside interpolation",
			input:    "string s = \"{x // }\n+ y}\";",
			expected: "string s = \"{x // }\n+ y}\";\n",
		},
		{
			name:     "block comment with brace inside interpolation",
			input:    "string s = \"{x /* } */ + y}\";",
			expected: "string s = \"{x /* } */ + y}\";\n",
		},
		{
			name:     "triple-quoted string inside interpolation",
			input:    "string s = \"{\"\"\"hello\"\"\"}\";",
			expected: "string s = \"{\"\"\"hello\"\"\"}\";\n",
		},
		{
			name:     "multiple args without trailing newline",
			input:    "foo(a, b, c)",
			expected: "foo(a, b, c)\n",
		},
		{
			name:     "match with block arm",
			input:    "match x {\n1 => {\nfoo();\n},\n}",
			expected: "match x {\n  1 => {\n    foo();\n  },\n}\n",
		},
		{
			name:     "underscore in match",
			input:    "match x {\n_ => y,\n}",
			expected: "match x {\n  _ => y,\n}\n",
		},
		// --- Trailing comma normalization ---
		{
			name:     "match adds trailing comma",
			input:    "match x {\n1 => \"one\",\n2 => \"two\"\n}",
			expected: "match x {\n  1 => \"one\",\n  2 => \"two\",\n}\n",
		},
		{
			name:     "match trailing comma already present",
			input:    "match x {\n1 => \"one\",\n}",
			expected: "match x {\n  1 => \"one\",\n}\n",
		},
		{
			name:     "match block arm adds trailing comma",
			input:    "match x {\n1 => {\nfoo();\n}\n}",
			expected: "match x {\n  1 => {\n    foo();\n  },\n}\n",
		},
		{
			name:     "match block arm trailing comma already present",
			input:    "match x {\n1 => {\nfoo();\n},\n}",
			expected: "match x {\n  1 => {\n    foo();\n  },\n}\n",
		},
		{
			name:     "enum adds trailing comma",
			input:    "enum Color {\nRed,\nGreen,\nBlue\n}",
			expected: "enum Color {\n  Red,\n  Green,\n  Blue,\n}\n",
		},
		{
			name:     "enum trailing comma already present",
			input:    "enum Color {\nRed,\nGreen,\n}",
			expected: "enum Color {\n  Red,\n  Green,\n}\n",
		},
		{
			name:     "enum with method no trailing comma after method",
			input:    "enum Shape {\nCircle,\nPoint,\narea() int {\nreturn 0;\n}\n}",
			expected: "enum Shape {\n  Circle,\n  Point,\n  area() int {\n    return 0;\n  }\n}\n",
		},
		{
			name:     "empty match no trailing comma",
			input:    "match x {\n}",
			expected: "match x {\n}\n",
		},
		{
			name:     "match trailing comma with comment before close",
			input:    "match x {\n1 => \"one\"\n// comment\n}",
			expected: "match x {\n  1 => \"one\", // comment\n}\n",
		},
		{
			name:     "match trailing comma with trailing comment",
			input:    "match x {\n1 => \"one\" // note\n}",
			expected: "match x {\n  1 => \"one\", // note\n}\n",
		},
		{
			name:     "nested match trailing comma",
			input:    "match x {\n1 => match y {\n2 => \"inner\"\n}\n}",
			expected: "match x {\n  1 => match y {\n    2 => \"inner\",\n  },\n}\n",
		},
		// --- Blank line preservation ---
		{
			name:     "blank line between top-level declarations",
			input:    "type Foo {\nint x;\n}\n\ntype Bar {\nint y;\n}",
			expected: "type Foo {\n  int x;\n}\n\ntype Bar {\n  int y;\n}\n",
		},
		{
			name:     "blank line between functions",
			input:    "foo() {\n}\n\nbar() {\n}",
			expected: "foo() {\n}\n\nbar() {\n}\n",
		},
		{
			name:     "no blank line between braces when source has none",
			input:    "foo() {\n}\nbar() {\n}",
			expected: "foo() {\n}\nbar() {\n}\n",
		},
		{
			name:     "max one blank line after brace",
			input:    "foo() {\n}\n\n\n\nbar() {\n}",
			expected: "foo() {\n}\n\nbar() {\n}\n",
		},
		// --- Real language patterns ---
		{
			name:     "error handler ? e block",
			input:    "x := parse(\"bad\")? e {\n42;\n};",
			expected: "x := parse(\"bad\")? e {\n  42;\n};\n",
		},
		{
			name:     "as! forced cast",
			input:    "x:=a as! int;",
			expected: "x := a as! int;\n",
		},
		{
			name:     "generic constraint",
			input:    "sort[T:Ordered](T[] vec) T[] {",
			expected: "sort[T: Ordered](T[] vec) T[] {\n",
		},
		{
			name:     "multiple constraints",
			input:    "Map[K:Hashable+Equal,V] m;",
			expected: "Map[K: Hashable + Equal, V] m;\n",
		},
		{
			name:     "get with fat arrow shorthand",
			input:    "get is_empty bool=>this.len==0;",
			expected: "get is_empty bool => this.len == 0;\n",
		},
		{
			name:     "void function type",
			input:    "(int)->void action;",
			expected: "(int) -> void action;\n",
		},
		{
			name:     "tuple destructuring",
			input:    "(a,b):=(10,20);",
			expected: "(a, b) := (10, 20);\n",
		},
		{
			name:     "for in with index",
			input:    "for i,n in numbers {\n}",
			expected: "for i, n in numbers {\n}\n",
		},
		{
			name:     "fixed size array type",
			input:    "int[3] arr=[1,2,3];",
			expected: "int[3] arr = [1, 2, 3];\n",
		},
		{
			name:     "empty lambda params",
			input:    "f:=||->42;",
			expected: "f := || -> 42;\n",
		},
		{
			name:     "move empty lambda",
			input:    "f:=move||->x;",
			expected: "f := move || -> x;\n",
		},
		{
			name:     "optional type",
			input:    "int? x=none;",
			expected: "int? x = none;\n",
		},
		{
			name:     "failable return type",
			input:    "parse() int!{",
			expected: "parse() int! {\n",
		},
		{
			name:     "chained methods with lambdas",
			input:    "arr.iter().filter(|n|->n>2).map(|n|->n*2).collect()",
			expected: "arr.iter().filter(|n| -> n > 2).map(|n| -> n * 2).collect()\n",
		},
		{
			name:     "generic enum with payload",
			input:    "enum Maybe[T] {\nSome(T value),\nNone,\n}",
			expected: "enum Maybe[T] {\n  Some(T value),\n  None,\n}\n",
		},
		{
			name:     "structural type annotation",
			input:    "type Iter[T] `structural `public {\nnext() T?;\n}",
			expected: "type Iter[T] `structural `public {\n  next() T?;\n}\n",
		},
		{
			name:     "test annotation with triple string",
			input:    "main() `test(expected: \"\"\"42\nhello\"\"\") {\nprint_line(42);\n}",
			expected: "main() `test(expected: \"\"\"42\nhello\"\"\") {\n  print_line(42);\n}\n",
		},
		{
			name:     "factory shorthand",
			input:    "from_nanos(int ns) Duration `factory=>Duration(nanos:ns);",
			expected: "from_nanos(int ns) Duration `factory => Duration(nanos: ns);\n",
		},
		{
			name:     "complex unary binary mix",
			input:    "x:=a*-b+~c&!d;",
			expected: "x := a * -b + ~c & !d;\n",
		},
		{
			name:     "nested generics",
			input:    "Map[string,Vector[int]] m;",
			expected: "Map[string, Vector[int]] m;\n",
		},
		{
			name:     "chained postfix operators",
			input:    "x ?. y ! . z ?;",
			expected: "x?.y!.z?;\n",
		},
		{
			name:     "deep nesting",
			input:    "a(){\nb(){\nc(){\nx();\n}\n}\n}",
			expected: "a() {\n  b() {\n    c() {\n      x();\n    }\n  }\n}\n",
		},
		{
			name:     "inline block comments",
			input:    "int /* type */ x = 1;",
			expected: "int /* type */ x = 1;\n",
		},
		{
			name:     "doc annotation",
			input:    "foo() `public `doc(\"Does stuff.\") {",
			expected: "foo() `public `doc(\"Does stuff.\") {\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(Format([]byte(tt.input)))
			if got != tt.expected {
				t.Errorf("Format(%q):\ngot:  %q\nwant: %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestIdempotent(t *testing.T) {
	inputs := []string{
		"main() {\n  int x = 1;\n  if x > 0 {\n    print_line(\"positive\");\n  }\n}\n",
		"type Foo `native `public {\n  get len int `native;\n\n  push(T elem) `native;\n}\n",
		"enum Color {\n  Red,\n  Green,\n  Blue,\n}\n",
		"// comment\nuse json;\n\nmain() {\n  int x = 1; // value\n}\n",
		"use io;\nuse json;\nuse path;\n",
		"for i := 0; i < 10; i++ {\n  arr[i] = i * 2;\n}\n",
		"return (a, b);\n",
		"if !(a && b) {\n  x();\n}\n",
		"f := |x| -> x * 2;\n",
		"assert('\\\\' == '\\\\', \"backslash\");\n",
		"select {\n  ch.send(v):\n    x();\n  default:\n    y();\n}\n",
		"match x {\n  1 => \"one\",\n  _ => \"other\",\n}\n",
		"string s = \"{match x { 3 => \"three\", _ => \"other\", }}\";\n",
		"string s = \"{a}{b}\";\n",
		"string s = \"{match c { 'x' => 1, _ => 0, }}\";\n",
		"string s = \"{fmt(\"{x}\")}\";\n",
		"string s = \"{x /* } */ + y}\";\n",
		"string s = \"{\"\"\"hello\"\"\"}\";\n",
		"string s = \"{r\"path\\\\end\"}\";\n",
	}

	for i, input := range inputs {
		first := string(Format([]byte(input)))
		second := string(Format([]byte(first)))
		if first != second {
			t.Errorf("input %d not idempotent:\nfirst:  %q\nsecond: %q", i, first, second)
		}
	}
}

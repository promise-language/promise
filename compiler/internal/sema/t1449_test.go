package sema

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1449: meta-annotation parameter contracts. Every annotation validates its
// full parameter list — unknown named parameters, excess/missing positionals,
// positional-after-named, duplicates, and wrong value forms are all errors.

// TestT1449SpecsExhaustive asserts every built-in annotation declares a
// parameter contract (and that the table has no entries for names that are not
// annotations). Adding an annotation without a contract fails here rather than
// silently accepting anything.
func TestT1449SpecsExhaustive(t *testing.T) {
	for name := range builtinMetas {
		if _, ok := metaParamSpecs[name]; !ok {
			t.Errorf("annotation `%s has no entry in metaParamSpecs", name)
		}
	}
	for name := range metaParamSpecs {
		if _, ok := builtinMetas[name]; !ok {
			t.Errorf("metaParamSpecs has entry for unknown annotation `%s", name)
		}
	}
}

func TestT1449BadParams(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		// --- positional after named ---
		{
			"test exclude then stray positional",
			"f() `test(exclude: wasm, macos) {}",
			"positional parameter must precede named parameters in `test",
		},
		{
			"embed named then positional",
			"get s string `embed(compress: true, \"a.txt\");",
			"positional parameter must precede named parameters in `embed",
		},

		// --- unknown named parameter ---
		{
			"test unknown named",
			"f() `test(bogus: 3) {}",
			"unknown parameter 'bogus' in `test; allowed: allow_leaks, exclude, expected, memory_limit, timeout",
		},
		{
			"doc named param",
			"f() `doc(\"x\", k: 3) {}",
			"unknown parameter 'k' in `doc; it takes no named parameters",
		},
		{
			"deprecated unknown named",
			"f() `deprecated(since: \"1.0\", bogus: true) {}",
			"unknown parameter 'bogus' in `deprecated; allowed: message, since",
		},
		{
			"serializable unknown named",
			"type T `serializable(nope: \"x\") { int x; }",
			"unknown parameter 'nope' in `serializable; allowed: tag",
		},

		// --- excess positional ---
		{
			"doc two positionals",
			"f() `doc(\"x\", \"y\") {}",
			"`doc takes at most 1 positional parameter (text)",
		},
		{
			"test positional",
			"f() `test(99) {}",
			"`test takes no positional parameters",
		},
		{
			"wasm_import three positionals",
			"f() `extern `wasm_import(\"a\", \"b\", \"c\") `target(wasm);",
			"`wasm_import takes at most 2 positional parameters (module, name)",
		},

		// --- missing required positional ---
		{
			"doc bare",
			"f() `doc {}",
			"`doc requires 1 positional parameter (text)",
		},
		{
			"key bare",
			"type T `serializable { int x `key; }",
			"`key requires 1 positional parameter (name)",
		},
		{
			"target bare",
			"f() `target {}",
			"`target requires 1 positional parameter (condition)",
		},
		{
			"align bare",
			"type T `align { int x; }",
			"`align requires 1 positional parameter (alignment)",
		},

		// --- wrong value form ---
		{
			"doc integer",
			"f() `doc(42) {}",
			"`doc parameter 'text' must be a string literal",
		},
		{
			"test allow_leaks string",
			"f() `test(allow_leaks: \"yes\") {}",
			"`test parameter 'allow_leaks' must be a boolean literal (true or false)",
		},
		{
			"align string",
			"type T `align(\"8\") { int x; }",
			"`align parameter 'alignment' must be an integer literal",
		},
		{
			"embed compress integer",
			"get s string `embed(\"a.txt\", compress: 1);",
			"`embed parameter 'compress' must be a boolean literal (true or false)",
		},
		{
			"extern integer",
			"f() `extern(7);",
			"`extern parameter 'symbol' must be a string literal",
		},
		{
			"deprecated since integer",
			"f() `deprecated(since: 1) {}",
			"`deprecated parameter 'since' must be a string literal",
		},

		// --- parameterless annotations ---
		{
			"copy with params",
			"type T `copy(1) { int x; }",
			"`copy takes no parameters",
		},
		{
			"inline with params",
			"f() `inline(fast: true) {}",
			"`inline takes no parameters",
		},

		// --- duplicates ---
		{
			"duplicate named",
			"f() `test(timeout: \"1s\", timeout: \"2s\") {}",
			"duplicate annotation parameter 'timeout' in `test",
		},
		{
			"deprecated positional and named message",
			"f() `deprecated(\"gone\", message: \"gone\") {}",
			"parameter 'message' in `deprecated is given both positionally and by name",
		},

		// --- condition expressions ---
		{
			"target unknown ident",
			"f() `target(sparc64) {}",
			"unknown target identifier \"sparc64\"",
		},
		{
			"target string",
			"f() `target(\"wasm32\") {}",
			"`target condition must be an identifier, not a string literal",
		},
		{
			"target integer",
			"f() `target(1) {}",
			"invalid `target condition; expected identifier, !, ||, or &&",
		},
		{
			"exclude and-chain",
			"f() `test(exclude: linux && macos) {}",
			"exclude expression must use || to combine target identifiers",
		},
		{
			"exclude negation",
			"f() `test(exclude: !linux) {}",
			"invalid exclude expression; expected identifier or identifier || identifier",
		},
		{
			"exclude unknown ident",
			"f() `test(exclude: sparc64) {}",
			"unknown exclude target \"sparc64\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkErrs(t, tc.src)
			expectError(t, errs, tc.want)
		})
	}
}

// TestT1449GoodParams covers every annotation spelling used across the repo.
func TestT1449GoodParams(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"doc", "f() `doc(\"a function\") {}"},
		{"test bare", "f() `test { assert(true, \"ok\"); }"},
		{"test all named", "f() `test(exclude: wasm || windows, timeout: \"5s\", memory_limit: \"256MB\", allow_leaks: false) {}"},
		{"test expected", "main() `test(expected: \"hi\") { print_line(\"hi\"); }"},
		{"deprecated bare", "f() `deprecated {}"},
		{"deprecated positional", "f() `deprecated(\"use g instead\") {}"},
		{"deprecated named", "f() `deprecated(since: \"1.3\", message: \"use g instead\") {}"},
		{"extern bare", "f() `extern;"},
		{"extern named symbol", "f() `extern(\"promise_sqrt\");"},
		{"target simple", "f() `target(linux) {}"},
		{"target not", "f() `target(!windows) {}"},
		{"target and-or", "f() `target((linux || macos) && x86_64) {}"},
		{"embed", "get s string `embed(\"a.txt\");"},
		{"embed compress", "get s string `embed(\"a.txt\", compress: true);"},
		{"key", "type T `serializable { int x `key(\"user_name\"); }"},
		{"serializable tag", "type T `serializable(tag: \"kind\") { int x; }"},
		{"lifetime", "f(string a `lifetime(x)) string& `lifetime(x) { return a; }"},
		{"wasm_import", "f() `extern `wasm_import(\"promise_env\", \"log\") `target(wasm);"},
		{"align", "type T `align(8) { int x; }"},
		{"packed", "type T `packed { int x; }"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkOK(t, tc.src)
		})
	}
}

// deprecationOf returns the recorded deprecation message for a top-level type.
func deprecationOf(t *testing.T, src string) string {
	t.Helper()
	info := checkOK(t, src)
	scope := info.Scopes[findFile(t, info)]
	return scope.Lookup("Old").(*types.TypeName).Type().(*types.Named).Deprecated()
}

// TestT1449DeprecatedNamedMessage is the regression for the positional read of
// `deprecated(since: …): the version string was reported as the message.
func TestT1449DeprecatedNamedMessage(t *testing.T) {
	got := deprecationOf(t, "type Old `deprecated(since: \"1.0\", message: \"use New\") {}")
	if got != "use New" {
		t.Errorf("deprecation message = %q, want %q", got, "use New")
	}
}

// TestT1449DeprecatedSinceOnlyHasNoMessage checks that `deprecated(since: …)
// with no message still marks the entity deprecated (sentinel " ").
func TestT1449DeprecatedSinceOnlyHasNoMessage(t *testing.T) {
	if got := deprecationOf(t, "type Old `deprecated(since: \"1.0\") {}"); got != " " {
		t.Errorf("deprecation message = %q, want the no-message sentinel", got)
	}
}

// TestT1449BadTargetOnFilteredDecl covers the annotation whose typo is the most
// destructive: an unknown identifier in `target(cond) evaluates false, so the
// declaration is filtered out of *every* build. Filtered declarations skip
// validateMetas entirely, so the condition must also be validated on the
// filtering path — otherwise the declaration vanishes with no diagnostic.
func TestT1449BadTargetOnFilteredDecl(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"func", "f() `target(sparc64) {}"},
		{"type", "type T `target(sparc64) { int x; }"},
		{"enum", "enum E `target(sparc64) { A; B; }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := checkSourceWithTarget(t, tc.src, "x86_64-unknown-linux-musl")
			expectError(t, errs, "unknown target identifier \"sparc64\"")
		})
	}
}

// TestT1449GoodTargetOnFilteredDeclReportedOnce checks the filtering path does
// not double-report a condition that is well formed but simply does not match.
func TestT1449GoodTargetOnFilteredDeclReportedOnce(t *testing.T) {
	_, errs := checkSourceWithTarget(t, "f() `target(windows) {}", "x86_64-unknown-linux-musl")
	if len(errs) != 0 {
		t.Errorf("well-formed non-matching `target must be silent, got %v", errs)
	}
	// A matching declaration is validated by validateMetas — exactly once.
	_, errs = checkSourceWithTarget(t, "f() `target(linux || sparc64) {}", "x86_64-unknown-linux-musl")
	if len(errs) != 1 {
		t.Errorf("expected exactly one diagnostic for a matching bad condition, got %v", errs)
	}
}

// TestT1449UnknownAnnotationStillReported confirms the parameter validator does
// not swallow the pre-existing unknown-name diagnostic.
func TestT1449UnknownAnnotationStillReported(t *testing.T) {
	errs := checkErrs(t, "f() `totally_bogus(1, k: 2) {}")
	expectError(t, errs, "unknown meta annotation `totally_bogus")
	for _, e := range errs {
		if strings.Contains(e.Error(), "takes no parameters") {
			t.Errorf("unknown annotation should not also report a parameter contract: %v", e)
		}
	}
}

// TestT1449ValidatedAtEveryAnnotationPosition pins that the contract validator
// runs from every validateMetas call site — a bad parameter must be rejected on
// a type, enum, variant, field, method, parameter, function, and global alike,
// not just on the free functions the other tables happen to use.
func TestT1449ValidatedAtEveryAnnotationPosition(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"type", "type T `doc(1) { int x; }"},
		{"enum", "enum E `doc(1) { A; B; }"},
		{"variant", "enum E { A `doc(1), B }"},
		{"field", "type T { int x `doc(1); }"},
		{"method", "type T { int x; m() `doc(1) {} }"},
		{"param", "f(int x `doc(1)) {}"},
		{"func", "f() `doc(1) {}"},
		{"global", "get s string `doc(1) { return \"x\"; }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectError(t, checkErrs(t, tc.src), "`doc parameter 'text' must be a string literal")
		})
	}
}

// --- exclude condition: the T1415 regression class ---

// TestT1449ExcludeIdentsCollected is the direct guard for the defect that red-lit
// the commit gate: a target the author listed must reach Info.TestExcludes, never
// be silently dropped. `test(exclude: a, b) is now a compile error (covered in
// TestT1449BadParams); every legal spelling must round-trip in full.
func TestT1449ExcludeIdentsCollected(t *testing.T) {
	for _, tc := range []struct {
		name string
		cond string
		want []string
	}{
		{"single", "wasm", []string{"wasm"}},
		{"or chain", "macos || linux || windows || wasm", []string{"macos", "linux", "windows", "wasm"}},
		{"parenthesized", "(linux || macos)", []string{"linux", "macos"}},
		{"nested parens", "linux || (macos || wasm)", []string{"linux", "macos", "wasm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := checkOK(t, "t_f() `test(exclude: "+tc.cond+") { assert(true, \"ok\"); }")
			got := info.TestExcludes["t_f"]
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("exclude targets = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestT1449DuplicateExclude covers a duplicate on the parameter whose silent loss
// caused T1415 — the second spelling must be reported, not quietly overwrite.
func TestT1449DuplicateExclude(t *testing.T) {
	errs := checkErrs(t, "t_f() `test(exclude: wasm, exclude: linux) {}")
	expectError(t, errs, "duplicate annotation parameter 'exclude' in `test")
}

// --- target conditions: evaluation, not just validation ---

// TestT1449TargetConditionEvaluated checks that the conditions the validator now
// accepts are also *evaluated* correctly — in particular `&&` and parenthesized
// grouping. Parentheses previously fell through evalTargetExpr's default arm and
// returned true, so `target((linux || macos) && x86_64) kept the declaration on
// every platform, including Windows.
func TestT1449TargetConditionEvaluated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cond    string
		triple  string
		present bool
	}{
		{"paren and arch match", "(linux || macos) && x86_64", "x86_64-unknown-linux-musl", true},
		{"paren and arch mismatch", "(linux || macos) && x86_64", "aarch64-apple-macosx14.0.0", false},
		{"paren and os mismatch", "(linux || macos) && x86_64", "x86_64-pc-windows-msvc", false},
		{"and both false", "windows && aarch64", "x86_64-unknown-linux-musl", false},
		{"not over parens", "!(windows)", "x86_64-unknown-linux-musl", true},
		{"not over parens excluded", "!(windows)", "x86_64-pc-windows-msvc", false},
		{"redundant parens", "((linux))", "x86_64-unknown-linux-musl", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "thing() string `target(" + tc.cond + ") { return \"x\"; }\nmain() { thing(); }"
			_, errs := checkSourceWithTarget(t, src, tc.triple)
			if tc.present && len(errs) != 0 {
				t.Fatalf("`target(%s) on %s: declaration must be kept, got %v", tc.cond, tc.triple, errs)
			}
			if !tc.present {
				expectError(t, errs, "thing")
			}
		})
	}
}

// TestT1449TargetNonBooleanBinaryOp covers a binary operator that is neither ||
// nor && — grammatical, meaningless as a target condition, and previously
// accepted (then evaluated as "include everywhere" by evalTargetExpr's default).
func TestT1449TargetNonBooleanBinaryOp(t *testing.T) {
	errs := checkErrs(t, "f() `target(linux == macos) {}")
	expectError(t, errs, "invalid `target condition; expected identifier, !, ||, or &&")
}

// TestT1449FilteredDeclSkipsNonTargetParams pins the documented trade-off in
// validateMetas: only the `target annotation is validated on the filtering path,
// so a malformed *other* annotation on an excluded declaration surfaces on the
// targets that actually compile it. If this ever changes, the note above
// validateMetas must change with it.
func TestT1449FilteredDeclSkipsNonTargetParams(t *testing.T) {
	src := "f() `doc(1) `target(windows) {}"
	if _, errs := checkSourceWithTarget(t, src, "x86_64-unknown-linux-musl"); len(errs) != 0 {
		t.Errorf("filtered declaration must not be parameter-checked, got %v", errs)
	}
	_, errs := checkSourceWithTarget(t, src, "x86_64-pc-windows-msvc")
	expectError(t, errs, "`doc parameter 'text' must be a string literal")
}

// --- wasm_import ---

// TestT1449WasmImportWasmMention covers the condition forms exprMentionsWasm has
// to see through when deciding whether `wasm_import is reachable: parentheses
// (newly unwrapped) and `!`, which mentions no wasm target and must still warn.
func TestT1449WasmImportWasmMention(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cond     string
		wantWarn bool
	}{
		{"parenthesized or", "(wasi || web)", false},
		{"redundant parens", "((wasm))", false},
		{"negation mentions nothing", "!windows", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "_fd_write(int fd) int `extern(\"fd_write\") `wasm_import(\"wasi\", \"fd_write\") `target(" + tc.cond + ");\nmain() {}"
			_, errs := checkSourceWithTarget(t, src, "wasm32-wasi")
			warned := false
			for _, e := range errs {
				if strings.Contains(e.Error(), "will be ignored on non-WASM targets") {
					warned = true
				}
			}
			if warned != tc.wantWarn {
				t.Errorf("`target(%s): warned = %v, want %v (errs: %v)", tc.cond, warned, tc.wantWarn, errs)
			}
		})
	}
}

// --- remaining contract edge cases ---

// TestT1449EmbedMissingPath covers a required positional omitted while a named
// parameter is present — the arity check must not be satisfied by named params.
func TestT1449EmbedMissingPath(t *testing.T) {
	errs := checkErrs(t, "get s string `embed(compress: true);")
	expectError(t, errs, "`embed requires 1 positional parameter (path)")
}

// TestT1449ExcessPositionalThenNamedSameSlot exercises the interaction of the two
// positional checks: the count runs past the declared slots and the same slot is
// then named, so the "given both positionally and by name" check must clamp
// rather than index out of range.
func TestT1449ExcessPositionalThenNamedSameSlot(t *testing.T) {
	errs := checkErrs(t, "f() `deprecated(\"a\", \"b\", message: \"c\") {}")
	expectError(t, errs, "`deprecated takes at most 1 positional parameter (message)")
	expectError(t, errs, "parameter 'message' in `deprecated is given both positionally and by name")
}

// TestT1449ValueKindDescribe covers the diagnostic wording for every value kind.
// The condition kinds report their own errors from validateCondExpr and so never
// reach describe() through checkMetaValue; asserting the text here keeps the
// switch total if that ever changes.
func TestT1449ValueKindDescribe(t *testing.T) {
	for _, tc := range []struct {
		kind metaValueKind
		want string
	}{
		{valString, "a string literal"},
		{valBool, "a boolean literal (true or false)"},
		{valInt, "an integer literal"},
		{valIdent, "an identifier"},
		{valTargetCond, "a target condition"},
		{valExcludeCond, "a target condition"},
	} {
		if got := tc.kind.describe(); got != tc.want {
			t.Errorf("kind %d describe() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

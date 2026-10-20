package regress11

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// #264: LLVM textual IR gives a function's value names and block labels one
// namespace. Every coroutine-shaped function used to label its body and cleanup
// blocks `body` and `cleanup` — both valid Promise identifiers — so a user local
// or parameter of either name was emitted as `%body` beside a `body:` label, and
// `opt` rejected the module ("'%body' is not a basic block" / "unable to create
// block named 'body'"). The labels are now `coro.body` / `coro.cleanup`, which
// no user-derived name can spell.
//
// Each case below compiles user code INTO a coroutine — the kinds that host
// user names — and checks the emitted function's labels against its values.

func TestIssue264_CoroutineLabelsDisjointFromUserNames(t *testing.T) {
	cases := []struct {
		name    string
		extract func(t *testing.T) string
	}{
		{
			// The issue's own shape: main's body is compiled into .goroutine.main.
			name: "main_goroutine",
			extract: func(t *testing.T) string {
				ir := codegentest.GenerateIR(t, `
					type Doc { string? body; }
					main() {
						Doc doc = Doc(body: "hi");
						if body := doc.body {
							print_line(body);
						}
						int cleanup = 2;
						print_line(cleanup);
					}
				`)
				return codegentest.ExtractDefine(ir, ".goroutine.main")
			},
		},
		{
			name: "go_block",
			extract: func(t *testing.T) string {
				ir := codegentest.GenerateIR(t, `
					spawn() {
						t := go {
							int body = 5;
							int cleanup = body + 1;
							assert(cleanup == 6, "go");
						};
						<-t;
					}
					main() { spawn(); }
				`)
				return codegentest.ExtractGoroutineCoro(t, ir)
			},
		},
		{
			// A callee that is not a bare identifier (here explicit type args)
			// sends `go f(...)` through genGoCallExprViaBlock, which compiles the
			// arguments inside the coroutine — so an error-handler binding in an
			// argument becomes one of its locals.
			name: "go_call_via_block",
			extract: func(t *testing.T) string {
				ir := codegentest.GenerateIR(t, `
					second[T](T a, T b) T { return b; }
					maybe!(bool fail) int {
						if fail { raise error("fail"); }
						return 1;
					}
					spawn() {
						t := go second[int](maybe(true)? body { -1; }, maybe(true)? cleanup { -2; });
						int r = <-t;
						print_line(r);
					}
					main() { spawn(); }
				`)
				return codegentest.ExtractGoroutineCoro(t, ir)
			},
		},
		{
			// Both forms: the generator coroutine's parameters carry the user's
			// parameter names, and its locals are named from the user's locals.
			name: "generator",
			extract: func(t *testing.T) string {
				ir := codegentest.GenerateIR(t, `
					gen(int body) stream[int] {
						int cleanup = body + 1;
						yield cleanup;
					}
					main() {
						for x in gen(3) { print_line(x); }
					}
				`)
				return codegentest.ExtractGeneratorBody(t, ir, "%cleanup = alloca")
			},
		},
		{
			// WASM only: a `test` body is compiled into .test_coro.<name>.
			name: "wasm_test_coroutine",
			extract: func(t *testing.T) string {
				ir := generateWasmTestMainIR(t, `
					check() `+"`test"+` {
						int body = 1;
						int cleanup = body + 1;
						assert(cleanup == 2, "test");
					}
				`)
				return codegentest.ExtractDefine(ir, ".test_coro.check")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := tc.extract(t)
			if fn == "" {
				t.Fatalf("coroutine definition not found")
			}
			values, labels := issue264Names(fn)
			// Premise: the user names really are emitted as value names, so the
			// test cannot pass because a local got renamed instead.
			for _, want := range []string{"body", "cleanup"} {
				if !values[want] {
					t.Errorf("expected a value named %%%s in the coroutine:\n%s", want, fn)
				}
			}
			for name := range labels {
				if values[name] {
					t.Errorf("block label %q is also a value name %%%s — opt rejects the module:\n%s", name, name, fn)
				}
			}
			// Shape: this is the coroutine whose body/cleanup labels moved.
			for _, want := range []string{"coro.body", "coro.cleanup"} {
				if !labels[want] {
					t.Errorf("expected a %s: block in the coroutine:\n%s", want, fn)
				}
			}
		})
	}
}

var (
	issue264Value = regexp.MustCompile(`^\s+%([-A-Za-z$._0-9]+|"[^"]*") = `)
	issue264Label = regexp.MustCompile(`^([-A-Za-z$._0-9]+|"[^"]*"):`)
)

// issue264Names returns the local value names (parameters and instruction
// results) and the block labels of one `define`, without the `%` sigil.
func issue264Names(fn string) (values, labels map[string]bool) {
	values, labels = map[string]bool{}, map[string]bool{}
	lines := strings.Split(fn, "\n")
	for _, p := range issue264Params(lines[0]) {
		values[p] = true
	}
	for _, ln := range lines[1:] {
		if m := issue264Value.FindStringSubmatch(ln); m != nil {
			values[m[1]] = true
		} else if m := issue264Label.FindStringSubmatch(ln); m != nil {
			labels[m[1]] = true
		}
	}
	return values, labels
}

// issue264Params returns the parameter names of a `define` header: the last
// field of each top-level comma-separated parameter (types such as `{ i8*, i8* }`
// carry their own commas, so the split tracks bracket depth).
func issue264Params(header string) []string {
	open := strings.Index(header, "(")
	if open < 0 {
		return nil
	}
	var names []string
	depth, start := 0, open+1
	for i := open + 1; i < len(header); i++ {
		switch header[i] {
		case '{', '[', '<', '(':
			depth++
		case '}', ']', '>':
			depth--
		case ')', ',':
			if header[i] == ')' && depth > 0 {
				depth--
				continue
			}
			if depth > 0 {
				continue
			}
			fields := strings.Fields(header[start:i])
			if n := len(fields); n > 0 && strings.HasPrefix(fields[n-1], "%") {
				names = append(names, strings.TrimPrefix(fields[n-1], "%"))
			}
			if header[i] == ')' {
				return names
			}
			start = i + 1
		}
	}
	return names
}

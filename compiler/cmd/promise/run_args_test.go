package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunForwardsProgramArgs is the end-to-end guard for T1426: everything after
// a bare `--` reaches the compiled program as its os.args, verbatim — including a
// forwarded `--flag` (which normalizeArgs must NOT rewrite) and a forwarded `-h`
// (which handleHelp must NOT hijack into printing help). This exercises the whole
// path — normalize → parseRunArgs → exec — not just the parser.
func TestRunForwardsProgramArgs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := locatePromiseBin(t)

	dir := t.TempDir()
	// A program that prints each os.args entry on its own line, so stdout is an
	// exact, order-preserving transcript of the forwarded argv.
	src := "use os;\n" +
		"main() {\n" +
		"  args := os.args;\n" +
		"  i := 0;\n" +
		"  while i < args.len {\n" +
		"    print_line(args[i]);\n" +
		"    i = i + 1;\n" +
		"  }\n" +
		"}\n"
	probe := filepath.Join(dir, "argsprobe.pr")
	if err := os.WriteFile(probe, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		tail []string
		want []string
	}{
		{name: "no args", tail: nil, want: nil},
		{name: "positional args", tail: []string{"one", "two"}, want: []string{"one", "two"}},
		{
			name: "flags survive normalization",
			tail: []string{"one", "two", "--flag", "-o=x"},
			want: []string{"one", "two", "--flag", "-o=x"},
		},
		{
			name: "help flags forwarded not hijacked",
			tail: []string{"-h", "-help", "--help"},
			want: []string{"-h", "-help", "--help"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv := []string{"run", probe}
			if tc.tail != nil {
				argv = append(argv, "--")
				argv = append(argv, tc.tail...)
			}
			out, err := exec.Command(bin, argv...).CombinedOutput()
			if err != nil {
				t.Fatalf("promise run failed: %v\n%s", err, out)
			}
			var got []string
			if s := strings.TrimSpace(string(out)); s != "" {
				got = strings.Split(s, "\n")
			}
			if !slicesEqual(got, tc.want) {
				t.Errorf("forwarded argv = %#v, want %#v (raw output %q)", got, tc.want, out)
			}
		})
	}
}

// TestRunForwardsFilenameLikeArgOnCacheMiss is the direct regression guard for
// the original T1426 bug: `promise run app.pr -- other.pr` errored with
// "error reading other.pr" because buildToFile treated the post-`--` token as a
// second source file. The fix is buildToFile's own `--` break, which is ONLY
// exercised on a compile (cache miss) — the shared TestRunForwardsProgramArgs
// cache-hits every case after its first, so it never reaches buildToFile with a
// `--`. A fresh (uniquely named) source guarantees the miss, and a
// filename-shaped tail token (`ghost.pr`, which does not exist) proves the tail
// is forwarded as argv rather than opened as source.
func TestRunForwardsFilenameLikeArgOnCacheMiss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := locatePromiseBin(t)

	dir := t.TempDir()
	src := "use os;\n" +
		"main() {\n" +
		"  args := os.args;\n" +
		"  i := 0;\n" +
		"  while i < args.len {\n" +
		"    print_line(args[i]);\n" +
		"    i = i + 1;\n" +
		"  }\n" +
		"}\n"
	// A one-shot, uniquely named source so this run is a guaranteed cache miss
	// and buildToFile actually parses the `--` tail.
	probe := filepath.Join(dir, "cache_miss_probe.pr")
	if err := os.WriteFile(probe, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	// `ghost.pr` looks like a source file but does not exist: pre-fix it would be
	// opened and fail; post-fix it is forwarded verbatim into os.args.
	out, err := exec.Command(bin, "run", probe, "--", "ghost.pr", "extra").CombinedOutput()
	if err != nil {
		t.Fatalf("promise run failed (buildToFile likely misread the tail as source): %v\n%s", err, out)
	}
	var got []string
	if s := strings.TrimSpace(string(out)); s != "" {
		got = strings.Split(s, "\n")
	}
	want := []string{"ghost.pr", "extra"}
	if !slicesEqual(got, want) {
		t.Errorf("forwarded argv = %#v, want %#v (raw output %q)", got, want, out)
	}
}

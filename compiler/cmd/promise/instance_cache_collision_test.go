package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// T1115: the per-instance build cache must fold in the DeclHashes of the user
// types reachable from a generic instance's type arguments. Two programs that
// define a type with the SAME NAME but a DIFFERENT body, and both instantiate a
// container over it (e.g. Map[int, CollideName]), must not collide in a shared
// build cache. Before the fix, the first program's cached container bitcode —
// referencing e.g. CollideName.drop — was reused by the second program (which
// never emits that symbol) → "undefined symbol: CollideName.drop" at link, or
// silent wrong-code.
//
// This drives the real binary against a shared (temp) PROMISE_HOME so both
// programs share one build cache, proving the collision is gone.
func TestInstanceCacheCollisionAcrossPrograms(t *testing.T) {
	promiseBin := locatePromiseBin(t)
	absBin, err := filepath.Abs(promiseBin)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	cases := []struct {
		name  string
		progA string
		wantA string
		progB string
		wantB string
	}{
		{
			name: "direct enum element",
			progA: `enum CollideName { S(string s, int n) }
main() {
  m := Map[int, CollideName]();
  m[1] = CollideName.S("hi", 2);
  h := m[1]!;
  match h { CollideName.S(s, n) => { print_line(s); } }
}`,
			wantA: "hi",
			progB: `enum CollideName { S(int n) }
main() {
  m := Map[int, CollideName]();
  m[1] = CollideName.S(5);
  h := m[1]!;
  match h { CollideName.S(n) => { print_line(n.to_string()); } }
}`,
			wantB: "5",
		},
		{
			name: "transitive field element",
			progA: `type Inner { string label; int extra; }
type Wrap { Inner c; }
main() {
  m := Map[int, Wrap]();
  m[1] = Wrap(c: Inner(label: "hi", extra: 9));
  h := m[1]!;
  print_line(h.c.label);
}`,
			wantA: "hi",
			progB: `type Inner { int label; }
type Wrap { Inner c; }
main() {
  m := Map[int, Wrap]();
  m[1] = Wrap(c: Inner(label: 42));
  h := m[1]!;
  print_line(h.c.label.to_string());
}`,
			wantB: "42",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// One shared PROMISE_HOME so both programs hit the same build cache.
			home := t.TempDir()
			run := func(prog, label string) (string, error) {
				dir := t.TempDir()
				src := filepath.Join(dir, "prog.pr")
				if err := os.WriteFile(src, []byte(prog), 0644); err != nil {
					t.Fatalf("write %s: %v", label, err)
				}
				cmd := exec.Command(absBin, "run", src)
				cmd.Env = append(os.Environ(), "PROMISE_HOME="+home)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}

			// Program A populates the cache for the container instance.
			outA, errA := run(tc.progA, "progA")
			if errA != nil {
				t.Fatalf("progA failed: %v\n%s", errA, outA)
			}
			if got := strings.TrimSpace(outA); got != tc.wantA {
				t.Fatalf("progA output = %q, want %q", got, tc.wantA)
			}

			// Program B, same container instance name but different element body,
			// must NOT reuse A's cached bitcode.
			outB, errB := run(tc.progB, "progB")
			if errB != nil {
				t.Fatalf("progB failed (cache collision regression): %v\n%s", errB, outB)
			}
			if got := strings.TrimSpace(outB); got != tc.wantB {
				t.Fatalf("progB output = %q, want %q\n%s", got, tc.wantB, outB)
			}
		})
	}
}

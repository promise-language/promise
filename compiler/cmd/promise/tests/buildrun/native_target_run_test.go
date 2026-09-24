package buildrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// T2206: `promise run -target <this host's own triple>` must build and RUN,
// exactly as an omitted -target does. Naming a target explicitly says which
// machine the binary is for, not that it is for someone else's.
//
// The case matters because the same triple means different things on different
// hosts: x86_64-pc-windows-msvc is a cross target on Linux and macOS, where
// `run` refuses to execute what it built, and the host's own on windows-amd64,
// where it must not. Asserting only the refusal — which is what
// windows_cross_link_test.go did — left the native side stated nowhere, so a
// host that took it produced a red suite for behaving correctly. This test has
// no platform branch: every host runs it against its own triple, and on
// windows-amd64 that triple IS the one the refusal test skips.
const nativeRunSource = `main() {
  print_line("native target ran");
}
`

// hostTriple asks the compiler which triple it calls this host's own, rather
// than reconstructing it from runtime.GOOS/GOARCH — macOS spells its host
// triple with a version suffix (arm64-apple-macosx26.0.0) that no table here
// can enumerate, and -target must be handed the exact spelling
// codegen.HostTargetTriple() produces.
func hostTriple(t *testing.T, bin string) string {
	t.Helper()
	res := clitest.Run(t, bin, nil, "targets", "-json")
	if res.ExitCode != 0 {
		t.Fatalf("targets -json failed (want exit 0)%s", res.Detail())
	}
	var out struct {
		Host    string `json:"host"`
		Targets []struct {
			Triple string `json:"triple"`
			Native bool   `json:"native"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		t.Fatalf("targets -json is not valid JSON: %v%s", err, res.Detail())
	}
	if out.Host == "" {
		t.Fatalf("targets -json reported no host triple%s", res.Detail())
	}
	// The host must also be listed, and listed as native. Without that row the
	// flag gate would reject its own host triple, and the run below would fail
	// for a reason that has nothing to do with execution.
	for _, spec := range out.Targets {
		if spec.Triple == out.Host {
			if !spec.Native {
				t.Errorf("targets -json lists the host triple %q without the native marker%s",
					out.Host, res.Detail())
			}
			return out.Host
		}
	}
	t.Fatalf("targets -json reports host %q but does not list it as a target%s", out.Host, res.Detail())
	return ""
}

func TestRunWithNativeTripleExecutes(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build+run integration test in short mode")
	}
	bin := clitest.Bin(t)
	triple := hostTriple(t, bin)

	dir := clitest.TempDir(t)
	srcPath := filepath.Join(dir, "prog.pr")
	if err := os.WriteFile(srcPath, []byte(nativeRunSource), 0644); err != nil {
		t.Fatal(err)
	}

	res := clitest.Run(t, bin, nil, "run", "-target", triple, srcPath)
	if res.ExitCode != 0 {
		t.Fatalf("run -target %s (this host's own triple) failed; want exit 0%s", triple, res.Detail())
	}
	if !strings.Contains(res.Stdout, "native target ran") {
		t.Errorf("run -target %s exited 0 but the program produced no output%s", triple, res.Detail())
	}
	// And it was not refused-then-reported-as-fine: the cross-execution message
	// must be absent, since this target is not cross.
	if strings.Contains(res.Combined(), "cross-target execution is not supported") {
		t.Errorf("run -target %s reported the host's own triple as a cross target%s", triple, res.Detail())
	}
}

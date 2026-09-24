package testrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// TestTestBinaryWarmupContractHolds guards the warm-up contract end to end
// (T1815): a freshly built batch test binary must exit immediately, silently
// and successfully when passed the warm-up flag, so the runner can pay
// macOS's first-exec code-signature scan in the compile phase instead of on the
// test budget.
//
// The failure this exists to catch is the silent one. A binary that stops
// honouring the flag — anyone editing codegen.GenerateTestMain — runs the
// whole suite during the warm-up and still exits 0. Nothing would look wrong
// except that the scan is charged to the budget again, resurfacing as
// intermittent "N of M tests did not report" timeouts on rebuild runs, which is
// a day of tracing to get back to here. warmTestBinary prints a warning on
// every way the contract can break; this asserts none of them fired.
//
// The unique tag defeats the per-source test-binary cache, so this is a real
// compile and the warm-up actually runs. On non-darwin hosts warmTestBinary
// returns immediately and the assertions hold trivially.
func TestTestBinaryWarmupContractHolds(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	unique := fmt.Sprintf("warmup-contract-%d-%d", os.Getpid(), time.Now().UnixNano())
	src := "// " + unique + "\n" +
		"warm_ok() `test {\n" +
		"  assert(1 == 1, \"ran once\");\n" +
		"}\n"
	file := filepath.Join(dir, "warm_test.pr")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	r := clitest.Run(t, promiseBin, nil, "test", file)
	combined := r.Combined()
	if r.ExitCode != 0 {
		t.Fatalf("expected the test file to pass:%s", r.Detail())
	}

	// warmTestBinary's warnings all carry this prefix.
	if strings.Contains(combined, "test binary warm-up broken") {
		t.Errorf("the warm-up contract is broken — see codegen.GenerateTestMain.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "1 passed, 0 failed") {
		t.Errorf("expected the test to run exactly once.\nOutput:\n%s", combined)
	}
}

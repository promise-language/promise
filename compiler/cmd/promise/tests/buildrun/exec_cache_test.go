package buildrun

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// TestExecCacheHitEndToEnd drives the real `promise exec` binary twice over the
// same source (T0857): the first invocation must compile and report a cache MISS,
// the second must skip compilation and report a cache HIT. Both must produce the
// program's stdout. This is the only path that exercises runExec + the cache
// save/lookup + executeExecBinary end-to-end. A per-run nonce in a leading comment
// guarantees the first run is a genuine miss regardless of prior cache state.
func TestExecCacheHitEndToEnd(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	nonce := time.Now().UnixNano()
	const marker = "exec-cache-e2e-ok"
	src := fmt.Sprintf("// T0857-exec-cache-e2e-%d\nprint_line(\"%s\");", nonce, marker)

	runOnce := func() (string, string) {
		t.Helper()
		cmd := exec.Command(promiseBin, "exec", src)
		cmd.Env = append(os.Environ(), "PROMISE_CACHE_DEBUG=1")
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("promise exec failed: %v\nstderr:\n%s", err, stderr.String())
		}
		return stdout.String(), stderr.String()
	}

	// First run: cache MISS, program output present.
	out1, err1 := runOnce()
	if !strings.Contains(out1, marker) {
		t.Errorf("first run stdout missing %q:\n%s", marker, out1)
	}
	if !strings.Contains(err1, "[cache MISS] <exec>") {
		t.Errorf("first run should report a cache MISS, got stderr:\n%s", err1)
	}

	// Second run: cache HIT (no recompile), same program output.
	out2, err2 := runOnce()
	if !strings.Contains(out2, marker) {
		t.Errorf("second run stdout missing %q:\n%s", marker, out2)
	}
	if !strings.Contains(err2, "[cache HIT] <exec>") {
		t.Errorf("second run should report a cache HIT, got stderr:\n%s", err2)
	}
}

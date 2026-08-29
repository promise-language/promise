package pkgmgr

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"github.com/promise-language/promise/compiler/internal/module"
)

// TestCheckUpgradeWithDeps drives `promise package check-upgrade <E>` end-to-end through
// the real compiler binary (so epochCompilerBin resolves to a true compiler): a
// project whose only dep has a verified E-compatible version reports all-clear; a
// project whose dep is broken hits the §9.10 gate and exits non-zero.
func TestCheckUpgradeWithDeps(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := clitest.Bin(t)
	epoch := clitest.CompilerEpoch(t)
	home := t.TempDir()

	makeDep := func(good bool) (url, commit string) {
		work := clitest.MakeWorkRepo(t)
		clitest.WriteModule(t, work, "dep", "2026.0", good)
		clitest.GitRun(t, work, "add", ".")
		clitest.GitRun(t, work, "commit", "-m", "init")
		clitest.GitRun(t, work, "tag", "epoch-"+epoch)
		return work, clitest.GitRun(t, work, "rev-parse", "HEAD")
	}

	runCheck := func(t *testing.T, depURL, depCommit string) (string, error) {
		proj := t.TempDir()
		toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + depURL + "\" = \"" + depCommit + "\"\n"
		os.WriteFile(filepath.Join(proj, "promise.toml"), []byte(toml), 0644)
		cmd := exec.Command(bin, "package", "check-upgrade", epoch)
		cmd.Dir = proj
		cmd.Env = append(os.Environ(), "PROMISE_HOME="+home, "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("compatible", func(t *testing.T) {
		url, commit := makeDep(true)
		out, err := runCheck(t, url, commit)
		if err != nil {
			t.Fatalf("check-upgrade should succeed for a compatible dep: %v\n%s", err, out)
		}
		if !strings.Contains(out, "compatible with epoch "+epoch) {
			t.Errorf("expected all-compatible report, got:\n%s", out)
		}
		// Verify the success message uses the canonical 'promise package update'
		// (not the deprecated 'promise pkg update') — T1033/T1007.
		if !strings.Contains(out, "promise package update") {
			t.Errorf("expected 'promise package update' in upgrade hint, got:\n%s", out)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		url, commit := makeDep(false)
		out, err := runCheck(t, url, commit)
		if err == nil {
			t.Fatalf("check-upgrade should exit non-zero for a broken dep:\n%s", out)
		}
		if !strings.Contains(out, "no compatible version") {
			t.Errorf("expected a §9.10 gate report, got:\n%s", out)
		}
	})

	// T1051: a dep whose only tag targets a NEWER epoch than the project is
	// versioned-but-incompatible — check-upgrade must hit the §9.10 gate and print
	// the "module only targets newer epochs" line, not the unversioned fallback.
	t.Run("only-newer-epochs", func(t *testing.T) {
		year, minor, ok := module.ParseEpoch(epoch)
		if !ok {
			t.Skipf("cannot parse compiler epoch %q", epoch)
		}
		newer := fmt.Sprintf("%d.%d", year, minor+1)
		work := clitest.MakeWorkRepo(t)
		clitest.WriteModule(t, work, "dep", "2026.0", true) // good module, but only tagged for a newer epoch
		clitest.GitRun(t, work, "add", ".")
		clitest.GitRun(t, work, "commit", "-m", "init")
		clitest.GitRun(t, work, "tag", "epoch-"+newer)
		commit := clitest.GitRun(t, work, "rev-parse", "HEAD")

		out, err := runCheck(t, work, commit)
		if err == nil {
			t.Fatalf("check-upgrade should exit non-zero when the only dep targets a newer epoch:\n%s", out)
		}
		if !strings.Contains(out, "module only targets newer epochs") {
			t.Errorf("expected the OnlyNewerEpochs CLI line, got:\n%s", out)
		}
		if !strings.Contains(out, "epoch-"+newer) {
			t.Errorf("expected the newer tag %q in output, got:\n%s", "epoch-"+newer, out)
		}
		// The versioned dep must never be reported via the unversioned/HEAD path.
		if strings.Contains(out, "unversioned") {
			t.Errorf("versioned dep mislabeled 'unversioned':\n%s", out)
		}
	})
}

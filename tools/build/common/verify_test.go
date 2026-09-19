package common

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunVerify_UnknownFlagReturnsUsageError verifies that passing an unknown
// flag causes RunVerify to return the usage error immediately (before any lock
// or filesystem side effects). Also confirms the usage string includes [--push].
// This is the wiring pin: parsing happens ahead of every side effect, so a
// mistyped flag cannot take the lock, clean, build or push.
func TestRunVerify_UnknownFlagReturnsUsageError(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	const want = verifyUsage
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

// TestParseVerifyArgs_EveryFlagSetsItsOption checks that every documented flag
// parses into the option it names, and into no other.
//
// It asserts on parseVerifyArgs rather than running RunVerify, which is what
// this coverage used to do (T2084). Running the pipeline to prove a flag parsed
// was both destructive and weak: `--shared --clean` resolved CleanHome to the
// host's ~/.promise and deleted it — the installed toolchain included — and the
// only assertion (the error is not a "usage:" error) was equally satisfied by a
// flag parsed into the wrong field, or by an ErrLockTimeout returned before the
// pipeline it claimed to exercise ever started.
func TestParseVerifyArgs_EveryFlagSetsItsOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want verifyOptions
	}{
		{"clean", []string{"--clean"}, verifyOptions{clean: true}},
		{"push", []string{"--push"}, verifyOptions{push: true}},
		{"lock-timeout", []string{"--lock-timeout=100ms"}, verifyOptions{lockTimeout: 100 * time.Millisecond}},
		{"lock-timeout separate value", []string{"--lock-timeout", "10m"}, verifyOptions{lockTimeout: 10 * time.Minute}},
		{"single dash", []string{"-clean"}, verifyOptions{clean: true}},
		{"none", nil, verifyOptions{}},
		// 0 is not "do not wait" — it is the zero value the absent flag leaves
		// behind, which acquireVerifyLockIn reads as "wait indefinitely". Anyone
		// reaching for --lock-timeout=0 to make verify give up immediately gets
		// the opposite, so the collision is pinned rather than left to be
		// rediscovered.
		{"zero timeout is the unbounded sentinel", []string{"--lock-timeout=0s"}, verifyOptions{}},
		{"repeated flag", []string{"--clean", "--clean"}, verifyOptions{clean: true}},
		{"last timeout wins", []string{"--lock-timeout=1s", "--lock-timeout=2s"}, verifyOptions{lockTimeout: 2 * time.Second}},
		{
			"all together",
			[]string{"--clean", "--push", "--lock-timeout=1s"},
			verifyOptions{clean: true, push: true, lockTimeout: time.Second},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVerifyArgs(tc.args)
			if err != nil {
				t.Fatalf("parseVerifyArgs(%v) = %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseVerifyArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseVerifyArgs_VariantOptionsAreRefused is the flag half of "one
// measurement, one meaning of blessed" (T2170).
//
// --wasm, --wasm-web, --shared and --local each used to change what a run
// measured or where it measured it, while the record it wrote said none of it:
// `bin/verify` and `bin/verify --shared --wasm` blessed the same tree id for two
// different measurements. They are REFUSED rather than ignored — a silent no-op
// would leave every caller that still passes one believing it got the suite it
// asked for, and the WASM suites are real measurements, addressed by name
// (`bin/gate wasm-test`, `bin/gate wasm-web-test`).
func TestParseVerifyArgs_VariantOptionsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		args []string
		// names is what the refusal must point the caller at: losing a flag and
		// losing a suite are different facts, and the two WASM suites are still
		// measured, by name, on a schedule.
		names string
	}{
		{[]string{"--wasm"}, "bin/gate wasm-test"},
		{[]string{"--wasm-web"}, "bin/gate wasm-web-test"},
		{[]string{"--shared"}, "bin/clean --shared"},
		{[]string{"--local"}, ".promise-home/"},
		{[]string{"--clean", "--wasm"}, "bin/gate wasm-test"},
		{[]string{"--shared", "--clean"}, "bin/clean --shared"},
		{[]string{"-wasm"}, "bin/gate wasm-test"},
	} {
		got, err := parseVerifyArgs(tc.args)
		if err == nil {
			t.Errorf("parseVerifyArgs(%v) = %+v, want a refusal", tc.args, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.names) {
			t.Errorf("parseVerifyArgs(%v) = %q, want it to name %q", tc.args, err, tc.names)
		}
		if !strings.Contains(err.Error(), verifyUsage) {
			t.Errorf("parseVerifyArgs(%v) = %q, want it to carry the usage line", tc.args, err)
		}
		if got != (verifyOptions{}) {
			t.Errorf("a rejected command line must yield zero options, got %+v", got)
		}
	}
}

// TestParseVerifyArgs_ATypoIsNotARetiredFlag: the two refusals are different on
// purpose. A retired option names where its measurement went; a mistyped one has
// nowhere to point, and inventing a destination for it would be a lie.
func TestParseVerifyArgs_ATypoIsNotARetiredFlag(t *testing.T) {
	_, err := parseVerifyArgs([]string{"--wsam"})
	if err == nil || err.Error() != verifyUsage {
		t.Errorf("parseVerifyArgs(--wsam) = %v, want exactly the usage line", err)
	}
}

// TestParseVerifyArgs_Rejections covers the error paths: an unknown flag gets
// the usage string, and a bad --lock-timeout value gets a flag-specific message
// naming the offending flag rather than the generic usage line.
func TestParseVerifyArgs_Rejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--unknown"}, "usage: bin/verify"},
		{"missing value", []string{"--lock-timeout"}, "-lock-timeout requires a duration value"},
		{"unparseable value", []string{"--lock-timeout=nope"}, "-lock-timeout: invalid duration"},
		{"negative value", []string{"--lock-timeout=-5s"}, "-lock-timeout: invalid duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVerifyArgs(tc.args)
			if err == nil {
				t.Fatalf("parseVerifyArgs(%v) = %+v, want an error", tc.args, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err.Error(), tc.want)
			}
			if got != (verifyOptions{}) {
				t.Errorf("a rejected command line must yield zero options, got %+v", got)
			}
		})
	}
}

func TestAcquireVerifyLock_WritesRepoDir(t *testing.T) {
	// Override lock dir to a temp directory so we don't conflict with real runs.
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// Holder metadata is recorded in the sibling .owner file (see
	// acquireVerifyLockIn — lockPath itself carries a mandatory byte-0 lock on
	// Windows and cannot be read while held).
	data, err := os.ReadFile(lockPath + ".owner")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	if got != "/home/user/my-repo" {
		t.Errorf("owner file = %q, want %q", got, "/home/user/my-repo")
	}
}

// TestAcquireVerifyLock_TimesOutWhenHeld holds the lock, then a second bounded
// acquire on the same path returns ErrLockTimeout (not a verification failure).
func TestAcquireVerifyLock_TimesOutWhenHeld(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/holder", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	start := time.Now()
	_, err = acquireVerifyLockIn(lockPath, "/waiter", 150*time.Millisecond)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v, want it to wait ~the lock timeout before giving up", elapsed)
	}
}

// TestAcquireVerifyLock_UnboundedAcquiresAfterRelease confirms lockTimeout=0
// waits (does not time out) and succeeds once the holder releases.
func TestAcquireVerifyLock_UnboundedAcquiresAfterRelease(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/holder", 0)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() {
		inner, ierr := acquireVerifyLockIn(lockPath, "/waiter", 0)
		if ierr == nil {
			inner()
		}
		acquired <- ierr
	}()

	// Give the waiter a moment to start blocking, then release.
	time.Sleep(100 * time.Millisecond)
	unlock()

	select {
	case ierr := <-acquired:
		if ierr != nil {
			t.Fatalf("unbounded waiter failed: %v", ierr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unbounded waiter did not acquire the lock after release")
	}
}

func TestAcquireVerifyLock_ClearsOnUnlock(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Unlock should clear the holder metadata.
	unlock()

	if _, err := os.Stat(lockPath + ".owner"); !os.IsNotExist(err) {
		t.Errorf("owner file should be removed after unlock, stat err = %v", err)
	}
}

// stepLog is a pipeline of steps that record the order they ran in, so the
// guarantees that matter — stop at the first failure, never push what was not
// blessed — can be asserted without taking a host lock or building a compiler
// (T2092). RunVerify's real pipeline is asserted separately, by name.
type stepLog struct {
	ran []string
}

func (l *stepLog) step(name string, err error) verifyStep {
	return verifyStep{name, func() error {
		l.ran = append(l.ran, name)
		return err
	}}
}

// verifyStepNames is the pipeline as a caller would read it.
func verifyStepNames(opts verifyOptions) []string {
	r := &verifyRun{root: "/nowhere", opts: opts}
	var names []string
	for _, s := range r.steps() {
		names = append(names, s.name)
	}
	return names
}

// TestVerifySteps_Order pins the sequence, which is where verify's guarantees
// live: the blessing is cleared before anything can change the tree, the build
// precedes the repairs (so `promise format` is the formatter `integration` then
// measures), the measurement precedes the record, and the record precedes the
// push.
func TestVerifySteps_Order(t *testing.T) {
	want := []string{
		"lock", "clear", "cache", "build",
		"format go", "format promise",
		"check go", "check structure",
		"integration", "record",
	}
	if got := verifyStepNames(verifyOptions{}); !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// TestVerifySteps_OptionalStepsSitWhereTheyBelong covers the two conditional
// steps. --clean must run before the cache is set up (it recreates the home
// empty), and --push must be last of all, after the record.
func TestVerifySteps_OptionalStepsSitWhereTheyBelong(t *testing.T) {
	both := verifyStepNames(verifyOptions{clean: true, push: true})
	if i, j := slices.Index(both, "clean"), slices.Index(both, "cache"); i < 0 || i > j {
		t.Errorf("clean at %d, cache at %d — clean must precede the cache setup: %v", i, j, both)
	}
	if got, want := both[len(both)-1], "push"; got != want {
		t.Errorf("last step = %q, want %q: %v", got, want, both)
	}
	if got, want := both[len(both)-2], "record"; got != want {
		t.Errorf("step before push = %q, want %q: %v", got, want, both)
	}
	for _, name := range []string{"clean", "push"} {
		if slices.Contains(verifyStepNames(verifyOptions{}), name) {
			t.Errorf("%q must not run without its flag", name)
		}
	}
}

// TestRunVerifySteps_StopsAtTheFirstFailureAndNamesIt is what makes the ordering
// above a guarantee rather than an intention: a step that failed ends the run,
// so nothing downstream has to re-check what happened upstream.
func TestRunVerifySteps_StopsAtTheFirstFailureAndNamesIt(t *testing.T) {
	var l stepLog
	boom := errors.New("boom")
	failed, err := runVerifySteps([]verifyStep{
		l.step("build", nil),
		l.step("check go", boom),
		l.step("integration", nil),
	})
	if failed != "check go" {
		t.Errorf("failed step = %q, want %q", failed, "check go")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap boom", err)
	}
	if !strings.HasPrefix(err.Error(), "check go: ") {
		t.Errorf("err = %q, want it to name the step that failed", err)
	}
	if want := []string{"build", "check go"}; !slices.Equal(l.ran, want) {
		t.Errorf("ran %v, want %v — nothing may run after a failure", l.ran, want)
	}
}

// TestRunVerifySteps_PushNeedsARecordedBlessing pins the one step whose effect
// leaves the machine: --push is downstream of the record, so a blessing that
// could not be written stops the run before anything is published.
func TestRunVerifySteps_PushNeedsARecordedBlessing(t *testing.T) {
	var l stepLog
	_, err := runVerifySteps([]verifyStep{
		l.step("integration", nil),
		l.step("record", errors.New("not gitignored")),
		l.step("push", nil),
	})
	if err == nil {
		t.Fatal("a failed record must fail the run")
	}
	if slices.Contains(l.ran, "push") {
		t.Errorf("pushed after a record that failed: %v", l.ran)
	}
}

// TestRunVerifySteps_InterruptStopsBeforeTheNextStep covers Ctrl+C. The pipeline
// stops between steps rather than mid-step, and reports the step it had reached.
func TestRunVerifySteps_InterruptStopsBeforeTheNextStep(t *testing.T) {
	t.Cleanup(func() { interrupted.Store(0) })
	var l stepLog
	failed, err := runVerifySteps([]verifyStep{
		l.step("build", nil),
		{"check go", func() error {
			l.ran = append(l.ran, "check go")
			interrupted.Store(1)
			return nil
		}},
		l.step("integration", nil),
	})
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v, want errInterrupted", err)
	}
	if failed != "check go" {
		t.Errorf("failed step = %q, want the step the run had reached", failed)
	}
	if want := []string{"build", "check go"}; !slices.Equal(l.ran, want) {
		t.Errorf("ran %v, want %v", l.ran, want)
	}
}

// TestRunVerify_HonoursTheParsedLockTimeout is the accepted-command-line half
// of the wiring, as TestRunVerify_UnknownFlagReturnsUsageError is the rejected
// half: --lock-timeout has to survive the trip from parseVerifyArgs into
// acquireVerifyLock, and the resulting ErrLockTimeout has to come back
// unwrapped.
//
// Both halves are load-bearing for the tracker runner, which distinguishes
// "another verify held the lock, retry next turn" from "verification failed" by
// errors.Is on this value. A regression that dropped opts.lockTimeout would not
// fail anything visibly — it would wait forever, which is exactly how the bug
// this test guards against would present.
//
// The lock is this test's own: HOME is redirected, so acquireVerifyLock resolves
// to a private ~/.promise rather than the host's, which an outer bin/verify
// holds while these tests run.
func TestRunVerify_HonoursTheParsedLockTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	lockDir := filepath.Join(home, ".promise")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireVerifyLockIn(filepath.Join(lockDir, "verify.lock"), "/holder", 0)
	if err != nil {
		t.Fatalf("acquireVerifyLockIn: %v", err)
	}

	// A stale blessing to watch: clearBlessing is RunVerify's first step
	// after the lock, so this file surviving proves the run gave up at the lock
	// and never entered the pipeline.
	root := t.TempDir()
	writeFile(t, root, ".workspace/verified-tree", "stale-blessing\n")

	done := make(chan error, 1)
	go func() { done <- RunVerify(root, []string{"--lock-timeout=100ms"}) }()

	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("RunVerify = %v, want ErrLockTimeout", err)
		}
	case <-time.After(30 * time.Second):
		// Deliberately not unlocking: the run is still blocked on the lock, and
		// releasing it now would send a verify with a broken timeout into the
		// pipeline — format, build, test and push — on a temp root, after
		// t.Setenv has already restored the real HOME.
		t.Fatal("RunVerify ignored --lock-timeout=100ms and is still waiting for the lock")
	}

	if !Exists(filepath.Join(root, ".workspace", "verified-tree")) {
		t.Error("a run that timed out on the lock must not have started the pipeline")
	}
}

// runVerifyStringArgs returns every string literal appearing inside each call to
// RunVerify in the given file, keyed by call site. Collecting literals from the
// whole call expression rather than matching an argument shape keeps it working
// for both RunVerify(dir, []string{...}) and any future spelling.
func runVerifyStringArgs(fset *token.FileSet, file *ast.File) map[string][]string {
	sites := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "RunVerify" {
			return true
		}
		site := fset.Position(call.Pos()).String()
		var args []string
		ast.Inspect(call, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					args = append(args, s)
				}
			}
			return true
		})
		sites[site] = NormalizeArgs(args)
		return true
	})
	return sites
}

// TestRunVerifyStringArgs exercises the scanner against synthetic source, so
// TestNoTestDrivesRunVerifyWithPush means something. Run only over the real
// (clean) tree, the scanner could match nothing at all — a typo'd function name,
// or a walk that never descends into the argument slice — and stay green
// forever.
func TestRunVerifyStringArgs(t *testing.T) {
	const src = `package p
func f() {
	RunVerify(dir, []string{"--clean", "--lock-timeout=30s"})
	RunVerify(dir, []string{"--push"})
	RunVerify(dir, nil)
	RunClean(dir, []string{"--push"})
	notRunVerify(dir, []string{"--push"})
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic_test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	sites := runVerifyStringArgs(fset, file)
	if len(sites) != 3 {
		t.Fatalf("found %d RunVerify call sites, want 3: %v", len(sites), sites)
	}

	var withPush, total int
	for _, args := range sites {
		total++
		if slices.Contains(args, "-push") {
			withPush++
		}
	}
	if withPush != 1 {
		t.Errorf("%d of %d call sites pass --push, want exactly 1 — the scanner is not reading the argument slice", withPush, total)
	}
	// --lock-timeout=30s must arrive normalized, the same way the parser sees it.
	var sawTimeout bool
	for _, args := range sites {
		if slices.Contains(args, "-lock-timeout") && slices.Contains(args, "30s") {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Error("literals must be normalized like a real command line (--lock-timeout=30s → -lock-timeout 30s)")
	}
}

// TestNoTestDrivesRunVerifyWithPush forbids this package's tests from handing
// RunVerify the one flag whose effect leaves the machine.
//
// The TestMain guard watches ~/.promise and the Go test cache, so a test that
// reached the clean is caught after the fact. A push cannot be caught that way:
// it publishes to the real remote, from whatever checkout the test pointed at,
// and there is nothing to compare afterwards. Before T2084 a flag-acceptance
// test did pass --push, and was harmless only because FormatGo happened to fail
// first on an empty temp root — one reordering away from a real push.
//
// Flag acceptance is a parser question: assert on parseVerifyArgs.
func TestNoTestDrivesRunVerifyWithPush(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var scanned int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		scanned++
		for site, args := range runVerifyStringArgs(fset, file) {
			if slices.Contains(args, "-push") {
				t.Errorf("%s: RunVerify must never be driven with --push from a test; "+
					"assert on parseVerifyArgs instead", site)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no _test.go files — the guard is looking in the wrong place")
	}
}

// TestAcquireVerifyLock_NoHomeRunsUnserialized covers the branch that gives up
// on locking entirely: with no user home there is nowhere to put the lock file,
// and acquireVerifyLock returns a no-op unlock and no error rather than
// refusing to run.
//
// That is a deliberate degradation, and worth a test precisely because it is
// silent — on such a host two concurrent verifies interleave over the same
// caches with nothing to say so. The contract pinned here is narrow: the caller
// still gets a usable unlock function (calling it must not panic), so the
// deferred release at every call site stays safe.
func TestAcquireVerifyLock_NoHomeRunsUnserialized(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on Windows

	unlock, err := acquireVerifyLock(t.TempDir(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("a host with no home must still run, got: %v", err)
	}
	if unlock == nil {
		t.Fatal("unlock must never be nil — every caller defers it")
	}
	// A second acquire proves nothing was actually taken: on a host with a home
	// this would block or time out.
	second, err := acquireVerifyLock(t.TempDir(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	second()
	unlock()
}

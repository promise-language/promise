package common

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

var errInterrupted = fmt.Errorf("interrupted by Ctrl+C")

// ErrLockTimeout is returned by RunVerify when --lock-timeout elapses before the
// host verify lock could be acquired. It is NOT a verification failure — it
// means another verify held the lock for the whole wait. Callers (the tracker
// runner) detect it (via errors.Is, or the EX_TEMPFAIL exit code / the
// VERIFY_LOCK_TIMEOUT stderr marker the verify binary prints) to retry for a
// turn rather than treating the run as failed.
var ErrLockTimeout = errors.New("verify lock acquisition timed out")

// lockRetryDelay is how often acquireVerifyLockIn re-polls for the lock while
// waiting under a bounded --lock-timeout.
const lockRetryDelay = 500 * time.Millisecond

// verifyOptions is bin/verify's command line, parsed.
//
// EVERY FLAG HERE IS UNABLE TO CHANGE WHAT IS MEASURED (T2170). What verify
// measures is `integration`, whole, and a flag that could narrow or widen it
// would make "blessed" mean different things on different invocations — which
// is the defect this file was rewritten for. --clean wipes caches before
// anything runs; --push acts after the blessing is recorded; --lock-timeout
// bounds a wait that happens before any measurement. The variant options
// --wasm, --wasm-web, --shared and --local are gone: the WASM suites are other
// targets' gates (docs/gate-system.md), and with one cache there is nothing to
// choose between.
type verifyOptions struct {
	clean bool
	push  bool
	// lockTimeout bounds how long to wait for the host verify lock. 0 (the
	// default, flag absent) waits UNBOUNDED — bin/verify is run on a variety of
	// machines where any hardcoded timeout would be wrong; bounding the wait is
	// the caller's choice via --lock-timeout (the tracker runner sets it so a
	// lost turn can be retried).
	lockTimeout time.Duration
}

// verifyUsage is the one spelling of what this tool accepts.
const verifyUsage = "usage: bin/verify [--clean] [--push] [--lock-timeout=<dur>]"

// retiredVerifyFlags are the variant options T2170 removed, each with what to
// run instead.
//
// The refusal NAMES THE REPLACEMENT, as every other refusal in this tree does —
// ensureRecordIgnored names the .gitignore line, the staleness check names
// ./make. A bare usage line says the flag is gone and leaves the reader to
// discover where the measurement went, and for the two WASM targets it went
// somewhere that still exists and is still run on a schedule. Losing a flag and
// losing a suite are different facts, and a caller is entitled to be told which
// one happened.
var retiredVerifyFlags = map[string]string{
	"wasm":     "The wasm32-wasi suite is its own gate: `bin/gate wasm-test`, or `bin/test --wasm` by hand.",
	"wasm-web": "The wasm32-web suite is its own gate: `bin/gate wasm-web-test`, or `bin/test --wasm-web` by hand.",
	"shared":   "Verify measures against the repo-local .promise-home/ and nothing else; `bin/clean --shared` is what still addresses ~/.promise.",
	"local":    "The repo-local .promise-home/ is the only cache verify uses, so there is nothing left to select.",
}

// unknownVerifyFlag refuses one argument. A retired variant option is told apart
// from a typo, because the two need different things said to them.
//
// The flag is named in its two-dash spelling rather than quoted back as typed:
// both spellings are accepted everywhere in this tool, NormalizeArgs has already
// collapsed them by the time this is reached, and echoing the normalized form
// would correct the reader in a spelling they may not have used.
func unknownVerifyFlag(arg string) error {
	if why, retired := retiredVerifyFlags[strings.TrimPrefix(arg, "-")]; retired {
		return fmt.Errorf("--%s is no longer a flag: verify measures `integration`, which is host-scoped. %s\n%s",
			strings.TrimPrefix(arg, "-"), why, verifyUsage)
	}
	return errors.New(verifyUsage)
}

// parseVerifyArgs parses bin/verify's flags and does nothing else — no lock, no
// filesystem, no subprocess. It is separate from RunVerify so that flag coverage
// can be a pure unit test: proving a flag parsed used to mean running the whole
// pipeline, and for `--shared --clean` that meant deleting the host's ~/.promise
// (T2084).
func parseVerifyArgs(args []string) (verifyOptions, error) {
	args = NormalizeArgs(args)
	var opts verifyOptions

	// --lock-timeout takes a duration value (NormalizeArgs has already split
	// --lock-timeout=10m into "-lock-timeout" "10m").
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-clean":
			opts.clean = true
		case "-push":
			opts.push = true
		case "-lock-timeout":
			if i+1 >= len(args) {
				return verifyOptions{}, fmt.Errorf("-lock-timeout requires a duration value (e.g. --lock-timeout=10m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d < 0 {
				return verifyOptions{}, fmt.Errorf("-lock-timeout: invalid duration %q (use Go duration syntax, e.g. 10m)", args[i])
			}
			opts.lockTimeout = d
		default:
			return verifyOptions{}, unknownVerifyFlag(args[i])
		}
	}
	return opts, nil
}

// verifyStep is one step of the pipeline, named as its failure will name it.
type verifyStep struct {
	name string
	run  func() error
}

// verifyRun is one bin/verify run: what its steps share, and what its summary is
// rendered from.
//
// The steps are a VALUE rather than a straight line of code (T2092) so their
// order and their stop-at-the-first-failure behaviour can be asserted. RunVerify
// itself takes a host-global lock and rebuilds the compiler, so it cannot be
// driven by a test directly — which is how the ordering guarantees that matter
// most (nothing is blessed before the measurement passed, nothing is pushed
// before it was blessed) went uncovered.
type verifyRun struct {
	root  string
	opts  verifyOptions
	start time.Time

	unlock func()
	store  casWindow

	// What the integration step measured and what the judge made of it. Only
	// these cross a step boundary: the summary reads them however the run ended,
	// and `record` blesses from them.
	env        Envelope
	parts      []PartResult
	terms      map[string]term
	acceptable bool
}

// RunVerify is the pre-commit pipeline: lock, clear the blessing, build, repair,
// check, measure `integration`, and — only if the judge passed it — record the
// blessing the commit guard reads.
//
// ITS TEST PHASE IS THE GATE, IN PROCESS. Not a spawned bin/gate and not a
// second implementation: MeasureContractGateParts is the entry point
// `bin/run integration` and `bin/gate integration` reach too, and verify's
// verdict is judge()'s verdict on that envelope. Verify used to run Go and
// Promise suites of its own, so "bin/verify passed" and "integration passed"
// were two answers about one tree that could differ — and only the first wrote
// the record the commit guard reads (T2170).
func RunVerify(root string, args []string) error {
	opts, err := parseVerifyArgs(args)
	if err != nil {
		return err
	}
	r := &verifyRun{root: root, opts: opts, start: time.Now()}
	defer r.release()

	failed, err := runVerifySteps(r.steps())
	// A run that never got the lock measured nothing, so it has no summary to
	// print — and its caller retries rather than reading one.
	if errors.Is(err, ErrLockTimeout) {
		return err
	}
	Progress().Print(r.summary(failed).String())
	if err != nil {
		return err
	}
	Progress().Println("✅ OK to commit")
	return nil
}

// steps is the pipeline, in order. Two orderings are load-bearing:
//
//   - CLEAR IS SECOND, right after the lock. A run that dies — a red step, a
//     Ctrl+C, a crash — must leave nothing blessed, or the commit guard would
//     honour a record describing content this run had already begun changing.
//   - BUILD PRECEDES THE REPAIRS. `promise format` is the formatter the compiler
//     carries, and `integration` MEASURES whether anything is still unformatted.
//     Repairing with a stale binary and being judged by the fresh one would fail
//     every change that touches the formatter, and on a fresh clone — no binary
//     yet — verify would not repair at all before being judged on it.
//
// The rest follows from what each step costs: the sweeps are milliseconds and
// come before the measurement, so a dangling link fails in seconds rather than
// after a full suite.
func (r *verifyRun) steps() []verifyStep {
	steps := []verifyStep{
		{"lock", r.stepLock},
		{"clear", r.stepClear},
	}
	if r.opts.clean {
		steps = append(steps, verifyStep{"clean", r.stepClean})
	}
	steps = append(steps,
		verifyStep{"cache", r.stepCache},
		verifyStep{"build", r.stepBuild},
		verifyStep{"format go", r.stepFormatGo},
		verifyStep{"format promise", r.stepFormatPromise},
		verifyStep{"check go", r.stepCheckGo},
		verifyStep{"check structure", r.stepCheckStructure},
		verifyStep{"integration", r.stepIntegration},
		verifyStep{"record", r.stepRecord},
	)
	if r.opts.push {
		steps = append(steps, verifyStep{"push", r.stepPush})
	}
	return steps
}

// runVerifySteps runs the steps in order and stops at the first failure, which
// it names. The name is the whole point: "structure: docs: dangling link" sends
// the reader to the sweep, where a bare "dangling link" sends them hunting for
// which phase produced it.
//
// Stopping is what makes the pipeline's guarantees hold by construction rather
// than by each step re-checking: nothing is recorded after a failed measurement,
// and nothing is pushed after a failed record.
func runVerifySteps(steps []verifyStep) (failed string, err error) {
	for _, s := range steps {
		if err := s.run(); err != nil {
			return s.name, fmt.Errorf("%s: %w", s.name, err)
		}
		if Interrupted() {
			Progress().Clear()
			return s.name, fmt.Errorf("%s: %w", s.name, errInterrupted)
		}
	}
	return "", nil
}

// stepLock serializes concurrent verify runs across the host.
func (r *verifyRun) stepLock() error {
	unlock, err := acquireVerifyLock(r.root, r.opts.lockTimeout)
	if err != nil {
		if errors.Is(err, ErrLockTimeout) {
			return err
		}
		return fmt.Errorf("acquire verify lock: %w", err)
	}
	r.unlock = unlock
	return nil
}

// release drops the host lock however the run ended.
func (r *verifyRun) release() {
	if r.unlock != nil {
		r.unlock()
	}
}

// stepClear drops any previous blessing before anything else runs.
func (r *verifyRun) stepClear() error { return clearBlessing(r.root) }

// stepClean wipes the repo-local home so the run starts from a known state.
// It cannot change what is measured — `integration` spells its own commands —
// only how much of the work is already cached.
func (r *verifyRun) stepClean() error {
	Progress().Println("Cleaning...")
	return cleanLocked(r.root, CleanOptions{})
}

// stepCache points this run at the repo-local .promise-home/.
func (r *verifyRun) stepCache() error { return SetupLocalCache(r.root) }

func (r *verifyRun) stepBuild() error {
	Progress().Println("Building compiler...")
	return RunBuild(r.root, nil)
}

func (r *verifyRun) stepFormatGo() error {
	Progress().Println("Formatting go...")
	return FormatGo(r.root)
}

func (r *verifyRun) stepFormatPromise() error {
	Progress().Println("Formatting promise...")
	if err := FormatPromiseFiles(r.root, filepath.Join(r.root, "bin", BinaryName())); err != nil {
		return err
	}
	Progress().Println()
	return nil
}

func (r *verifyRun) stepCheckGo() error {
	Progress().Println("Checking go...")
	return RunCheck(r.root)
}

// stepCheckStructure runs every check in structuralChecks, which is the one list
// of them (T2160). They read the working tree rather than a staged set, which is
// what lets them run here at all: verify has no staged set to look at.
//
// They live here because this is the pre-commit path this project actually uses.
// The git hook execs the workspace's bin/precommit-guard, which does not carry
// them, so between the hook losing the project's own tool and this call they held
// by review alone — and a rule with no enforcement is a rule that holds until the
// next person does not know it, which is the exact sentence that motivated the
// first of them. Going through the list rather than naming each check here is
// what keeps the next one from arriving with no caller at all.
func (r *verifyRun) stepCheckStructure() error {
	Progress().Println("Checking structure...")
	return RunStructuralChecks(r.root)
}

// stepIntegration measures the gate and judges it. This step IS verify's verdict.
func (r *verifyRun) stepIntegration() error {
	// What the measurement costs the content-addressed store, from here (T2143):
	// the build above and the toolchain warm-up inside openCASWindow come first,
	// so anything fetched or exploded below is work the TREE asked for a second
	// time — the shape T2133 had, and nothing measured for eighteen days.
	//
	// Reported to the operator, and deliberately NOT written into the gate values
	// below. This run does not pass -count=1, so its Go phase reports anywhere
	// from one home to thirty depending on how much of the suite Go's test cache
	// replayed (T2150); a ratchet fed from that would settle on whichever run
	// replayed the most and then fail every full one. The gates measure with
	// -count=1 and are where these numbers are judged.
	r.store = openCASWindow(r.root)

	// The identity of the content about to be measured. AFTER the repairs, so
	// what a passing run blesses is the tree a commit would carry; BEFORE the
	// measurement, so an edit made while it runs is caught rather than blessed
	// by a run that never saw it (T2008).
	before, _ := treeIdentity(r.root)

	Progress().Println("\nMeasuring integration...")
	env, parts, err := MeasureContractGateParts(r.root, integrationGate)
	if err != nil {
		return err
	}
	tree, moved := settledTree(r.root, before)
	env.Tree = tree
	env.Incomplete = joinIncomplete(env.Incomplete, moved)

	caps, baselines, err := projectTerms(r.root)
	if err != nil {
		return err
	}
	r.env, r.parts = env, parts
	acceptable, terms, detail := judge(env, caps, baselines)
	r.acceptable, r.terms = acceptable, terms
	if !acceptable {
		return errors.New(detail)
	}
	r.writeGateValues()
	return nil
}

// stepRecord blesses the tree the measurement passed on. The rule is
// blessIfPassed's, shared with bin/run, so verify and the flow cannot come to
// disagree about what a blessing means.
func (r *verifyRun) stepRecord() error { return blessIfPassed(r.root, r.env, r.acceptable) }

// stepPush is last, after the blessing is recorded. Recording first is
// deliberate: a push that fails (non-fast-forward, a network blip) has not
// unverified the content, and dropping the record would charge a full re-verify
// to get it back.
func (r *verifyRun) stepPush() error {
	Progress().Println("Pushing to remote...")
	return RunIn(r.root, "git", "push")
}

// writeGateValues writes the sidecar whatever advances a baseline reads.
//
// EXACTLY WHAT `integration` REPORTED, and nothing of verify's own: a number
// only verify produced is a number no gate can reproduce, and the wasm_* keys
// this used to add described suites `integration` does not measure at all.
// Warning-only, unlike the blessing: a stale sidecar is refused by its own
// worktree identity, where a missing blessing refuses every later commit.
func (r *verifyRun) writeGateValues() {
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  r.env.Target,
		Values:    make(map[string]float64, len(r.env.Metrics)),
	}
	for _, m := range r.env.Metrics {
		gv.Values[m.Name] = m.Number()
	}
	if err := WriteGateValues(r.root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}
}

// summary is the block verify always prints, whatever became of the run.
func (r *verifyRun) summary(failed string) verifySummary {
	target := r.env.Target
	if target == "" {
		target = HostTarget()
	}
	return verifySummary{
		Target:   target,
		Compiler: compilerIdentity(r.root),
		Parts:    r.parts,
		Terms:    r.terms,
		Failed:   failed,
		Store:    r.store.Summary(),
		Elapsed:  time.Since(r.start),
	}
}

// acquireVerifyLock acquires an OS-level file lock to serialize concurrent
// verify runs. The lock is automatically released by the OS if the process
// dies, so there is no risk of orphaned locks.
// Returns an unlock function that must be deferred.
func acquireVerifyLock(root string, lockTimeout time.Duration) (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return func() {}, nil
	}

	lockDir := filepath.Join(home, ".promise")
	os.MkdirAll(lockDir, 0o755)
	lockPath := filepath.Join(lockDir, "verify.lock")

	return acquireVerifyLockIn(lockPath, root, lockTimeout)
}

// acquireVerifyLockIn takes the host verify lock. lockTimeout <= 0 waits
// indefinitely (the default); a positive lockTimeout bounds the wait and
// returns ErrLockTimeout if the lock is still held when it elapses.
func acquireVerifyLockIn(lockPath, root string, lockTimeout time.Duration) (func(), error) {
	fl := flock.New(lockPath)
	// Holder metadata lives in a sibling file, NOT lockPath itself: on Windows
	// flock takes a mandatory byte-range lock on byte 0 of lockPath, so a
	// concurrent read/write of lockPath while the lock is held fails (the
	// repo-dir write would be silently lost and waiters couldn't read it). The
	// .owner sibling is unaffected by the lock and readable on every platform.
	ownerPath := lockPath + ".owner"

	// Try non-blocking first to detect contention.
	locked, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	if !locked {
		// Read the lock holder's repo directory before blocking.
		msg := "Waiting for another verify run to finish..."
		if data, err := os.ReadFile(ownerPath); err == nil {
			if dir := strings.TrimSpace(string(data)); dir != "" {
				msg = fmt.Sprintf("Waiting for verify run in %s to finish...", dir)
			}
		}
		fmt.Println(msg)
		if lockTimeout > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
			defer cancel()
			ok, lerr := fl.TryLockContext(ctx, lockRetryDelay)
			if lerr != nil && !errors.Is(lerr, context.DeadlineExceeded) {
				return nil, fmt.Errorf("acquire lock: %w", lerr)
			}
			if !ok {
				return nil, ErrLockTimeout
			}
		} else if err := fl.Lock(); err != nil {
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
	}

	// Record our repo directory for other waiters.
	os.WriteFile(ownerPath, []byte(root+"\n"), 0o644)

	return func() {
		os.Remove(ownerPath)
		fl.Unlock()
	}, nil
}

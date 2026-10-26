package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promise-language/promise/tools/build/internal/hostscope"
	"github.com/promise-language/promise/tools/build/internal/verifiedtree"
)

var errInterrupted = fmt.Errorf("interrupted by Ctrl+C")

// verifySpec is everything bin/verify accepts: the parameters every invocation
// does, and nothing of its own.
//
// NOTHING ON THIS COMMAND LINE CAN CHANGE WHAT IS MEASURED (T2170). What verify
// measures is `integration`, whole, and a flag that could narrow or widen it
// would make "blessed" mean different things on different invocations. The
// variant options --wasm, --wasm-web, --shared and --local went first; --clean,
// --push and --lock-timeout went with the tool contract (#624): a wiped cache is
// `bin/clean`, a push is `git push`, and the wait for the host-scope exclusion
// is unbounded because how long a turn on this machine takes is the queue's
// business, which a runner counts as waiting rather than against the run.
var verifySpec = ToolSpec{
	Tool:        "verify",
	Description: "repair what is mechanically repairable, then measure and judge integration and bless the tree it passed",
}

// The three things a step can have been, as the summary and the JSON both name
// them.
const (
	stepPassed = "passed"
	stepFailed = "failed"
	stepNotRun = "not-run"
)

// verifyStep is one step of the pipeline, named as its failure will name it,
// in the stage the summary groups it under.
type verifyStep struct {
	stage string
	name  string
	run   func() error
}

// stepResult is one step, what became of it, how long it took, and — for the
// one that failed — what it said.
type stepResult struct {
	Name           string      `json:"name"`
	Status         string      `json:"status"`
	ElapsedSeconds json.Number `json:"elapsed_seconds"`
	Detail         string      `json:"detail,omitempty"`
}

// stageResult is one stage of the run and what became of its steps.
type stageResult struct {
	Name  string       `json:"name"`
	Steps []stepResult `json:"steps"`
}

// verifyResult is what bin/verify answers in JSON: whether every stage passed,
// each stage's steps, and the tree the run blessed — absent when it recorded
// nothing.
type verifyResult struct {
	OK     bool          `json:"ok"`
	Stages []stageResult `json:"stages"`
	Tree   string        `json:"tree,omitempty"`
}

// verifyRun is one bin/verify run: what its steps share, and what its summary is
// rendered from.
//
// The steps are a VALUE rather than a straight line of code (T2092) so their
// order and their stop-at-the-first-failure behaviour can be asserted. RunVerify
// itself takes a host-global lock and rebuilds the compiler, so it cannot be
// driven by a test directly — which is how the ordering guarantees that matter
// most (nothing is blessed before the measurement passed) went uncovered.
type verifyRun struct {
	root  string
	start time.Time

	unlock func()
	store  casWindow

	// What the integration step measured and what the judge made of it. Only
	// these cross a step boundary: the summary reads them however the run ended,
	// and `record` blesses from them.
	env     Envelope
	parts   []PartResult
	verdict Verdict
	blessed string
}

// RunVerify is the pre-commit pipeline: lock, clear the blessing, build, repair,
// check, measure `integration`, and — only if the judge passed it — record the
// blessing the commit guard reads. It writes its summary, and only that, on
// stdout: the stage roll for a person, or one JSON object.
//
// ITS TEST PHASE IS THE GATE, IN PROCESS. Not a spawned bin/gate and not a
// second implementation: MeasureContractGateParts is the entry point
// `bin/run integration` and `bin/gate integration` reach too, and verify's
// verdict is judge()'s verdict on that envelope. Verify used to run Go and
// Promise suites of its own, so "bin/verify passed" and "integration passed"
// were two answers about one tree that could differ — and only the first wrote
// the record the commit guard reads (T2170).
func RunVerify(root string, args []string, stdout io.Writer) error {
	inv, err := ParseInvocation(verifySpec, args)
	if err != nil {
		return err
	}
	switch {
	case inv.Help:
		return WriteHelp(stdout, verifySpec, inv.Mode)
	case inv.Version:
		return WriteVersion(stdout, inv.Mode, versionText())
	}
	SetObserved(ObservedFor(inv))

	// Stdout carries the summary and nothing else, so for the whole run
	// os.Stdout points at the narration: the build, the formatters, a blob
	// fetch, Progress() — anything in this process that prints to os.Stdout —
	// cannot put a line in front of it.
	saved := os.Stdout
	os.Stdout = Narration()
	defer func() { os.Stdout = saved }()

	r := &verifyRun{root: root, start: time.Now()}
	defer r.release()
	results := runVerifySteps(r.steps())
	res := verifyResult{OK: true, Stages: results, Tree: r.blessed}
	for _, stage := range results {
		for _, s := range stage.Steps {
			if s.Status != stepPassed {
				res.OK = false
			}
		}
	}
	human := r.summary(failedStep(results)).String() + stepRoll(res)
	if inv.Mode == ModeJSON {
		fmt.Fprint(Narration(), human)
		if err := encodeLine(stdout, res); err != nil {
			return err
		}
	} else if _, err := io.WriteString(stdout, human); err != nil {
		return err
	}
	if !res.OK {
		return &reportedExit{code: 1}
	}
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
	return []verifyStep{
		{"prepare", "lock", r.stepLock},
		{"prepare", "clear", r.stepClear},
		{"prepare", "cache", r.stepCache},
		{"prepare", "build", r.stepBuild},
		{"repair", "gofmt", r.stepFormatGo},
		{"repair", "promise format", r.stepFormatPromise},
		{"check", "go vet", r.stepCheckGo},
		{"check", "structure", r.stepCheckStructure},
		{"measure", "integration", r.stepIntegration},
		{"record", "verified tree", r.stepRecord},
	}
}

// runVerifySteps runs the steps in order and stops at the first failure: the
// one that failed carries what it said, and every step after it is not-run.
// Each stage appears once, in the order its first step does.
//
// Stopping is what makes the pipeline's guarantees hold by construction rather
// than by each step re-checking: nothing is recorded after a failed measurement.
func runVerifySteps(steps []verifyStep) []stageResult {
	var stages []stageResult
	stopped := false
	for _, s := range steps {
		res := stepResult{Name: s.name, Status: stepNotRun, ElapsedSeconds: "0"}
		if !stopped {
			start := time.Now()
			err := s.run()
			if err == nil && Interrupted() {
				Progress().Clear()
				err = errInterrupted
			}
			res.ElapsedSeconds = json.Number(strconv.FormatFloat(time.Since(start).Seconds(), 'f', 3, 64))
			res.Status = stepPassed
			if err != nil {
				res.Status, res.Detail, stopped = stepFailed, err.Error(), true
			}
		}
		if n := len(stages); n == 0 || stages[n-1].Name != s.stage {
			stages = append(stages, stageResult{Name: s.stage})
		}
		stages[len(stages)-1].Steps = append(stages[len(stages)-1].Steps, res)
	}
	return stages
}

// failedStep names the step that stopped the run, "" when none did.
func failedStep(stages []stageResult) string {
	for _, stage := range stages {
		for _, s := range stage.Steps {
			if s.Status == stepFailed {
				return s.Name
			}
		}
	}
	return ""
}

// stepRoll is the stage roll a person reads under the summary block, ending
// with the failed step's own words — so the last lines of the output carry all
// the evidence — and then the one line saying whether the tree may be committed.
func stepRoll(res verifyResult) string {
	var b strings.Builder
	var failures []stepResult
	for _, stage := range res.Stages {
		fmt.Fprintf(&b, "  %s\n", stage.Name)
		for _, s := range stage.Steps {
			took := ""
			if s.Status != stepNotRun {
				secs, _ := strconv.ParseFloat(string(s.ElapsedSeconds), 64)
				took = time.Duration(secs * float64(time.Second)).Round(time.Millisecond).String()
			}
			fmt.Fprintf(&b, "    %-7s  %-16s %s\n", stepLabel(s.Status), s.Name, took)
			if s.Status == stepFailed {
				failures = append(failures, s)
			}
		}
	}
	if res.Tree != "" {
		fmt.Fprintf(&b, "  blessed tree %s\n", res.Tree)
	}
	for _, s := range failures {
		fmt.Fprintf(&b, "\n%s failed:\n", s.Name)
		for _, line := range strings.Split(strings.TrimRight(s.Detail, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	b.WriteString(verifySummaryThin + "\n")
	if res.OK {
		b.WriteString("✅ OK to Commit\n")
	} else {
		b.WriteString("❌ Verify FAILED: not safe to commit\n")
	}
	return b.String()
}

// stepLabel is how a status reads in the roll.
func stepLabel(status string) string {
	switch status {
	case stepPassed:
		return "ok"
	case stepFailed:
		return "FAIL"
	}
	return stepNotRun
}

// stepLock serializes concurrent verify runs across the host (acquireHostScope).
func (r *verifyRun) stepLock() error {
	unlock, err := acquireHostScope(r.root)
	if err != nil {
		return fmt.Errorf("acquire verify lock: %w", err)
	}
	r.unlock = unlock
	return nil
}

// release drops the host-scope exclusion however the run ended.
func (r *verifyRun) release() {
	if r.unlock != nil {
		r.unlock()
	}
}

// stepClear drops any previous blessing before anything else runs. The removal
// is verifiedtree's, like the writing end it undoes — an absent record is
// success there, which is exactly the state this step exists to produce.
func (r *verifyRun) stepClear() error { return verifiedtree.Clear(r.root) }

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
	// JUDGED here, like every other metric `integration` reports, and kept out
	// of the gate values below — the two are separate properties and this is the
	// only metric where they differ. Judging is owed because verify's verdict is
	// the judge's verdict on integration's envelope; a metric the gate enforces
	// and verify does not is a change that passes verify, is blessed by it, and
	// fails `bin/run integration`. Feeding a baseline is NOT owed: the Go phase
	// here does not pass -count=1, so a cache-replayed run spawns no compiler and
	// reports no home where a cold one reports its one, and a ratchet fed from
	// the first settles below every later run (T2150).
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
	// The same append `bin/gate` makes, from the same function, so the envelope
	// judged here and the one `bin/run integration` judges carry one metric set.
	r.store.AddToEnvelope(&env)
	tree, moved := settledTree(r.root, before)
	env.Tree = tree
	env.Incomplete = joinIncomplete(env.Incomplete, moved)

	terms, err := LoadTerms(r.root)
	if err != nil {
		return err
	}
	r.env, r.parts = env, parts
	r.verdict = judge(r.root, env, terms)
	if !r.verdict.Acceptable {
		return errors.New(r.verdict.Detail)
	}
	r.writeGateValues()
	return nil
}

// stepRecord blesses the tree the measurement passed on. The rule is
// blessIfPassed's, shared with bin/run, so verify and the flow cannot come to
// disagree about what a blessing means.
func (r *verifyRun) stepRecord() error {
	if err := blessIfPassed(r.root, r.env, r.verdict.Acceptable); err != nil {
		return err
	}
	r.blessed = r.env.Tree
	return nil
}

// writeGateValues writes the sidecar whatever advances a baseline reads.
//
// EXACTLY WHAT `integration` REPORTED, and nothing of verify's own: a number
// only verify produced is a number no gate can reproduce, and the wasm_* keys
// this used to add described suites `integration` does not measure at all.
// Warning-only, unlike the blessing: a stale sidecar is refused by its own
// worktree identity, where a missing blessing refuses every later commit.
//
// The store metrics are the one exception, and are dropped here: this run's Go
// phase replays whatever the test cache holds, so the home count it reports is
// a fact about the cache rather than about the tree. They are still judged —
// stepIntegration says why both halves are right.
func (r *verifyRun) writeGateValues() {
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  r.env.Target,
		Values:    make(map[string]float64, len(r.env.Metrics)),
	}
	for _, m := range r.env.Metrics {
		if isContractStoreMetric(m.Name) {
			continue
		}
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
		Terms:    r.verdict.Thresholds,
		Failed:   failed,
		Store:    r.store.Summary(),
		Elapsed:  time.Since(r.start),
	}
}

// acquireHostScope takes the host-scope exclusion, which serializes verify,
// clean and every gate that declares serialize "host" across every checkout on
// the machine. It is the orchestrator's exclusion, not one of this project's
// own (flow docs/gates-and-commands.md § Two scopes): bin/verify declares it on
// its `bin/run --list --json` row and each such gate on its `bin/gate --list`
// row, so a flow runner takes it before spawning them and reports the queue as
// waiting, outside the run's allowance. Taking the same exclusion here is what
// keeps that queue the only one (#96):
//
//   - under a runner, this checkout's arena already holds it, so the run is
//     let straight through and its release does nothing;
//   - run by hand, it queues on the same file as every runner and every other
//     hand-run verify or gate on the machine.
//
// The holder is the arena a runner names itself by, read from the same two
// records: the host id in the host home's identity.json and the arena id in
// root's .workspace/arena.json. A checkout with no arena record, or on a
// machine with no host record (a contributor clone, a CI runner), still takes
// the exclusion, as a party nothing can re-enter — no runner ever holds it for
// such a checkout, because flow refuses to run without both records. A record
// of either kind that does not parse is refused rather than read around, as
// flow refuses it.
//
// It refuses rather than degrading: a machine with nowhere to hold the
// exclusion gets an error, never an unserialized run. The kernel releases it if
// the process dies. The wait is unbounded: how long a turn on this machine
// takes is the queue's business, and the runner counts it as waiting, not
// against the run. The returned release must be deferred.
func acquireHostScope(root string) (func(), error) {
	return acquireHostScopeCtx(context.Background(), root)
}

// acquireHostScopeCtx is acquireHostScope bounded by ctx: a context that ends
// while the run is queued ends the wait with its error. Production code has no
// bound to give it; a test asking "would this queue?" does.
func acquireHostScopeCtx(ctx context.Context, root string) (func(), error) {
	home, _ := hostscope.HostHome()
	holder, err := hostscope.ArenaAt(home, root)
	if err != nil && !errors.Is(err, hostscope.ErrArenaUnknown) && !errors.Is(err, hostscope.ErrHostUnknown) {
		return nil, err
	}
	// Said once, when — and only when — this run is about to block behind
	// another arena: a free exclusion and a re-entered one print nothing.
	ctx = hostscope.OnQueue(ctx, func(hostscope.Scope) func() {
		fmt.Fprintln(Narration(), hostScopeWaitMessage())
		return nil
	})
	release, _, err := hostscope.Acquire(ctx, holder)
	if err != nil {
		return nil, err
	}
	return release, nil
}

// hostScopeWaitMessage names who this run is waiting for, as far as the
// exclusion's record says. The record is a reading, not a check — it can lag the
// lock — so it only ever feeds this line.
func hostScopeWaitMessage() string {
	if holder, ok := hostscope.Holder(); ok {
		return fmt.Sprintf("Waiting for the host-scope exclusion, held by arena %s on %s...", holder.Id, holder.Host)
	}
	return "Waiting for the host-scope exclusion, held by another verify or flow run..."
}

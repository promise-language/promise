package common

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// gateSpec is everything bin/gate accepts. The gates themselves are not in
// it: the command class points at `gate --list`, which reads the one registry
// (contractGates), so -help cannot come to name a gate the tool does not have.
var gateSpec = ToolSpec{
	Tool:        "gate",
	Description: "measure one property of this tree",
	Flags:       []FlagSpec{{Name: "list", Description: "name every gate this project answers"}},
	Commands: []CommandSpec{{
		Name:         "<gate>",
		Description:  "a gate this project answers",
		EnumeratedBy: "gate --list",
		Flags: []FlagSpec{{
			Name:        "envelope",
			Description: "write the measurement as one envelope on stdout",
			FixesOutput: true,
		}},
	}},
}

// RunGate is bin/gate: `bin/gate <gate> --envelope` measures one gate and
// writes its envelope on stdout, and `bin/gate --list` names the gates. Every
// malformed command line is one *UsageError (exit 2) that has written nothing
// on stdout, so a runner reads one envelope, one listing, or nothing.
func RunGate(root string, args []string) error {
	// A gate run is unattended CI — it must never be able to reach a model.
	// PROMISE_GATE propagates to every subprocess a gate spawns (RunBuild,
	// promise test, go test, ...) since none of the subprocess helpers in
	// exec.go set cmd.Env, so they all inherit os.Environ(). bin/guard checks
	// this marker and denies every prompt-invoking tool while it's set. Set
	// before anything else, so no path through this function runs without it.
	os.Setenv("PROMISE_GATE", "1")
	inv, err := ParseInvocation(gateSpec, args)
	if err != nil {
		return err
	}
	switch {
	case inv.Help:
		return WriteHelp(os.Stdout, gateSpec, inv.Mode)
	case inv.Version:
		return WriteVersion(os.Stdout, inv.Mode, versionText())
	}
	SetObserved(ObservedFor(inv))

	if inv.Command == "" {
		// --list answers "which gates does this project provide?". An
		// orchestrator must not hold a second copy of the list, and asking the
		// entry point is the only way to learn it that cannot go stale.
		if inv.Flags["list"] {
			return writeGateList(os.Stdout, root, inv.Mode)
		}
		return BareInvocation(gateSpec, versionText())
	}
	name := inv.Command
	if !IsContractGate(name) {
		return unknownContractGate(name)
	}
	// Without --envelope nothing is measured: a bare run that printed numbers
	// and exited 0 would be read as a pass by the first script that wrapped it.
	// The judged, human form of the same measurement is bin/run's.
	if !inv.Flags["envelope"] {
		return &UsageError{Tool: "gate", Problems: []string{fmt.Sprintf(
			"%s measures %s, and writes it only with --envelope; run `bin/run %s` for a result meant for a person",
			name, ContractGateSummary(name), name)}}
	}
	// A gate measures the tree on the pinned toolchain. Under a hand-pointed one
	// the measurement describes the operator's machine instead, and cannot stand
	// as evidence about a (commit, platform, check) triple — so it is refused as
	// a verdict source rather than allowed to masquerade as a pinned result
	// (T2108). After the command line, so listing still answers under an
	// override; before measuring, so the refusal prints nothing on stdout.
	// Building and testing by hand under an override is still fine: bin/build,
	// bin/test and bin/verify announce it and proceed.
	if overrides := ToolchainOverrides(); len(overrides) > 0 {
		return fmt.Errorf("refusing to measure under a toolchain override: %s\n"+
			"  a gate reports what the tree does on the pinned toolchain; this run would report what this machine does\n"+
			"  unset the variable(s) and re-run, or use bin/test / bin/verify for a by-hand run",
			strings.Join(overrides, " "))
	}
	return runContractGate(root, name, os.Stdout)
}

// gateGoTestArgv is the coverage gate's `go test` command: goTestArgs()'s bounded
// -p / -parallel / -timeout, plus -v and the -count=1 it needs to measure the
// tree rather than replay a saved result.
//
// It is goTestArgs() and not a second spelling because a tool and the gate that
// measures the same property are one implementation in two modes
// (docs/gate-system.md). The gate this was written for kept its own argv through
// T1817 and so went on running unbounded -p/-parallel long after bin/test
// stopped: on a 12-core host that is 82 concurrent compilers at load average 31,
// which is what turned a cold toolchain materialization into three-minute test
// timeouts (T2133).
//
// extra flags go ahead of the package pattern, so the coverage command below is
// this command plus instrumentation rather than a second copy of it.
func gateGoTestArgv(extra ...string) []string {
	return goTestArgs(append([]string{"-v", "-count=1"}, extra...)...)
}

// gateCoverageArgv is gateGoTestArgv plus the coverage instrumentation.
func gateCoverageArgv(covPkgs []string, covFile string) []string {
	return gateGoTestArgv("-coverpkg="+strings.Join(covPkgs, ","), "-coverprofile="+covFile)
}

// goCoveragePackages lists the compiler packages whose statements the coverage
// gate and bin/coverage measure: every package except the generated one, whose
// 30k machine-written lines would swamp the signal. The exclusion is
// excludeGeneratedGoPackages — the same one bin/check and checked:go apply, from
// the one place it is spelled. The per-area test packages under .../tests/ carry
// no non-test statements, so naming them costs nothing.
func goCoveragePackages(compilerDir string) ([]string, error) {
	out, err := RunOutputIn(compilerDir, "go", "list", "./...")
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}

	pkgs := excludeGeneratedGoPackages(out)
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages found to measure")
	}
	return pkgs, nil
}

// --- Parser helpers ---

// goModulePrefix is the repository's VCS-root import prefix. Go package paths
// are import paths (e.g. github.com/promise-language/promise/compiler/internal/
// codegen); stripping this yields the repo-relative directory (compiler/
// internal/codegen), matching the file identity used for Promise tests so the
// tracker sees one path convention across all gates. T0763.
const goModulePrefix = "github.com/promise-language/promise/"

// goPkgToRepoRel strips the repo module prefix from a Go import path, leaving a
// repo-relative directory. Paths without the prefix (e.g. third-party) are kept.
func goPkgToRepoRel(pkg string) string {
	if rel, ok := strings.CutPrefix(pkg, goModulePrefix); ok {
		return rel
	}
	return pkg
}

var stressIterRe = regexp.MustCompile(`(\d+) iterations over`)
var stressFlakyRe = regexp.MustCompile(`FLAKY \((\d+) tests?\):`)

// ParseStressOutput extracts iteration count and flaky test count from stress output.
func ParseStressOutput(output string) (iterations, flakyCount int) {
	if m := stressIterRe.FindStringSubmatch(output); m != nil {
		iterations, _ = strconv.Atoi(m[1])
	}
	if m := stressFlakyRe.FindStringSubmatch(output); m != nil {
		flakyCount, _ = strconv.Atoi(m[1])
	}
	return
}

var coverageTotalRe = regexp.MustCompile(`total:.*?(\d+\.\d+)%`)

// ParseCoverageTotal extracts the total coverage percentage from Go or Promise coverage output.
func ParseCoverageTotal(output string) float64 {
	if m := coverageTotalRe.FindStringSubmatch(output); m != nil {
		pct, _ := strconv.ParseFloat(m[1], 64)
		return pct
	}
	return 0
}

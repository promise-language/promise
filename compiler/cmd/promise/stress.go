package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"djabi.dev/go/promise_lang/internal/codegen"
	"djabi.dev/go/promise_lang/internal/module"
	"djabi.dev/go/promise_lang/internal/sema"
)

// --- Stress test types ---

type stressTarget struct {
	relPath  string   // display path
	binary   string   // compiled temp binary path
	isE2E    bool     // true for `test(expected:...) on main
	expected string   // expected output for e2e tests
	tests    []string // test function names
}

type testStats struct {
	name         string
	file         string
	passes       int
	fails        int
	timeouts     int       // subset of fails caused by timeout
	timings      []float64 // seconds per run (recorded for pass and non-timeout fail; excludes timeouts)
	lastErr      string    // last failure reason (for debugging)
	lastCrashCtx string    // detailed crash context: signal + stderr tail (for crash diagnosis)
}

func (s *testStats) total() int { return s.passes + s.fails }

func (s *testStats) passRate() float64 {
	if s.total() == 0 {
		return 1.0
	}
	return float64(s.passes) / float64(s.total())
}

func (s *testStats) mean() float64 {
	if len(s.timings) == 0 {
		return 0
	}
	sum := 0.0
	for _, t := range s.timings {
		sum += t
	}
	return sum / float64(len(s.timings))
}

func (s *testStats) stddev() float64 {
	if len(s.timings) < 2 {
		return 0
	}
	m := s.mean()
	sumSq := 0.0
	for _, t := range s.timings {
		d := t - m
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(s.timings)))
}

func (s *testStats) cov() float64 {
	m := s.mean()
	if m == 0 {
		return 0
	}
	return s.stddev() / m
}

func (s *testStats) minTime() float64 {
	if len(s.timings) == 0 {
		return 0
	}
	v := s.timings[0]
	for _, t := range s.timings[1:] {
		if t < v {
			v = t
		}
	}
	return v
}

func (s *testStats) maxTime() float64 {
	if len(s.timings) == 0 {
		return 0
	}
	v := s.timings[0]
	for _, t := range s.timings[1:] {
		if t > v {
			v = t
		}
	}
	return v
}

// isHighVariance returns true if timing variance is suspiciously high.
// Ignores sub-millisecond tests where measurement noise dominates.
func (s *testStats) isHighVariance() bool {
	return s.total() >= 5 && s.mean() > 0.005 && s.cov() > 1.0
}

type fileStats struct {
	path      string
	stats     map[string]*testStats
	testOrder []string // stable iteration order for stats map
	interval  int      // run every N iterations
	skipCount int      // counts down to 0
	runs      int      // total runs
}

func (f *fileStats) hasFailures() bool {
	for _, name := range f.testOrder {
		if f.stats[name].fails > 0 {
			return true
		}
	}
	return false
}

func (f *fileStats) hasHighVariance() bool {
	for _, name := range f.testOrder {
		if f.stats[name].isHighVariance() {
			return true
		}
	}
	return false
}

// recalcInterval updates the adaptive run interval for this file.
func (f *fileStats) recalcInterval() {
	if f.hasFailures() || f.hasHighVariance() {
		f.interval = 1
		return
	}
	switch {
	case f.runs < 20:
		f.interval = 1
	case f.runs < 50:
		f.interval = 2
	case f.runs < 100:
		f.interval = 4
	default:
		f.interval = 8
	}
}

// --- Compile phase ---

func compileTargets(files []string, baseDir string, targetTriple string) (targets []stressTarget, cleanup func()) {
	unlock := module.LockBuildDirShared()
	defer unlock()

	var tempFiles []string
	target := targetTriple
	if target == "" {
		target = codegen.HostTargetTriple()
	}

	// Dedup module test files by module directory.
	moduleTestSeen := map[string]bool{}

	for _, f := range files {
		relPath := f
		if baseDir != "" {
			if r, err := filepath.Rel(baseDir, f); err == nil {
				relPath = r
			}
		}

		// Module test dispatch.
		if modDir := isModuleTestFile(f); modDir != "" {
			if moduleTestSeen[modDir] {
				continue
			}
			moduleTestSeen[modDir] = true
			file, info := compileModuleTestFrontend(modDir, targetTriple)
			if len(info.Tests) == 0 {
				continue
			}
			ext := binaryExtension(target)
			tmp, err := os.CreateTemp("", "promise-stress-*"+ext)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
				os.Exit(1)
			}
			tmp.Close()
			result := codegen.Compile(file, info, target)
			result.GenerateTestMain(info.Tests, nil)
			compileAndLink(result, tmp.Name(), target, f)
			tempFiles = append(tempFiles, tmp.Name())
			var testNames []string
			for _, t := range info.Tests {
				if excludes, ok := info.TestExcludes[t.Name()]; ok {
					if isTestExcluded(target, excludes) {
						continue
					}
				}
				testNames = append(testNames, t.Name())
			}
			targets = append(targets, stressTarget{
				relPath: relPath,
				binary:  tmp.Name(),
				tests:   testNames,
			})
			continue
		}

		// Non-module test: check cache first.
		cacheKey, cacheable := computeTestFileCacheKey(f, target)
		var cacheDir string
		if cacheable {
			cacheDir, _ = module.BuildCacheDir()
		}

		if cacheDir != "" {
			if cachedBin := module.LookupTestBinaryCache(cacheDir, cacheKey); cachedBin != "" {
				meta := module.LoadTestBinaryMeta(cacheDir, cacheKey)
				if meta != nil && meta.E2E {
					if isTestExcluded(target, meta.ExcludeTargets) {
						continue
					}
					targets = append(targets, stressTarget{
						relPath:  relPath,
						binary:   cachedBin,
						isE2E:    true,
						expected: strings.TrimRight(meta.ExpectedOutput, "\n"),
						tests:    []string{strings.TrimSuffix(filepath.Base(f), ".pr")},
					})
				} else if meta != nil && len(meta.Tests) > 0 {
					var testNames []string
					for _, name := range meta.Tests {
						if excludes, ok := meta.TestExcludes[name]; ok {
							if isTestExcluded(target, excludes) {
								continue
							}
						}
						testNames = append(testNames, name)
					}
					targets = append(targets, stressTarget{
						relPath: relPath,
						binary:  cachedBin,
						tests:   testNames,
					})
				}
				continue
			}
		}

		// Cache miss — compile.
		file, info := compileFrontendForTarget(f, targetTriple)

		ext := binaryExtension(target)
		tmp, err := os.CreateTemp("", "promise-stress-*"+ext)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating temp file: %v\n", err)
			os.Exit(1)
		}
		tmp.Close()

		if info.HasExpectOutput {
			if isTestExcluded(target, info.ExcludeTargets) {
				os.Remove(tmp.Name())
				continue
			}
			result := codegen.Compile(file, info, target)
			compileAndLink(result, tmp.Name(), target, f)
			tempFiles = append(tempFiles, tmp.Name())

			// Save to cache.
			if cacheDir != "" {
				module.SaveTestBinaryCache(cacheDir, cacheKey, tmp.Name())
				module.SaveTestBinaryMeta(cacheDir, cacheKey, &module.CacheMeta{
					Kind:           module.CacheKindBinary,
					Name:           f,
					CacheKey:       cacheKey,
					E2E:            true,
					ExpectedOutput: info.ExpectOutput,
					ExcludeTargets: info.ExcludeTargets,
				})
			}

			targets = append(targets, stressTarget{
				relPath:  relPath,
				binary:   tmp.Name(),
				isE2E:    true,
				expected: strings.TrimRight(info.ExpectOutput, "\n"),
				tests:    []string{strings.TrimSuffix(filepath.Base(f), ".pr")},
			})
		} else if len(info.Tests) > 0 {
			result := codegen.Compile(file, info, target)
			result.GenerateTestMain(info.Tests, nil)
			compileAndLink(result, tmp.Name(), target, f)
			tempFiles = append(tempFiles, tmp.Name())

			var testNames []string
			testExcludes := map[string][]string{}
			for _, t := range info.Tests {
				testNames = append(testNames, t.Name())
				if excludes, ok := info.TestExcludes[t.Name()]; ok {
					testExcludes[t.Name()] = excludes
				}
			}

			// Save to cache.
			if cacheDir != "" {
				module.SaveTestBinaryCache(cacheDir, cacheKey, tmp.Name())
				module.SaveTestBinaryMeta(cacheDir, cacheKey, &module.CacheMeta{
					Kind:         module.CacheKindBinary,
					Name:         f,
					CacheKey:     cacheKey,
					Tests:        testNames,
					TestExcludes: testExcludes,
				})
			}

			var filteredNames []string
			for _, name := range testNames {
				if excludes, ok := testExcludes[name]; ok {
					if isTestExcluded(target, excludes) {
						continue
					}
				}
				filteredNames = append(filteredNames, name)
			}
			targets = append(targets, stressTarget{
				relPath: relPath,
				binary:  tmp.Name(),
				tests:   filteredNames,
			})
		} else {
			os.Remove(tmp.Name())
		}
	}

	cleanup = func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}
	return
}

// --- Run loop ---

func runStress(files []string, count int, duration time.Duration, perRunTimeout time.Duration, targetTriple string, outputFile string) {
	if len(files) == 0 {
		fmt.Println("no test files found")
		return
	}

	// Resolve host triple so the report always shows the platform.
	if targetTriple == "" {
		targetTriple = codegen.HostTargetTriple()
	}

	// SIGINT handler — set up before compile so Ctrl+C during compile works
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt)
	defer signal.Stop(stopCh)

	// Determine base directory for relative display paths
	baseDir := commonDir(files)

	// Compile all targets (exits on compile error)
	fmt.Fprintf(os.Stderr, "Compiling %d file(s)...\n", len(files))
	targets, cleanup := compileTargets(files, baseDir, targetTriple)
	defer cleanup()

	if len(targets) == 0 {
		fmt.Println("no tests found")
		return
	}

	totalTests := 0
	for _, t := range targets {
		totalTests += len(t.tests)
	}
	fmt.Fprintf(os.Stderr, "Compiled %d file(s), %d test(s). Starting stress loop.\n\n", len(targets), totalTests)

	// Initialize file stats with stable iteration order
	allFiles := make([]*fileStats, len(targets))
	for i, t := range targets {
		stats := make(map[string]*testStats, len(t.tests))
		for _, name := range t.tests {
			stats[name] = &testStats{name: name, file: t.relPath}
		}
		allFiles[i] = &fileStats{
			path:      t.relPath,
			stats:     stats,
			testOrder: t.tests, // preserves declaration order
			interval:  1,
		}
	}

	// TTY detection
	isTTY := false
	if fi, err := os.Stdout.Stat(); err == nil {
		isTTY = fi.Mode()&os.ModeCharDevice != 0
	}

	resultRe := regexp.MustCompile(`^(PASS|FAIL) \((\d+\.\d+)s\) (.+)$`)
	start := time.Now()
	iteration := 0
	lastProgress := time.Time{}

	for {
		// Check stopping conditions
		select {
		case <-stopCh:
			fmt.Fprintf(os.Stderr, "\n\nInterrupted.\n")
			goto report
		default:
		}

		if count > 0 && iteration >= count {
			break
		}
		if duration > 0 && time.Since(start) >= duration {
			break
		}

		iteration++

		// Run each target (with adaptive skipping)
		for i, t := range targets {
			fs := allFiles[i]

			// Adaptive skip
			if fs.skipCount > 0 {
				fs.skipCount--
				continue
			}

			// Run binary with separate stdout/stderr capture.
			// Test PASS/FAIL lines go to stdout; panic/crash output goes to stderr.
			runStart := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), perRunTimeout)
			var cmd *exec.Cmd
			if isWasmTarget(targetTriple) {
				cmd = exec.CommandContext(ctx, "wasmtime", t.binary)
			} else {
				cmd = exec.CommandContext(ctx, t.binary)
			}
			var stdoutBuf, stderrBuf bytes.Buffer
			cmd.Stdout = &stdoutBuf
			cmd.Stderr = &stderrBuf
			err := cmd.Run()
			timedOut := ctx.Err() == context.DeadlineExceeded
			cancel()
			wallClock := time.Since(runStart).Seconds()
			stdout := stdoutBuf.String()
			stderr := stderrBuf.String()

			// If SIGINT arrived during this run, the child was killed by the
			// signal — don't record that as a real failure.
			select {
			case <-stopCh:
				fmt.Fprintf(os.Stderr, "\n\nInterrupted.\n")
				goto report
			default:
			}

			fs.runs++

			if t.isE2E {
				// E2E: single test, compare combined stdout+stderr against expected.
				// Combined output matches CombinedOutput behavior (panic messages on
				// stderr are included). Non-zero exit code is NOT a failure if output
				// matches — this handles panic tests where the expected output IS the
				// panic message and the binary exits non-zero.
				name := t.tests[0]
				st := fs.stats[name]
				combined := strings.TrimRight(stdout+stderr, "\n")
				if timedOut {
					// Timeout counts as failure; don't add timeout duration to timings
					// as it would inflate CoV (the timeout ceiling is not real variance).
					st.fails++
					st.timeouts++
					st.lastErr = "timeout"
				} else if combined == t.expected {
					// Output matches — pass regardless of exit code (handles panic tests)
					st.timings = append(st.timings, wallClock)
					st.passes++
				} else if err != nil {
					// Output doesn't match AND binary crashed — capture crash context
					st.timings = append(st.timings, wallClock)
					st.fails++
					st.lastErr = extractCrashReason(stdout, stderr, err)
					st.lastCrashCtx = buildCrashContext(stderr, err)
				} else {
					// Output doesn't match but binary exited cleanly — output mismatch
					st.timings = append(st.timings, wallClock)
					st.fails++
					st.lastErr = failReason(t.expected, combined)
				}
			} else {
				// Unit tests: parse per-test results from stdout.
				// On timeout, we still parse whatever output the binary produced
				// before being killed — tests that completed get their real results.
				// Only the test that was running at timeout is marked as timeout.
				seen := make(map[string]bool, len(fs.testOrder))
				for _, line := range strings.Split(stdout, "\n") {
					m := resultRe.FindStringSubmatch(line)
					if m == nil {
						continue
					}
					status, timing, name := m[1], m[2], m[3]
					st := fs.stats[name]
					if st == nil {
						st = &testStats{name: name, file: t.relPath}
						fs.stats[name] = st
						fs.testOrder = append(fs.testOrder, name)
					}
					seen[name] = true
					if status == "PASS" {
						st.passes++
					} else {
						st.fails++
						st.lastErr = "test failed"
					}
					if v, e := strconv.ParseFloat(timing, 64); e == nil {
						st.timings = append(st.timings, v)
					}
				}
				if timedOut {
					// Find the first unseen test — that's the one that was running
					// when the timeout fired. Only attribute the timeout to it.
					for _, name := range fs.testOrder {
						if !seen[name] {
							st := fs.stats[name]
							st.fails++
							st.timeouts++
							st.lastErr = "timeout"
							break
						}
					}
				} else if err != nil {
					// Binary crashed — find the first unseen test (the one running
					// when the crash happened). Only count that test as failed.
					crashReason := extractCrashReason(stdout, stderr, err)
					crashCtx := buildCrashContext(stderr, err)
					for _, name := range fs.testOrder {
						if !seen[name] {
							st := fs.stats[name]
							st.fails++
							st.lastErr = crashReason
							st.lastCrashCtx = crashCtx
							break
						}
					}
				}
			}

			// Recalculate adaptive interval
			fs.recalcInterval()
			fs.skipCount = fs.interval - 1
		}

		// Display: TTY redraws every iteration; non-TTY prints every 2 seconds
		if isTTY {
			printStressLive(iteration, time.Since(start), allFiles, totalTests)
		} else if iteration == 1 || time.Since(lastProgress) >= 2*time.Second {
			printStressProgress(iteration, time.Since(start), allFiles, totalTests)
			lastProgress = time.Now()
		}
	}

report:
	// Clear live progress before printing the final report.
	if isTTY {
		fmt.Print("\033[H\033[2J")
	} else {
		fmt.Println()
	}
	report := buildStressReport(iteration, time.Since(start), allFiles, totalTests, targetTriple)
	fmt.Print(report)

	// Write report to file if requested.
	if outputFile != "" {
		if err := os.WriteFile(outputFile, []byte(report), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing report: %v\n", err)
		}
	}

	// Exit code: 1 if any flaky tests
	for _, fs := range allFiles {
		if fs.hasFailures() {
			os.Exit(1)
		}
	}
}

// --- Display ---

// collectTestsByCategory partitions all tests into flaky, high-variance, and stable.
// Returns sorted slices (flaky by worst pass rate, high-var by highest CoV).
func collectTestsByCategory(files []*fileStats) (flaky, highVar, stable []*testStats) {
	for _, fs := range files {
		for _, name := range fs.testOrder {
			st := fs.stats[name]
			if st.fails > 0 {
				flaky = append(flaky, st)
			} else if st.isHighVariance() {
				highVar = append(highVar, st)
			} else {
				stable = append(stable, st)
			}
		}
	}
	sort.Slice(flaky, func(i, j int) bool {
		return flaky[i].passRate() < flaky[j].passRate()
	})
	sort.Slice(highVar, func(i, j int) bool {
		return highVar[i].cov() > highVar[j].cov()
	})
	return
}

func printStressLive(iteration int, elapsed time.Duration, files []*fileStats, totalTests int) {
	fmt.Print("\033[H\033[2J") // clear screen

	fmt.Printf("Stress: %d iterations, %.1fs elapsed\n\n", iteration, elapsed.Seconds())

	flaky, highVar, _ := collectTestsByCategory(files)

	suppressedFiles := 0
	for _, fs := range files {
		if fs.interval > 1 {
			suppressedFiles++
		}
	}

	if len(flaky) > 0 {
		fmt.Printf("Flaky (%d):\n", len(flaky))
		printTestGroup(flaky, false)
		fmt.Println()
	}

	if len(highVar) > 0 {
		fmt.Printf("High variance (%d):\n", len(highVar))
		printTestGroup(highVar, true)
		fmt.Println()
	}

	stableCount := totalTests - len(flaky) - len(highVar)
	fmt.Printf("Stable: %d/%d tests", stableCount, totalTests)
	if suppressedFiles > 0 {
		fmt.Printf(" (%d files suppressed)", suppressedFiles)
	}
	fmt.Println()
}

func printStressProgress(iteration int, elapsed time.Duration, files []*fileStats, totalTests int) {
	flaky, highVar, _ := collectTestsByCategory(files)
	stableCount := totalTests - len(flaky) - len(highVar)
	fmt.Printf("Stress: iteration %d (%.1fs) — %d flaky, %d high-variance, %d/%d stable\n",
		iteration, elapsed.Seconds(), len(flaky), len(highVar), stableCount, totalTests)
}

// buildStressReport generates the final stress test report as a string.
func buildStressReport(iterations int, elapsed time.Duration, files []*fileStats, totalTests int, targetTriple string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== Stress Test Report ===\n")
	ti := sema.ParseTargetInfo(targetTriple)
	if ti.OS != "" && ti.Arch != "" {
		fmt.Fprintf(&b, "Target: %s-%s\n", ti.OS, ti.Arch)
	} else if targetTriple != "" {
		fmt.Fprintf(&b, "Target: %s\n", targetTriple)
	}
	if commit := gitCommitHash(); commit != "" {
		fmt.Fprintf(&b, "Commit: %s\n", commit)
	}
	fmt.Fprintf(&b, "%d iterations over %.1fs\n\n", iterations, elapsed.Seconds())

	flaky, highVar, stable := collectTestsByCategory(files)

	if len(flaky) > 0 {
		fmt.Fprintf(&b, "FLAKY (%d tests):\n", len(flaky))
		writeTestGroupDetailed(&b, flaky)
		fmt.Fprintln(&b)
	}

	if len(highVar) > 0 {
		fmt.Fprintf(&b, "HIGH VARIANCE (%d tests):\n", len(highVar))
		writeTestGroupDetailed(&b, highVar)
		fmt.Fprintln(&b)
	}

	if len(flaky) == 0 && len(highVar) == 0 {
		fmt.Fprintf(&b, "ALL STABLE: %d tests, all 100%% pass rate with low variance\n", totalTests)
	} else {
		stableFiles := map[string]bool{}
		for _, st := range stable {
			stableFiles[st.file] = true
		}
		fmt.Fprintf(&b, "STABLE: %d tests across %d files\n", len(stable), len(stableFiles))
	}
	return b.String()
}

// gitCommitHash returns the short git commit hash if the working tree is clean,
// or "" if unavailable or there are uncommitted changes.
func gitCommitHash() string {
	// Check for uncommitted changes first.
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil || len(strings.TrimSpace(string(status))) > 0 {
		return ""
	}
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// printTestGroup prints tests grouped by file, with compact stats.
// If showCoV is true, appends the coefficient of variation.
func printTestGroup(tests []*testStats, showCoV bool) {
	// Group by file while preserving order of first appearance
	type fileGroup struct {
		file  string
		tests []*testStats
	}
	var groups []fileGroup
	idx := map[string]int{}
	for _, st := range tests {
		if i, ok := idx[st.file]; ok {
			groups[i].tests = append(groups[i].tests, st)
		} else {
			idx[st.file] = len(groups)
			groups = append(groups, fileGroup{file: st.file, tests: []*testStats{st}})
		}
	}

	for _, g := range groups {
		fmt.Printf("  %s\n", g.file)
		for _, st := range g.tests {
			line := fmt.Sprintf("    %-30s pass: %d/%d (%.1f%%)  avg: %s  σ: %s",
				st.name, st.passes, st.total(), st.passRate()*100,
				fmtDuration(st.mean()), fmtDuration(st.stddev()))
			if showCoV {
				line += fmt.Sprintf("  (CoV: %.2f)", st.cov())
			}
			if st.fails > 0 {
				line += "  " + st.failSummary()
			}
			fmt.Println(line)
		}
	}
}

// writeTestGroupDetailed writes tests grouped by file, with full stats including min/max.
func writeTestGroupDetailed(w *strings.Builder, tests []*testStats) {
	type fileGroup struct {
		file  string
		tests []*testStats
	}
	var groups []fileGroup
	idx := map[string]int{}
	for _, st := range tests {
		if i, ok := idx[st.file]; ok {
			groups[i].tests = append(groups[i].tests, st)
		} else {
			idx[st.file] = len(groups)
			groups = append(groups, fileGroup{file: st.file, tests: []*testStats{st}})
		}
	}

	for _, g := range groups {
		fmt.Fprintf(w, "  %s\n", g.file)
		for _, st := range g.tests {
			fmt.Fprintf(w, "    %-30s %d/%d (%.1f%%)  avg: %s  σ: %s  min: %s  max: %s",
				st.name,
				st.passes, st.total(), st.passRate()*100,
				fmtDuration(st.mean()), fmtDuration(st.stddev()),
				fmtDuration(st.minTime()), fmtDuration(st.maxTime()))
			if st.isHighVariance() {
				fmt.Fprintf(w, "  CoV: %.2f", st.cov())
			}
			if st.fails > 0 {
				fmt.Fprintf(w, "\n      %s", st.failSummary())
			}
			fmt.Fprintln(w)
			// Print crash context if available (signal, stderr tail)
			if st.lastCrashCtx != "" {
				for _, line := range strings.Split(st.lastCrashCtx, "\n") {
					fmt.Fprintf(w, "      | %s\n", line)
				}
			}
		}
	}
}

// fmtDuration formats seconds as a human-readable duration string.
func fmtDuration(secs float64) string {
	if secs == 0 {
		return "0ms"
	}
	if secs < 0.001 {
		return fmt.Sprintf("%.0fμs", secs*1e6)
	}
	if secs < 1.0 {
		return fmt.Sprintf("%.1fms", secs*1e3)
	}
	return fmt.Sprintf("%.3fs", secs)
}

// failReason returns a short description of an e2e output mismatch.
func failReason(expected, actual string) string {
	if actual == "" {
		return "no output"
	}
	// Show first differing line
	expLines := strings.Split(expected, "\n")
	actLines := strings.Split(actual, "\n")
	for i := 0; i < len(expLines) && i < len(actLines); i++ {
		if expLines[i] != actLines[i] {
			got := actLines[i]
			if len(got) > 80 {
				got = got[:77] + "..."
			}
			return fmt.Sprintf("line %d: got %q", i+1, got)
		}
	}
	if len(actLines) != len(expLines) {
		return fmt.Sprintf("expected %d lines, got %d", len(expLines), len(actLines))
	}
	return "output mismatch"
}

// extractCrashReason returns a short reason from crash/panic output.
// Searches both stdout and stderr for panic messages, and extracts the
// signal name if the process was killed by a signal.
func extractCrashReason(stdout, stderr string, err error) string {
	// Check if killed by signal (SIGSEGV, SIGABRT, etc.)
	sig := extractSignal(err)

	// Look for panic message in stderr first, then stdout
	for _, src := range []string{stderr, stdout} {
		for _, line := range strings.Split(src, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") {
				if len(line) > 100 {
					line = line[:97] + "..."
				}
				if sig != "" {
					return fmt.Sprintf("%s (%s)", line, sig)
				}
				return line
			}
		}
	}

	if sig != "" {
		return sig
	}

	// Fall back to exit code (> 0 only; -1 means signal-killed which was handled above)
	exitCode := extractExitCode(err)
	if exitCode > 0 {
		return fmt.Sprintf("exit code %d", exitCode)
	}
	return "crash"
}

// extractSignal returns the signal name (e.g. "SIGSEGV") if the process
// was killed by a signal, or "" otherwise.
func extractSignal(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return signalName(ws.Signal())
		}
	}
	return ""
}

// signalName returns the standard signal constant name (e.g. "SIGSEGV")
// for common crash signals. Falls back to the OS description for unknown signals.
func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTRAP:
		return "SIGTRAP"
	default:
		return fmt.Sprintf("signal %d (%s)", int(sig), sig.String())
	}
}

// extractExitCode returns the process exit code, or -1 if unavailable.
func extractExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// buildCrashContext builds a detailed crash context string for diagnosis.
// Includes the signal, exit code, and last N lines of stderr.
func buildCrashContext(stderr string, err error) string {
	var parts []string

	// Signal info
	if sig := extractSignal(err); sig != "" {
		parts = append(parts, fmt.Sprintf("signal: %s", sig))
	}

	// Exit code
	if code := extractExitCode(err); code > 0 {
		parts = append(parts, fmt.Sprintf("exit code: %d", code))
	}

	// Last N lines of stderr (where panic messages and stack traces go)
	if tail := lastNLines(stderr, 20); tail != "" {
		parts = append(parts, "stderr:\n"+indent(tail, "  "))
	}

	if len(parts) == 0 {
		return "crash (no context available)"
	}
	return strings.Join(parts, "\n")
}

// lastNLines returns the last n non-empty lines of s.
func lastNLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	// Filter empty lines
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) == 0 {
		return ""
	}
	if len(nonEmpty) > n {
		nonEmpty = nonEmpty[len(nonEmpty)-n:]
	}
	return strings.Join(nonEmpty, "\n")
}

// indent prepends prefix to each line of s.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// failSummary returns a short string summarizing why a test is flaky.
func (s *testStats) failSummary() string {
	if s.fails == 0 {
		return ""
	}
	parts := []string{}
	if s.timeouts > 0 {
		parts = append(parts, fmt.Sprintf("%d timeout", s.timeouts))
	}
	nonTimeout := s.fails - s.timeouts
	if nonTimeout > 0 {
		parts = append(parts, fmt.Sprintf("%d fail", nonTimeout))
	}
	summary := strings.Join(parts, ", ")
	if s.lastErr != "" && s.lastErr != "timeout" && s.lastErr != "test failed" {
		summary += "  last: " + s.lastErr
	} else if s.lastErr != "" {
		summary += "  (" + s.lastErr + ")"
	}
	return summary
}

// commonDir returns the deepest common directory of the given file paths.
// Returns "" if there's only one file.
func commonDir(files []string) string {
	if len(files) <= 1 {
		return ""
	}
	// Use filepath.Abs so we compare canonical paths
	abs := make([]string, len(files))
	for i, f := range files {
		a, err := filepath.Abs(f)
		if err != nil {
			return ""
		}
		abs[i] = a
	}
	common := filepath.Dir(abs[0])
	for _, f := range abs[1:] {
		for common != "/" && common != "." {
			// Check with trailing separator to avoid /tmp/test matching /tmp/test2/
			prefix := common + string(filepath.Separator)
			if strings.HasPrefix(f, prefix) {
				break
			}
			common = filepath.Dir(common)
		}
	}
	return common
}

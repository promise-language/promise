package casmetrics

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestLedgerFormat pins the on-disk format. tools/build/common cannot import
// this package (separate Go module) and therefore spells the same line shape a
// second time; this test and TestCASLedgerFormat there are what make a change
// to one that is not made to the other fail immediately, instead of leaving a
// gate silently reporting zeros forever.
func TestLedgerFormat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l := at(dir)
	l.append(event{Event: eventNetwork, Bytes: 4096})
	l.append(event{Event: eventMaterialize, Name: "llvm-view", Bytes: 375, Count: 1})
	l.append(event{Event: eventHome, Path: "/w/.promise-home"})

	data, err := os.ReadFile(filepath.Join(dir, LedgerName))
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	const want = `{"event":"network","bytes":4096}
{"event":"materialize","name":"llvm-view","bytes":375,"count":1}
{"event":"home","path":"/w/.promise-home"}
`
	if string(data) != want {
		t.Errorf("ledger format drifted:\n got %q\nwant %q", data, want)
	}
	if LedgerName != ".promise-cas.jsonl" {
		t.Errorf("ledger name drifted: %q", LedgerName)
	}
}

// TestLedgerFolds: the four numbers a gate reports are a fold of the events,
// with homes and names deduplicated — two processes racing to register the same
// home must not read as two homes.
func TestLedgerFolds(t *testing.T) {
	t.Parallel()
	l := at(t.TempDir())
	l.append(event{Event: eventNetwork, Bytes: 100})
	l.append(event{Event: eventNetwork, Bytes: 23})
	l.append(event{Event: eventMaterialize, Name: "llvm-view", Bytes: 7, Count: 1})
	l.append(event{Event: eventMaterialize, Name: "crt-view", Count: 1}) // a link: no bytes
	l.append(event{Event: eventHome, Path: "/a"})
	l.append(event{Event: eventHome, Path: "/a"})
	l.append(event{Event: eventHome, Path: "/b"})

	got := l.read()
	if got.NetworkBytes != 123 {
		t.Errorf("NetworkBytes = %d, want 123", got.NetworkBytes)
	}
	if got.MaterializedBytes != 7 {
		t.Errorf("MaterializedBytes = %d, want 7", got.MaterializedBytes)
	}
	if got.Materializations != 2 {
		t.Errorf("Materializations = %d, want 2", got.Materializations)
	}
	if len(got.Homes) != 2 || got.Homes[0] != "/a" || got.Homes[1] != "/b" {
		t.Errorf("Homes = %v, want [/a /b]", got.Homes)
	}
	if len(got.Names) != 2 || got.Names[0] != "crt-view" || got.Names[1] != "llvm-view" {
		t.Errorf("Names = %v, want [crt-view llvm-view]", got.Names)
	}
}

// TestLedgerAbsentReadsAsAvailableZero: a run against a store that cost nothing
// must be distinguishable from a run nobody measured, so an absent ledger is a
// zero that IS available. This is what lets cas_network_bytes be judged at
// exactly zero.
func TestLedgerAbsentReadsAsAvailableZero(t *testing.T) {
	t.Parallel()
	got := at(t.TempDir()).read()
	if !got.Available {
		t.Error("an absent ledger in a writable directory read as unavailable")
	}
	if got.NetworkBytes != 0 || got.MaterializedBytes != 0 || got.Materializations != 0 || len(got.Homes) != 0 {
		t.Errorf("an absent ledger folded to %+v, want zeros", got)
	}
}

// TestLedgerUnwritableReadsAsUnavailable: where the ledger cannot be written —
// a read-only install directory — the zeros mean "not measured", and the reader
// is told so rather than reporting a clean run.
func TestLedgerUnwritableReadsAsUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory mode bits do not deny creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this asserts on")
	}
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	l := at(dir)
	l.append(event{Event: eventNetwork, Bytes: 9}) // silently does nothing
	got := l.read()
	if got.Available {
		t.Error("an unwritable directory read as an available ledger")
	}
	if got.NetworkBytes != 0 {
		t.Errorf("NetworkBytes = %d from a ledger that could not be written", got.NetworkBytes)
	}
}

// TestLedgerSkipsTornLine: a writer killed mid-append can leave a partial
// trailing line. The rest of the run's accounting is still worth having, so it
// is skipped rather than failing the whole read.
func TestLedgerSkipsTornLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l := at(dir)
	l.append(event{Event: eventNetwork, Bytes: 10})
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"event":"network","by`)
	f.Close()

	got := l.read()
	if got.NetworkBytes != 10 {
		t.Errorf("NetworkBytes = %d, want 10 (the torn line skipped)", got.NetworkBytes)
	}
}

// TestLedgerConcurrentAppends: many compilers materialize at once against one
// ledger and no lock. Every event has to survive — a lost one understates the
// cost, which is the failure this whole accounting exists to prevent.
func TestLedgerConcurrentAppends(t *testing.T) {
	t.Parallel()
	l := at(t.TempDir())
	const writers, each = 16, 25
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				l.append(event{Event: eventMaterialize, Name: fmt.Sprintf("view-%d", i), Bytes: 1, Count: 1})
			}
		}(i)
	}
	wg.Wait()

	got := l.read()
	if want := int64(writers * each); got.MaterializedBytes != want {
		t.Errorf("MaterializedBytes = %d, want %d — an append was lost or interleaved", got.MaterializedBytes, want)
	}
	if want := writers * each; got.Materializations != want {
		t.Errorf("Materializations = %d, want %d", got.Materializations, want)
	}
	if len(got.Names) != writers {
		t.Errorf("Names holds %d entries, want %d", len(got.Names), writers)
	}
}

// TestRegisterHomeIsIdempotent: a home already on file is not written again, so
// a run that compiles a thousand files still reads as one home.
func TestRegisterHomeIsIdempotent(t *testing.T) {
	t.Parallel()
	l := at(t.TempDir())
	for i := 0; i < 5; i++ {
		l.registerHome("/w/.promise-home")
	}
	l.registerHome("/tmp/other-home")

	got := l.read()
	if len(got.Homes) != 2 {
		t.Fatalf("Homes = %v, want two distinct entries", got.Homes)
	}
	data, _ := os.ReadFile(l.path)
	if n := countLines(string(data)); n != 2 {
		t.Errorf("ledger holds %d lines for two homes, want 2", n)
	}
}

// TestAddIgnoresNothingToReport: the warm path must not touch the file at all —
// a cache hit that appended "0 bytes" would grow the ledger on every compile.
func TestAddIgnoresNothingToReport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	AddNetwork(0)
	AddMaterialized("llvm-view", 0, 0)
	RegisterHome("")
	if _, err := os.Stat(filepath.Join(dir, LedgerName)); !os.IsNotExist(err) {
		t.Errorf("a no-op call created a ledger: %v", err)
	}
	l := at(dir)
	l.append(event{Event: eventMaterialize, Name: "crt-view", Count: 1})
	if got := l.read(); got.Materializations != 1 {
		t.Errorf("a zero-byte population was dropped: %+v", got)
	}
}

func countLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestFacadeWritesBesideTheRunningBinary drives the package-level functions
// rather than a ledger of its own, because WHERE they write is the whole design
// (a ledger inside a Promise home cannot see a run that used three) and every
// other test here deliberately bypasses it.
//
// Not parallel, and it restores what it found: this is the one ledger every
// caller in the process shares.
func TestFacadeWritesBesideTheRunningBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path to anchor a ledger to:", err)
	}
	path := filepath.Join(filepath.Dir(exe), LedgerName)
	restore := snapshot(t, path)
	defer restore()

	before := Read()
	if !before.Available {
		t.Skipf("the directory holding this test binary cannot hold a ledger (%s)", filepath.Dir(exe))
	}

	// A home this process has never seen, so the per-process memo cannot hide
	// the write the assertion is about.
	home := filepath.Join(t.TempDir(), "facade-home")
	AddNetwork(4096)
	AddMaterialized("probe-view", 7, 1)
	RegisterHome(home)

	after := Read()
	if got := after.NetworkBytes - before.NetworkBytes; got != 4096 {
		t.Errorf("NetworkBytes moved by %d, want 4096", got)
	}
	if got := after.MaterializedBytes - before.MaterializedBytes; got != 7 {
		t.Errorf("MaterializedBytes moved by %d, want 7", got)
	}
	if got := after.Materializations - before.Materializations; got != 1 {
		t.Errorf("Materializations moved by %d, want 1", got)
	}
	if !containsString(after.Names, "probe-view") {
		t.Errorf("Names = %v, want it to hold probe-view", after.Names)
	}
	if !containsString(after.Homes, home) {
		t.Errorf("Homes = %v, want it to hold %s", after.Homes, home)
	}

	// The second registration of the same home is memoized per process, so it
	// costs nothing — which is what keeps a thousand-file run at one line.
	linesBefore := len(after.Homes)
	RegisterHome(home)
	if got := len(Read().Homes); got != linesBefore {
		t.Errorf("re-registering a home changed the count from %d to %d", linesBefore, got)
	}
}

// snapshot captures a file's bytes (or its absence) and returns the restore.
func snapshot(t *testing.T, path string) func() {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return func() { os.Remove(path) }
	}
	return func() { os.WriteFile(path, data, 0o644) }
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestLedgerAppendLeaksNoDescriptors: every event opens the ledger and must
// hand the descriptor back. A run appends one per materialization across
// thousands of compiles, so a leak here exhausts the process's table and takes
// the build down somewhere unrelated — the shape of failure this whole item
// exists to make visible rather than to introduce.
func TestLedgerAppendLeaksNoDescriptors(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd to count descriptors with")
	}
	t.Parallel()
	l := at(t.TempDir())
	l.append(event{Event: eventNetwork, Bytes: 1}) // create the file first

	before := openDescriptors(t)
	for i := 0; i < 500; i++ {
		l.append(event{Event: eventNetwork, Bytes: 1})
		l.read()
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("500 append+read rounds leaked %d descriptor(s) (%d -> %d)", after-before, before, after)
	}
	if got := l.read().NetworkBytes; got != 501 {
		t.Errorf("NetworkBytes = %d, want 501 — an append was lost", got)
	}
}

// openDescriptors counts this process's open file descriptors.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("counting descriptors: %v", err)
	}
	return len(entries)
}

package testrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// End to end: what a run costs the content-addressed store is written to a
// ledger beside the compiler binary, and a run that uses several Promise homes
// is visible as several (T2143).
//
// This is the regression T2133 was: clitest.IsolateHome gave each of the three
// CLI test packages its own empty PROMISE_HOME, turning one warm 375 MB LLVM
// view materialization into three cold ones per run. Nothing measured it and it
// rode trunk for eighteen days, surfacing as a timeout in a progress-rendering
// test. The ledger lives beside the BINARY rather than inside a home precisely
// so that private homes cannot hide from it.

// compilerWithItsOwnLedger puts the built compiler in a directory of its own and
// returns its path.
//
// A directory of its own is what gives this test a ledger of its own: the real
// one under bin/ is a live measurement window whenever a gate is running, and a
// test that wrote to it would be counted into somebody else's numbers. It is the
// only way to get a second ledger — casmetrics derives the path from
// os.Executable() and from nothing else, deliberately, so that no environment
// variable can move it.
//
// Off Windows it is a hardlink, so it costs metadata rather than the tens of
// megabytes T2133 is about. On Windows it is a copy: a running image is locked
// by its file IDENTITY rather than by the path it was launched from, and a
// hardlink is the same file — so while any peer test drives bin\promise.exe,
// which is nearly always, the temp-dir link cannot be removed, and t.TempDir()'s
// RemoveAll failure is reported by testing as this test failing (T2154). ~35 MB
// of copy is what a deletable file costs there.
func compilerWithItsOwnLedger(t *testing.T) string {
	t.Helper()
	src := clitest.Bin(t)
	dst := filepath.Join(clitest.TempDir(t), "promise")
	if runtime.GOOS == "windows" {
		dst += ".exe"
	}
	// Off Windows the link is tried first, and the copy still covers what it
	// always did: different filesystems (a temp dir on another volume).
	if runtime.GOOS == "windows" || os.Link(src, dst) != nil {
		if err := copyExecutable(src, dst); err != nil {
			t.Fatal(err)
		}
	}
	// Ahead of t.TempDir()'s own RemoveAll — cleanups run last-registered-first —
	// and bounded, so the moment between this test's child exiting and Windows
	// releasing its image costs a few retries rather than a red test. The error
	// is deliberately dropped: a file that is genuinely stuck is reported by
	// RemoveAll next, in the message that names the path, so a real leaked child
	// still fails loudly.
	t.Cleanup(func() { _ = removeRetrying(os.Remove, removeBackoff, dst) })
	return dst
}

// copyExecutable copies src to dst as an executable file. Streamed rather than
// read-then-write: on Windows this is the normal path rather than a rare
// fallback, and holding a whole compiler in the test process's heap to place it
// is not worth it.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("reading the compiler: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("placing the compiler: %w", err)
	}
	if _, cerr := io.Copy(out, in); cerr != nil {
		out.Close()
		return fmt.Errorf("copying the compiler: %w", cerr)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing the placed compiler: %w", err)
	}
	return nil
}

// removeAttempts bounds the wait for a released image; removeBackoff yields the
// pause before attempt i+1. This is not synchronization with anything a test
// asserts on — every assertion is done by the time it runs — but a wait on a
// lock whose release has no observable event, the same shape as stub_embed.go's
// renameWithRetry.
const removeAttempts = 10

func removeBackoff(i int) time.Duration { return time.Duration(i+1) * 10 * time.Millisecond }

// removeRetrying deletes path, retrying while the removal is refused. Its
// dependencies are injected so the retry and exhaustion paths are reachable from
// a unit test on any platform — the same reason llvm_cas.go's linkOrCopy takes a
// link function rather than reading a package var every parallel test would race.
func removeRetrying(remove func(string) error, backoff func(int) time.Duration, path string) error {
	var err error
	for i := 0; i < removeAttempts; i++ {
		if err = remove(path); err == nil || os.IsNotExist(err) {
			return nil
		}
		if i < removeAttempts-1 { // no point sleeping after the final attempt
			time.Sleep(backoff(i))
		}
	}
	return err
}

// readLedger folds the ledger beside a compiler. Kept to the same three event
// kinds tools/build/common.readCASLedger folds, so a format change fails here
// as well as in the two golden tests.
func readLedger(t *testing.T, bin string) (homes, names []string, network, materialized int64, populations int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(bin), ".promise-cas.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, 0, 0, 0
		}
		t.Fatalf("reading the ledger: %v", err)
	}
	seenHome := map[string]bool{}
	seenName := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e struct {
			Event string `json:"event"`
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
			Count int    `json:"count"`
			Path  string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("ledger line is not JSON: %q", line)
		}
		switch e.Event {
		case "network":
			network += e.Bytes
		case "materialize":
			materialized += e.Bytes
			populations += e.Count
			if e.Name != "" && !seenName[e.Name] {
				seenName[e.Name] = true
				names = append(names, e.Name)
			}
		case "home":
			if e.Path != "" && !seenHome[e.Path] {
				seenHome[e.Path] = true
				homes = append(homes, e.Path)
			}
		}
	}
	return homes, names, network, materialized, populations
}

// TestStoreLedgerCountsEveryHomeARunUses is the assertion the item exists for:
// three compilations that each name a different PROMISE_HOME read as three
// homes, whatever their contents.
//
// The homes are three paths to ONE already-warm directory. That is deliberate:
// it isolates what the metric counts (distinct homes a run reaches the
// toolchain from) from what it must not depend on (how warm each one was), and
// it keeps the test hermetic — three genuinely empty homes would each stage a
// toolchain and fetch the musl CRT over the network, which no test may need.
func TestStoreLedgerCountsEveryHomeARunUses(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("a directory symlink needs administrator rights on Windows")
	}
	warm := os.Getenv("PROMISE_HOME")
	if warm == "" {
		t.Skip("no PROMISE_HOME to alias (TestMain sets one)")
	}
	bin := compilerWithItsOwnLedger(t)

	aliases := clitest.TempDir(t)
	var homes []string
	for i := 0; i < 3; i++ {
		alias := filepath.Join(aliases, fmt.Sprintf("home-%d", i))
		if err := os.Symlink(warm, alias); err != nil {
			t.Fatalf("aliasing the warm home: %v", err)
		}
		homes = append(homes, alias)
	}

	for i, home := range homes {
		// Unique source per run: an exec served from the build cache never
		// reaches the toolchain, so it registers nothing — correct, and the
		// reason a fully cached run reads zero homes rather than one.
		r := clitest.Run(t, bin, []string{"PROMISE_HOME=" + home}, "exec", uniqueProgram(t, i))
		if r.ExitCode != 0 {
			t.Fatalf("compiling under %s failed%s", home, r.Detail())
		}
	}

	gotHomes, _, _, _, _ := readLedger(t, bin)
	if len(gotHomes) != 3 {
		t.Fatalf("the ledger names %d home(s) for a run that used 3: %v\n"+
			"a per-home tally would have read 1 each and missed T2133 entirely", len(gotHomes), gotHomes)
	}
	for _, want := range homes {
		if !contains(gotHomes, want) {
			t.Errorf("home %s is missing from %v", want, gotHomes)
		}
	}
}

// TestStoreLedgerIsQuietOnAWarmHome: a cache hit costs the store nothing and
// must say so. Without this, "zero" could never be told apart from "nobody
// looked", and cas_network_bytes could not be judged at exactly zero.
func TestStoreLedgerIsQuietOnAWarmHome(t *testing.T) {
	t.Parallel()
	if os.Getenv("PROMISE_HOME") == "" {
		t.Skip("no warm PROMISE_HOME (TestMain sets one)")
	}
	bin := compilerWithItsOwnLedger(t)

	// The first compilation settles whatever this home still owed; the ledger
	// is then emptied and the second — a fresh source, so it really compiles —
	// is the warm run being asserted on.
	if r := clitest.Run(t, bin, nil, "exec", uniqueProgram(t, 0)); r.ExitCode != 0 {
		t.Fatalf("the first compilation failed%s", r.Detail())
	}
	if err := os.Remove(filepath.Join(filepath.Dir(bin), ".promise-cas.jsonl")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clearing the ledger: %v", err)
	}
	if r := clitest.Run(t, bin, nil, "exec", uniqueProgram(t, 1)); r.ExitCode != 0 {
		t.Fatalf("the warm compilation failed%s", r.Detail())
	}

	homes, names, network, materialized, populations := readLedger(t, bin)
	if network != 0 {
		t.Errorf("a warm run fetched %d bytes; the store was supposed to have them", network)
	}
	if materialized != 0 || populations != 0 {
		t.Errorf("a warm run materialized %d bytes over %d population(s): %v", materialized, populations, names)
	}
	if len(homes) != 1 {
		t.Errorf("a warm run names %d home(s), want exactly the one it used: %v", len(homes), homes)
	}
}

// TestPromiseTestEmitsTheStoreRecord: the --json stream carries the run's store
// cost once, with its zeros present, so a reader judging it against a baseline
// of zero can tell a clean run from an absent one.
func TestPromiseTestEmitsTheStoreRecord(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "store_record_test.pr")
	if err := os.WriteFile(src, []byte("one_ok() `test {\n  assert(1 == 1, \"ran\");\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := clitest.Run(t, bin, nil, "test", "--json", src)
	if r.ExitCode != 0 {
		t.Fatalf("promise test --json failed%s", r.Detail())
	}

	var records int
	for _, line := range strings.Split(r.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stream line is not JSON: %q", line)
		}
		if rec["kind"] != "cas" {
			continue
		}
		records++
		for _, field := range []string{"network_bytes", "materialized_bytes", "materializations", "available"} {
			if _, ok := rec[field]; !ok {
				t.Errorf("the store record omits %q: %s", field, line)
			}
		}
		if _, ok := rec["test"]; ok {
			t.Errorf("the store record carries test identity and would be counted as a test: %s", line)
		}
	}
	if records != 1 {
		t.Errorf("the stream carries %d store records, want exactly 1\nstdout:\n%s", records, r.Stdout)
	}
}

// uniqueProgram is a trivial program no build cache can already hold, so the
// invocation actually compiles and therefore actually reaches the toolchain.
func uniqueProgram(t *testing.T, n int) string {
	t.Helper()
	return fmt.Sprintf("// %s-%d-%d\nprint_line(\"\");", t.Name(), os.Getpid(), n)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestStoreLedgerReportsWhatAColdHomeCost is the other half of the pair above:
// a home that has to materialize reports it, with the bytes it wrote and the
// trees it wrote them into, and the byte total agrees with what is on disk
// afterwards.
//
// The home is cold but NOT empty: its CAS is seeded by hardlinking the warm
// home's blobs, which costs metadata and makes the compile entirely local. That
// is deliberate on two counts — no test may need the network, and it isolates
// the two directions the item is about, since a home that had the blobs and
// still fetched would be the defect `cas_network_bytes` exists to catch.
//
// It does not run on Windows today: it skips above, before placing a compiler,
// because a debug worktree home holds no cache/blobs to seed from. If it ever
// does run there, note that it carries T2154's hazard one step removed —
// linkTree seeds by hardlink and materializeViewFile hardlinks the view on
// Windows (llvm_cas.go, T2133), so the seeded blobs AND the view built from them
// share file identity with the warm home's blobs, which peer compilers are
// executing; t.TempDir()'s RemoveAll would then be refused exactly as it was for
// the compiler itself. Copying instead is not available here — 375 MB+ is
// precisely what T2143 measures against — so the treatment is a Windows skip,
// like the one TestStoreLedgerCountsEveryHomeARunUses already takes.
func TestStoreLedgerReportsWhatAColdHomeCost(t *testing.T) {
	t.Parallel()
	warm := os.Getenv("PROMISE_HOME")
	if warm == "" {
		t.Skip("no warm PROMISE_HOME to seed from (TestMain sets one)")
	}
	blobs := filepath.Join(warm, "cache", "blobs")
	if _, err := os.Stat(blobs); err != nil {
		t.Skip("the warm home holds no CAS blobs to seed a cold one from")
	}

	bin := compilerWithItsOwnLedger(t)
	cold := filepath.Join(clitest.TempDir(t), "cold-home")
	if err := linkTree(blobs, filepath.Join(cold, "cache", "blobs")); err != nil {
		t.Skipf("could not seed a cold home without copying: %v", err)
	}

	r := clitest.Run(t, bin, []string{"PROMISE_HOME=" + cold}, "exec", uniqueProgram(t, 0))
	if r.ExitCode != 0 {
		t.Fatalf("compiling against a cold home failed%s", r.Detail())
	}

	homes, names, network, materialized, populations := readLedger(t, bin)
	if populations == 0 {
		t.Fatalf("a cold home reported no materializations; names=%v homes=%v", names, homes)
	}
	if network != 0 {
		t.Errorf("a cold home whose CAS was seeded still fetched %d bytes — the blobs were there", network)
	}
	if len(homes) != 1 || homes[0] != cold {
		t.Errorf("homes = %v, want exactly the cold one (%s)", homes, cold)
	}
	if len(names) == 0 {
		t.Error("the materializations name nothing, so a non-zero reading cannot point at its cause")
	}
	// The reported bytes are what was WRITTEN, which on Linux excludes the
	// symlinked and hardlinked views — so the claim that holds on every platform
	// is that it never exceeds what the home now holds.
	if onDisk := treeSize(t, cold); materialized > onDisk {
		t.Errorf("reported %d bytes materialized, but the home only holds %d", materialized, onDisk)
	}
}

// linkTree mirrors a directory by hardlinking every file, so seeding a cold
// home's CAS costs metadata rather than the tens of megabytes the blobs weigh.
func linkTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return os.Link(path, target)
	})
}

// treeSize is the total size of every regular file under root.
func treeSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// TestCopyExecutablePlacesAnIndependentFile pins the property the Windows branch
// of compilerWithItsOwnLedger exists for, and the one a hardlink does NOT have:
// the placed file is a different file, so removing it is nobody else's business.
// Byte-identity and an executable mode are the other two halves of "a compiler
// that still runs".
func TestCopyExecutablePlacesAnIndependentFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	want := []byte("\x7fELF not really, but bytes are bytes\n")
	if err := os.WriteFile(src, want, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")

	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the copy is not byte-identical: %q, want %q", got, want)
	}
	si, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(si, di) {
		t.Error("the copy is the same file as the source — that is a hardlink, " +
			"which is exactly what cannot be removed on Windows while a peer runs it (T2154)")
	}
	if runtime.GOOS != "windows" && di.Mode()&0o111 == 0 {
		t.Errorf("the copy is not executable: mode %v", di.Mode())
	}
	// Last, because it removes dst: the placer must not still hold it open. A
	// leaked handle is invisible on POSIX and is refused on Windows, which is
	// the platform this whole placement exists for — so the assertion that
	// catches it has to be made here rather than discovered there.
	if err := os.Remove(dst); err != nil {
		t.Errorf("the placed file could not be removed straight away — copyExecutable "+
			"left a handle open: %v", err)
	}
}

// TestCopyExecutableReportsAnUnplaceableDestination covers the second of the
// three error branches: the source opens and the destination does not. It is a
// separate message from the read side on purpose — the two failures send the
// reader to different directories.
func TestCopyExecutableReportsAnUnplaceableDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := copyExecutable(src, filepath.Join(dir, "no-such-dir", "dst.bin"))
	if err == nil {
		t.Fatal("copying into a directory that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), "placing the compiler") {
		t.Errorf("error does not name the destination side: %v", err)
	}
}

// TestCopyExecutableReportsAFailedTransfer covers the third branch: the source
// opens but will not read. A directory arranges that portably — os.Open accepts
// one and the first Read does not — and what matters is that the failure is
// reported rather than leaving a truncated binary behind reporting success.
func TestCopyExecutableReportsAFailedTransfer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")

	err := copyExecutable(src, dst)
	if err == nil {
		t.Fatal("copying a directory reported success")
	}
	// Either side may be the one that refuses, depending on the platform; what
	// must never happen is a silent success.
	if !strings.Contains(err.Error(), "the compiler") {
		t.Errorf("error names neither side of the placement: %v", err)
	}
}

// TestCopyExecutableOverwritesWhatIsAlreadyThere: the destination is truncated
// rather than partially overwritten, so a shorter compiler cannot leave a longer
// one's tail behind and produce a binary that is neither.
func TestCopyExecutableOverwritesWhatIsAlreadyThere(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("short"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(dst, []byte("a much longer previous occupant"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "short" {
		t.Errorf("the copy left %q behind, want %q", got, "short")
	}
}

// TestCopyExecutableReportsAnUnreadableSource: the failure names which half of
// the placement failed, since "placing the compiler" for a missing source sends
// the reader to the wrong directory.
func TestCopyExecutableReportsAnUnreadableSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := copyExecutable(filepath.Join(dir, "absent"), filepath.Join(dir, "dst.bin"))
	if err == nil {
		t.Fatal("copying an absent source succeeded")
	}
	if !strings.Contains(err.Error(), "reading the compiler") {
		t.Errorf("error does not name the read side: %v", err)
	}
}

// noBackoff drives removeRetrying's loop without sleeping through its ~0.55s
// production budget (the same device stub_embed_test.go uses for renameRetrying).
func noBackoff(int) time.Duration { return 0 }

// TestRemoveRetryingOutlastsATransientRefusal is the case the retry exists for:
// Windows can hold a just-exited image for a moment, and that moment must cost
// retries rather than the test.
func TestRemoveRetryingOutlastsATransientRefusal(t *testing.T) {
	t.Parallel()
	calls := 0
	remove := func(string) error {
		calls++
		if calls < 3 {
			return fs.ErrPermission
		}
		return nil
	}
	if err := removeRetrying(remove, noBackoff, "p"); err != nil {
		t.Fatalf("removeRetrying gave up on a transient refusal: %v", err)
	}
	if calls != 3 {
		t.Errorf("removeRetrying made %d attempts, want 3", calls)
	}
}

// TestRemoveRetryingReportsAPersistentRefusal: a file that is genuinely stuck is
// not retried forever, and the last error is returned rather than swallowed —
// the caller drops it so RemoveAll can report the same path, but a helper that
// reported success would make that impossible to tell from a clean removal.
func TestRemoveRetryingReportsAPersistentRefusal(t *testing.T) {
	t.Parallel()
	calls := 0
	remove := func(string) error {
		calls++
		return fs.ErrPermission
	}
	err := removeRetrying(remove, noBackoff, "p")
	if err == nil {
		t.Fatal("removeRetrying reported success for a file it never removed")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("removeRetrying returned %v, want the refusal it saw", err)
	}
	if calls != removeAttempts {
		t.Errorf("removeRetrying made %d attempts, want %d", calls, removeAttempts)
	}
}

// TestRemoveRetryingAcceptsAnAlreadyAbsentFile: nothing to remove is the goal
// state, not an error, and it must not spend the whole retry budget reaching
// that conclusion.
func TestRemoveRetryingAcceptsAnAlreadyAbsentFile(t *testing.T) {
	t.Parallel()
	calls := 0
	remove := func(string) error {
		calls++
		return fs.ErrNotExist
	}
	if err := removeRetrying(remove, noBackoff, "p"); err != nil {
		t.Fatalf("removeRetrying failed on an absent file: %v", err)
	}
	if calls != 1 {
		t.Errorf("removeRetrying made %d attempts for an absent file, want 1", calls)
	}
}

// TestRemoveBackoffIsBoundedAndRising: this schedule runs in the cleanup of
// every test that places a compiler, on every platform, so its total is a cost
// the whole suite pays. Pinning it keeps a "just make it more patient" edit from
// silently turning a bounded wait into a stall — the retries are there to absorb
// a moment, and a file that is genuinely stuck is RemoveAll's to report.
func TestRemoveBackoffIsBoundedAndRising(t *testing.T) {
	t.Parallel()
	var slept time.Duration
	prev := time.Duration(-1)
	for i := 0; i < removeAttempts; i++ {
		d := removeBackoff(i)
		if d <= prev {
			t.Errorf("backoff(%d) = %s, not longer than backoff(%d) = %s", i, d, i-1, prev)
		}
		prev = d
		if i < removeAttempts-1 { // the final attempt is not followed by a sleep
			slept += d
		}
	}
	if slept > time.Second {
		t.Errorf("the removal retry budget is %s of sleeping; a cleanup that can stall "+
			"this long is worse than the failure it absorbs", slept)
	}
}

// TestPlacedCompilerLeavesNothingBehind: placing a compiler costs ~35 MB of copy
// on Windows, so a placement that outlived its test would put that on every run,
// growing %TEMP% until something else fails. The subtest is the device — its
// cleanups have run by the time t.Run returns, so the outer test sees what they
// left.
//
// It does NOT pin the ordering between the retrying removal and t.TempDir()'s
// RemoveAll, and no portable test can: off Windows both orders end with the file
// gone, and which one removed it is not observable from outside. That ordering
// is argued in compilerWithItsOwnLedger's comment and only has consequences on
// the platform where RemoveAll can be refused.
func TestPlacedCompilerLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	var placed string
	t.Run("place", func(t *testing.T) {
		placed = compilerWithItsOwnLedger(t)
		if _, err := os.Stat(placed); err != nil {
			t.Fatalf("the compiler was not placed: %v", err)
		}
	})
	if placed == "" {
		t.Skip("no compiler to place (clitest.Bin skipped the subtest)")
	}
	if _, err := os.Stat(placed); !os.IsNotExist(err) {
		t.Errorf("the placed compiler outlived the test that placed it (stat err = %v) — "+
			"on Windows that is a whole compiler leaked into %%TEMP%% per run", err)
	}
}

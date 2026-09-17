package testrun

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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

// linkedCompiler puts the built compiler in a directory of its own — by
// hardlink, so it costs metadata rather than the tens of megabytes T2133 is
// about — and returns its path.
//
// A directory of its own is what gives this test a ledger of its own: the real
// one under bin/ is a live measurement window whenever a gate is running, and a
// test that wrote to it would be counted into somebody else's numbers.
func linkedCompiler(t *testing.T) string {
	t.Helper()
	src := clitest.Bin(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "promise")
	if runtime.GOOS == "windows" {
		dst += ".exe"
	}
	if err := os.Link(src, dst); err != nil {
		// Different filesystems (a temp dir on another volume): pay for the copy.
		data, rerr := os.ReadFile(src)
		if rerr != nil {
			t.Fatalf("reading the compiler: %v", rerr)
		}
		if werr := os.WriteFile(dst, data, 0o755); werr != nil {
			t.Fatalf("placing the compiler: %v", werr)
		}
	}
	return dst
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
	bin := linkedCompiler(t)

	aliases := t.TempDir()
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
	bin := linkedCompiler(t)

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
	dir := t.TempDir()
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

	bin := linkedCompiler(t)
	cold := filepath.Join(t.TempDir(), "cold-home")
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

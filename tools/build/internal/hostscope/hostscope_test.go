package hostscope

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// childVerb is the first argument that makes this test binary a holder process
// rather than a test run: `<test binary> hostscope-hold <dir> <test|peer>`. The
// exclusion's whole subject is two processes — a runner and the verify it
// spawns, or two checkouts' verifies — and flock is held per open file
// description, so only a second process is the case the mechanism must survive.
const childVerb = "hostscope-hold"

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == childVerb {
		os.Exit(holdAsChild(os.Args[2], os.Args[3]))
	}
	os.Exit(m.Run())
}

// holdAsChild takes the exclusion in dir as the named arena, says "held" once it
// has it, and keeps it until its stdin closes or it is killed.
func holdAsChild(dir, which string) int {
	machineDir = func() (string, bool) { return dir, true }
	arena := testArena()
	if which == "peer" {
		arena = peerArena()
	}
	release, _, err := Acquire(context.Background(), arena)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: Acquire: %v\n", err)
		return 1
	}
	defer release()
	fmt.Println("held")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// useTempDir points the exclusion at a directory this test owns. Every test
// takes it: the machine's real exclusion is held by real runs, and a test that
// took it would block every arena on the machine for as long as it ran.
func useTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := machineDir
	machineDir = func() (string, bool) { return dir, true }
	t.Cleanup(func() { machineDir = prev })
	return dir
}

func requireFlock(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin", "dragonfly", "freebsd", "illumos", "linux", "netbsd", "openbsd", "windows":
	default:
		t.Skip("no advisory file lock is reachable from the standard library here")
	}
}

func testArena() ArenaIdentity {
	return ArenaIdentity{Host: "build01", Id: "a-00000000000000000000000000000001"}
}

// peerArena is a SECOND arena on the same host — another checkout on the same
// machine, which is the party the exclusion exists to keep out.
func peerArena() ArenaIdentity {
	return ArenaIdentity{Host: "build01", Id: "a-00000000000000000000000000000002"}
}

// stillHeld reports whether a peer is refused within a short bound — the
// exclusion is held by somebody.
func stillHeld(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	release, _, err := Acquire(ctx, peerArena())
	if err == nil {
		release()
		return false
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probing the exclusion failed for a reason other than it being held: %v", err)
	}
	return true
}

// An uncontended acquire reports EXACTLY no wait, and names its holder.
func TestAcquire_UncontendedReportsExactlyNoWaitAndNamesTheHolder(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	release, waited, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	if waited != 0 {
		t.Errorf("waited = %s on a free exclusion, want exactly 0", waited)
	}
	if got, ok := Holder(); !ok || got != testArena() {
		t.Errorf("Holder() = %v, %v; want %v", got, ok, testArena())
	}
}

// The runner's case: its arena holds the exclusion and the verify it spawned
// joins it. The nested acquire is granted at once, and its release does not
// hand the machine to a peer while the outer acquisition is still measuring.
func TestAcquire_TheSameArenaIsAlreadyInsideIt(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	outer, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("outer Acquire: %v", err)
	}
	defer outer()

	// Bounded, because the regression is a deadlock: without re-entrancy this
	// queues behind the acquire above and never returns.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nested, waited, err := Acquire(ctx, testArena())
	if err != nil {
		t.Fatalf("nested Acquire: %v — an arena cannot enter the exclusion it already holds", err)
	}
	if waited != 0 {
		t.Errorf("waited = %s entering an exclusion this arena already holds", waited)
	}
	nested()

	if !stillHeld(t) {
		t.Fatal("a nested release freed the exclusion its outer acquire still holds")
	}
}

// ANOTHER PROCESS HOLDS IT UNTIL IT DIES. The runner's case across the process
// boundary it really crosses: a holder process in this arena lets a second
// process of the same arena straight in (the verify a runner spawns), keeps a
// peer arena out, and — killed, which is how a wedged verify ends — gives the
// exclusion back by dying, because the kernel releases it. A lock the holder had
// to remove itself would disable the machine after every crash.
func TestAcquire_AHolderProcessKeepsPeersOutUntilItDies(t *testing.T) {
	requireFlock(t)
	dir := useTempDir(t)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, childVerb, dir, "test")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	reap := func() {
		if !reaped {
			reaped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	defer reap()

	// The child prints only once it holds the exclusion, and the read returns
	// the moment it does — or at EOF, if it died trying.
	if line, err := bufio.NewReader(stdout).ReadString('\n'); line != "held\n" {
		reap()
		t.Fatalf("the holder process never took the exclusion (read %q, %v):\n%s", line, err, stderr.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	nested, waited, err := Acquire(ctx, testArena())
	cancel()
	if err != nil {
		t.Fatalf("a process in the holding process's arena could not enter: %v", err)
	}
	if waited != 0 {
		t.Errorf("waited = %s entering an exclusion this arena already holds", waited)
	}
	nested()

	if !stillHeld(t) {
		t.Fatal("a peer was granted the exclusion while another process held it")
	}

	reap()
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	after, _, err := Acquire(ctx, peerArena())
	if err != nil {
		t.Fatalf("Acquire after the holder process was killed: %v — a dead process still holds it", err)
	}
	after()
}

// A peer arena queues: it is announced as queued before it blocks, is granted
// when the holder releases, and reports the time it spent waiting.
func TestAcquire_APeerQueuesAndIsGrantedOnRelease(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	holder, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}

	queued := make(chan Scope, 1)
	dequeued := make(chan struct{}, 1)
	ctx := OnQueue(context.Background(), func(s Scope) func() {
		queued <- s
		return func() { dequeued <- struct{}{} }
	})
	type result struct {
		release func()
		waited  time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		release, waited, err := Acquire(ctx, peerArena())
		done <- result{release, waited, err}
	}()

	select {
	case s := <-queued:
		if s != ScopeHost {
			t.Errorf("queued for %q, want %q", s, ScopeHost)
		}
	case r := <-done:
		holder()
		if r.err == nil {
			r.release()
		}
		t.Fatalf("a peer was answered while another arena held the exclusion: %v", r.err)
	case <-time.After(10 * time.Second):
		holder()
		t.Fatal("the peer never announced that it was queued")
	}

	holder()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the peer was not granted the exclusion after the release: %v", r.err)
		}
		defer r.release()
		if r.waited <= 0 {
			t.Errorf("waited = %s, but the peer was held up", r.waited)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the peer was not granted the exclusion within 10s of the release")
	}
	select {
	case <-dequeued:
	default:
		t.Error("the queue announcement was never ended")
	}
}

// A peer that gives up waiting returns its context's error, so a caller can
// tell "the deadline passed" from "the exclusion could not be taken at all" —
// and leaves nothing held. The flock request it abandoned is still outstanding
// and is granted once the holder lets go; a grant nobody unlocked would wedge
// every arena on the machine behind a caller that had already left.
func TestAcquire_APeerGivesUpAtItsDeadline(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	holder, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	release, waited, err := Acquire(ctx, peerArena())
	if err == nil {
		release()
		holder()
		t.Fatal("a peer was granted an exclusion another arena held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if release != nil {
		t.Error("a failed Acquire returned a release — a caller that defers it would unlock something it never held")
	}
	if waited <= 0 {
		t.Errorf("waited = %s, but the peer queued until its deadline", waited)
	}

	holder()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	next, _, err := Acquire(ctx2, testArena())
	if err != nil {
		t.Fatalf("Acquire after an abandoned wait: %v — the abandoned request kept the exclusion", err)
	}
	next()
}

// held names the holder as part of the acquire, so a holder that cannot write
// its name gives the exclusion back rather than keeping a lock its own arena
// could never recognise and re-enter. A read-only descriptor reaches that state
// portably: the lock is granted on it, and the truncate that clears the previous
// record is refused.
func TestHeld_AnExclusionItCannotNameIsGivenBack(t *testing.T) {
	requireFlock(t)
	dir := useTempDir(t)

	path := filepath.Join(dir, ScopeHost.file())
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if granted, err := tryLockExclusive(f); err != nil || !granted {
		f.Close()
		t.Fatalf("tryLockExclusive on a free exclusion = (%v, %v), want granted", granted, err)
	}

	release, err := held(f, testArena())
	if err == nil {
		release()
		t.Fatal("held reported an exclusion it could not name a holder for")
	}
	if release != nil {
		t.Error("a refused acquire returned a release")
	}
	if !strings.Contains(err.Error(), "holder") {
		t.Errorf("err = %v, want it to name what it could not record", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next, _, err := Acquire(ctx, peerArena())
	if err != nil {
		t.Fatalf("Acquire after a refused one: %v — the refusal left the exclusion held", err)
	}
	next()
}

// THE RECORD IS READABLE WHILE THE LOCK IS HELD, through any handle but the
// holder's. Every reader of it reads at exactly that moment — the refused party
// deciding re-entry, and Holder — so a lock that covered the record would leave
// both reading nobody. On Windows a byte-range lock refuses reads of the bytes
// it covers to every other handle, even one in the same process, which is how a
// lock on byte 0 made a nested party queue behind its own arena (flow#800).
func TestAcquire_TheRecordIsReadableThroughAnotherHandleWhileHeld(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	release, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	path, _ := Path()
	other, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open a second handle on the held exclusion: %v", err)
	}
	defer other.Close()
	if got, ok := readHolder(other); !ok || got != testArena() {
		t.Errorf("readHolder through a second handle = (%+v, %v), want (%+v, true) — a nested party would queue behind its own arena", got, ok, testArena())
	}
	if got, ok := Holder(); !ok || got != testArena() {
		t.Errorf("Holder while held = (%+v, %v), want (%+v, true)", got, ok, testArena())
	}
}

// UNLOCKING GIVES THE LOCK BACK WITH THE HANDLE STILL OPEN. Every release path
// unlocks and then closes, and the close alone would free it — so an unlock that
// missed, its error discarded at every call site, would pass every other case
// here. On Windows that is an easy miss to make: UnlockFileEx frees only the
// exact range LockFileEx took, and an unlock naming any other fails.
func TestUnlock_FreesTheLockWhileTheHandleStaysOpen(t *testing.T) {
	requireFlock(t)
	path := filepath.Join(t.TempDir(), "unlock.lock")

	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	other, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	if granted, err := tryLockExclusive(holder); err != nil || !granted {
		t.Fatalf("tryLockExclusive on a free lock = (%v, %v), want granted", granted, err)
	}
	// The precondition that makes the last assertion mean anything.
	if granted, err := tryLockExclusive(other); err != nil || granted {
		t.Fatalf("a second handle's tryLockExclusive while held = (%v, %v), want refused", granted, err)
	}
	if err := unlock(holder); err != nil {
		t.Fatalf("unlock: %v — it does not name the range the lock took", err)
	}
	if granted, err := tryLockExclusive(other); err != nil || !granted {
		t.Fatalf("a second handle's tryLockExclusive after unlock = (%v, %v), want granted — the lock outlived its unlock", granted, err)
	}
	_ = unlock(other)
}

// A caller that cannot name itself re-enters nothing — not even an exclusion
// held by another caller that could not either.
func TestAcquire_AnUnnamedCallerIsNeverAlreadyInside(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	outer, _, err := Acquire(context.Background(), ArenaIdentity{})
	if err != nil {
		t.Fatalf("outer Acquire: %v", err)
	}
	defer outer()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	release, _, err := Acquire(ctx, ArenaIdentity{})
	if err == nil {
		release()
		t.Fatal("an unnamed caller was treated as already inside an unnamed holder's exclusion")
	}
}

// A record that cannot be read names nobody, and nobody is never the caller's
// own arena: a torn record costs a wait, never a walk-in.
func TestAcquire_AnUnreadableRecordQueues(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	outer, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("outer Acquire: %v", err)
	}
	defer outer()
	// Torn through a second handle, as a reader would meet it: the record's
	// opening bytes overwritten, the file left in place.
	path, _ := Path()
	other, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.WriteAt([]byte("!!"), 0); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	release, _, err := Acquire(ctx, testArena())
	if err == nil {
		release()
		t.Fatal("a torn record was read as this arena's own")
	}
}

// Release blanks the record before it unlocks, so the next acquirer never reads
// a name that has moved on; and it is idempotent, because every caller defers it.
func TestRelease_ClearsTheRecordAndIsIdempotent(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	release, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	release()

	path, _ := Path()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("the record survived its release: %q", b)
	}
	if got, ok := Holder(); ok {
		t.Errorf("Holder() = %v after release, want nobody", got)
	}
	if stillHeld(t) {
		t.Error("the exclusion is still held after its release")
	}
}

// A machine with nowhere to put the lock refuses: a silent free pass would run
// everything beside everything else and say nothing.
func TestAcquire_NoMachineDirectoryRefuses(t *testing.T) {
	prev := machineDir
	machineDir = func() (string, bool) { return "", false }
	t.Cleanup(func() { machineDir = prev })

	release, _, err := Acquire(context.Background(), testArena())
	if err == nil {
		release()
		t.Fatal("Acquire succeeded with no directory to hold the exclusion in")
	}
}

// The exclusion lives where flow keeps it: host-scope.lock in flow's directory
// under the user cache directory. A different path is a different exclusion.
func TestPath_IsFlowsMachineDirectory(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("this host has no user cache directory: %v", err)
	}
	path, ok := Path()
	if !ok {
		t.Fatal("Path() reports no directory on a host that has a user cache directory")
	}
	if want := filepath.Join(cache, "flow", "host-scope.lock"); path != want {
		t.Errorf("Path() = %q, want %q", path, want)
	}
}

// THE FORMAT PIN. The record is read by flow's runner, and flow's records are
// read here: the field names and nesting are the contract, so they are pinned
// against a literal rather than against this package's own struct.
func TestRecord_IsFlowsShape(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	release, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	path, _ := Path()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Arena struct {
			Host string `json:"host"`
			Id   string `json:"id"`
		} `json:"arena"`
		PID   int       `json:"pid"`
		Since time.Time `json:"since"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("the record does not parse as flow's: %v\n%s", err, b)
	}
	if rec.Arena.Host != "build01" || rec.Arena.Id != string(testArena().Id) || rec.PID != os.Getpid() || rec.Since.IsZero() {
		t.Errorf("record = %+v, want arena %v, this pid and an instant", rec, testArena())
	}
	release()

	// What flow's runner writes, byte for byte as flow renders it.
	flowWritten := []byte(`{"arena":{"host":"build01","id":"a-00000000000000000000000000000001"},"pid":4242,"since":"2026-10-02T12:00:00Z"}`)
	if got, ok := parseHolder(flowWritten); !ok || got != testArena() {
		t.Errorf("parseHolder(flow's record) = %v, %v; want %v", got, ok, testArena())
	}
	// An empty arena names nobody, whatever else the record carries.
	if got, ok := parseHolder([]byte(`{"arena":{"host":"","id":""},"pid":1}`)); ok {
		t.Errorf("parseHolder(empty arena) = %v, want nobody", got)
	}
}

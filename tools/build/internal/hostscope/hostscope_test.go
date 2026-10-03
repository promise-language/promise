package hostscope

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

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
// tell "the deadline passed" from "the exclusion could not be taken at all".
func TestAcquire_APeerGivesUpAtItsDeadline(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	holder, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer holder()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	release, waited, err := Acquire(ctx, peerArena())
	if err == nil {
		release()
		t.Fatal("a peer was granted an exclusion another arena held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if waited <= 0 {
		t.Errorf("waited = %s, but the peer queued until its deadline", waited)
	}
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

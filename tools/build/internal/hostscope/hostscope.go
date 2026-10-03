// Package hostscope is a copy of the half of flow's pkg/hostscope that TAKES
// the host-scope exclusion, taken because this project may not depend on flow
// as a Go module (internal/verifiedtree says why: both modules are private, and
// importing one broke ./make on every machine without the maintainer's
// credentials, T2240/T2241).
//
// WHY THE COPY EXISTS. The host-scope exclusion is the orchestrator's: one slot
// per machine, held by an arena, re-entrant to the arena that holds it, and
// joined by every party that runs a declared command — the flow's own runner and
// a person at a terminal alike (flow docs/gates-and-commands.md § Two scopes).
// bin/verify declares it ("serialize": "host" on its `bin/run --list --json`
// row), so a runner takes it before spawning verify and reports the queue as
// waiting, outside the run's allowance. Verify must then take THE SAME exclusion
// rather than a lock of its own: a second lock is one the runner's re-entrancy
// cannot reach, and a runner's verify would queue on it inside its bound — which
// is the defect this replaced (#96). Where the orchestrator keeps the exclusion
// is the orchestrator's business, not a project tool writing shared state
// (docs/build-tools.md § Test Sandboxing).
//
// WHY IT IS COPIED RATHER THAN REWRITTEN. The exclusion has several parties and
// they meet only if every one of them agrees on the path, the lock primitive and
// the byte range it locks, the holder record's format, and the rule a refused
// party re-enters by. A paraphrase that differed in any of them is an exclusion
// of its own: two parties each believing they hold the machine, or a nested
// verify queueing behind the runner that spawned it until its allowance kills
// it. So this is flow's code, unchanged except as listed below.
//
// WHAT IS COPIED, from github.com/promise-language/flow at 283fc72:
//   - pkg/hostscope/hostscope.go — Acquire, acquireAt, held, writeHolder,
//     readHolder, parseHolder, parseRecord, holderRecord, maxHolderRecord,
//     Holder, Path;
//   - pkg/hostscope/scope.go — Scope, ScopeHost, file, OnQueue, Queued;
//   - pkg/hostscope/lock_flock.go, lock_windows.go, lock_noflock.go — verbatim;
//   - pkg/machinecache.Dir — the body of machineDir;
//   - identity.go — identity.go in this package.
//
// THE MECHANICAL CHANGES, AND NO OTHERS: flow's types (ArenaIdentity, HostId,
// ArenaId) become this package's; the scope set is reduced to ScopeHost, so
// AcquireScope and ScopePath fold into Acquire and Path, and ScopeMemory, Held
// and Break are not copied; machinecache.Dir is inlined as machineDir.
//
// IF YOU CHANGE ANYTHING HERE, YOU HAVE FORKED THE EXCLUSION. The other parties
// are in another repository and will not change with you.
package hostscope

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Scope names one machine-wide exclusion. Flow declares a closed set of them;
// this copy carries the one a project command joins.
type Scope string

// ScopeHost is the host-scope exclusion: the one slot a measurement too heavy
// to run beside another holds while it runs (flow docs/gates-and-commands.md
// § Two scopes).
const ScopeHost Scope = "host-scope"

// file is the scope's lock file name. Derived from the scope and never
// supplied, so there is exactly one spelling of each.
func (s Scope) file() string { return string(s) + ".lock" }

// maxHolderRecord bounds a read of the holder record. The record is one arena,
// a pid and an instant, and the arena's larger half is a filesystem path — so
// this is generous by two orders of magnitude and still refuses to size a
// buffer from bytes on disk.
const maxHolderRecord = 64 << 10

// holderRecord is written into the locked file while the exclusion is held: it
// is how an operator looking at a stalled machine sees which arena has it, and
// it is what a nested party checks to find that the exclusion is its own
// arena's already.
//
// THE SECOND USE IS WHY THE WRITE IS PART OF THE ACQUIRE. It is read only by a
// party the kernel has just refused, so whoever it names is whoever holds the
// lock — and a holder that could not write its name would be one its own tools
// could not recognise, which is a deadlock rather than a missing diagnostic.
// The PID and the instant are diagnostic; the arena is not.
type holderRecord struct {
	Arena ArenaIdentity `json:"arena"`
	PID   int           `json:"pid"`
	Since time.Time     `json:"since"`
}

// machineDir is where flow keeps its per-machine records — flow's directory
// under the OS user cache directory (flow pkg/machinecache.Dir) — and the seam
// this package is tested through: a package var, deliberately NOT an
// environment variable.
//
// A test that took the real exclusion would not merely mislead itself: it would
// block every other arena on the developer's machine for as long as it ran, and
// serialize against whatever real gate happened to be measuring.
var machineDir = func() (string, bool) {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Join(dir, "flow"), true
}

// Path reports where the exclusion lives, and false when this machine has no
// directory for it.
func Path() (string, bool) {
	dir, ok := machineDir()
	if !ok {
		return "", false
	}
	return filepath.Join(dir, ScopeHost.file()), true
}

// Acquire blocks until this process holds the host's exclusion, and reports how
// long that took. A zero wait is an exclusion that was free.
//
// holder is the arena taking it — (HostId, ArenaId), never a checkout path. A
// path can say which directory is busy and cannot say which of a host's arenas
// holds the machine, which is the question an operator looking at a stalled
// queue is asking. It is also what the exclusion is re-entered on, below, so it
// is load-bearing rather than only diagnostic.
//
// IT IS RE-ENTRANT TO THE ARENA THAT HOLDS IT. A caller whose own arena is
// already inside the exclusion is granted it at once, with no wait, and gets a
// release that does nothing — the acquisition it is nested inside is what owns
// the machine. Every party in one arena takes it, and the nested ones do not
// queue behind each other; see the block at the contended branch for why the
// alternative is a deadlock rather than a slow run.
//
// IT REFUSES RATHER THAN DEGRADING. A machine with nowhere to put the lock, or
// a platform with no way to hold one, gets an error and no exclusion — never a
// silent free pass. That distinction is the whole point: a caller that proceeds
// unserialized produces measurements that are wrong in a way reproducing
// nowhere, and no report mentions it. Its caller's obligation is the other half
// — a party that cannot take the exclusion does not run the measurement.
//
// THE WAIT IS BOUNDED BY ctx AND BY NOTHING ELSE. No TTL, no cap of its own:
// every holder's run is bounded by the treasurer's allowance for it and released by its
// own death, so the queue is finite by construction. A cap set below what a
// real run costs would turn every busy period into false failures, which is the
// failure gates-and-commands.md § Gates may have to wait names.
//
// A QUEUE IS ANNOUNCED BEFORE IT IS SAT IN. A context carrying a notice
// (OnQueue) is told the moment this call is about to block behind another
// arena, and told again when the block ends — so a run that is alive and
// queued can say so while it waits, rather than only in the figure returned
// after. A free exclusion and a re-entered one announce nothing.
//
// The returned release is idempotent and safe to defer.
func Acquire(ctx context.Context, holder ArenaIdentity) (release func(), waited time.Duration, err error) {
	path, ok := Path()
	if !ok {
		return nil, 0, fmt.Errorf("hostscope: this machine has no directory for the %s exclusion, so it cannot be taken", ScopeHost)
	}
	return acquireAt(ctx, ScopeHost, path, holder)
}

// acquireAt is the acquisition itself, against one already-named lock file. It
// is where every property Acquire documents is implemented, and it is shared by
// every scope so that no member of the set can drift into its own semantics.
func acquireAt(ctx context.Context, s Scope, path string, holder ArenaIdentity) (release func(), waited time.Duration, err error) {
	// 0o700 for the directory and 0o600 for the file: what an arena is named
	// here carries a machine name, which nothing else on the machine has a
	// reason to read. It never leaves the machine — docs/disclosure.md guards
	// outward bytes, and this is why the claim's published half is the arena
	// id alone while this one carries the host too.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, 0, fmt.Errorf("hostscope: cannot create the directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("hostscope: cannot open %s: %w", path, err)
	}

	// ZERO MEANS UNCONTENDED, NOT "TOO FAST TO SEE". The lock is asked for
	// without blocking first, so a run that queued for nothing reports nothing —
	// rather than the microseconds a clock around the syscall would report,
	// which the ledger would then carry as contention and pay an orchestrator
	// write for on every single run.
	granted, err := tryLockExclusive(f)
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if granted {
		release, err := held(f, holder)
		return release, 0, err
	}

	// CONTENDED — BUT BY WHOM? An exclusion this arena already holds is one
	// this caller is already inside, and a lock is re-entrant to the party that
	// holds it. Without this the participant set would be a deadlock rather
	// than a queue: the runner takes the exclusion and then spawns the project's
	// gate entry point, which is bound by the same rule and would queue behind
	// its own parent until the gate's timeout killed it — so every gate a
	// project declared host-scoped would report a timeout, and only those.
	//
	// THE PARTY IS THE ARENA, which is the granularity the holder is recorded
	// at, and it is the one the norm can state: the parties inside one arena
	// are one measurement by construction, because a claim is item ↔ arena.
	// What it gives up is a person running a gate by hand inside a checkout a
	// flow is currently measuring in — which is an operator reaching into a live
	// arena, a thing no exclusion was going to make safe. An operator with their
	// own checkout is their own arena and queues like anybody else.
	//
	// An empty holder can never match: Arena.Empty() is not an identity, and
	// comparing one would let a caller that could not name itself re-enter
	// anything that also could not.
	//
	// THE ONE READING THIS CANNOT RULE OUT is a name left behind by a holder the
	// kernel reaped: the lock is free at that instant, so the next acquirer is
	// granted it and blanks the record — but between its grant and that syscall
	// the file still names the arena that died. A third party from THAT arena,
	// asking inside that window, would read its own name and walk in. It is a
	// crash, an immediate peer acquire and a same-arena acquire aligning inside
	// one ftruncate, and closing it would take a lock-and-truncate the kernel
	// does not offer. Writing the name is therefore the first thing a holder
	// does, so the window is as narrow as the syscall.
	if ours, ok := readHolder(f); ok && !holder.Empty() && ours == holder {
		f.Close()
		// A no-op release. The acquisition this call is nested inside owns the
		// exclusion, and releasing it here would hand the machine away while
		// the outer measurement is still running.
		return func() {}, 0, nil
	}

	// A real queue starts here and nowhere above: announced now, so whoever
	// keeps this run's liveness record can say it is queued while it is, and
	// ended the moment the kernel answers, on every path — granted, refused or
	// given up.
	dequeued := Queued(ctx, s)
	started := time.Now()
	err = waitLockExclusive(ctx, f)
	dequeued()
	if err != nil {
		// waitLockExclusive owns f from here: the blocking lock may still be
		// outstanding and only it can know when to close.
		return nil, time.Since(started), err
	}
	release, err = held(f, holder)
	if err != nil {
		return nil, time.Since(started), err
	}
	return release, time.Since(started), nil
}

// held names the holder in the now-locked f and returns the release.
//
// THE NAME IS PART OF THE ACQUIRE, NOT A DIAGNOSTIC BESIDE IT, which is why a
// failure here gives the exclusion back rather than proceeding without it. The
// re-entrancy check above decides on this record: a holder that took the lock
// and could not say whose it is leaves its own tools unable to recognise it,
// and they would queue behind their own arena until their timeout killed them.
// Refusing costs one run and says why; writing nothing costs every nested run
// on the machine and says nothing.
func held(f *os.File, holder ArenaIdentity) (func(), error) {
	if err := writeHolder(f, holder); err != nil {
		_ = unlock(f)
		_ = f.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Clear the holder before unlocking, so the next acquirer never
			// reads a name that has moved on — which at this point is not
			// cosmetic: a stale name is a name another party could mistake for
			// its own and re-enter on.
			_ = f.Truncate(0)
			_ = unlock(f)
			_ = f.Close()
		})
	}, nil
}

// writeHolder records who holds it.
func writeHolder(f *os.File, holder ArenaIdentity) error {
	b, err := json.Marshal(holderRecord{Arena: holder, PID: os.Getpid(), Since: time.Now()})
	if err != nil {
		return fmt.Errorf("hostscope: cannot render the holder of %s: %w", f.Name(), err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("hostscope: cannot clear the previous holder of %s: %w", f.Name(), err)
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		return fmt.Errorf("hostscope: cannot record the holder of %s: %w", f.Name(), err)
	}
	return nil
}

// readHolder reads the record out of an already-open exclusion file.
//
// A record that is absent, torn or unparseable reads as NOBODY, and nobody is
// never equal to a caller's own arena — so every way of failing to read this
// lands on "queue", which is the answer that is wrong at worst by a wait.
func readHolder(f *os.File) (ArenaIdentity, bool) {
	b := make([]byte, maxHolderRecord)
	n, err := f.ReadAt(b, 0)
	if n == 0 && err != nil {
		return ArenaIdentity{}, false
	}
	return parseHolder(b[:n])
}

func parseHolder(b []byte) (ArenaIdentity, bool) {
	rec, ok := parseRecord(b)
	if !ok {
		return ArenaIdentity{}, false
	}
	return rec.Arena, true
}

// parseRecord is the whole record, for the reporting half that needs the
// process and the instant beside the arena. parseHolder is this function's
// arena, so the two cannot disagree about what a record says or about which
// records are readable at all.
func parseRecord(b []byte) (holderRecord, bool) {
	var rec holderRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return holderRecord{}, false
	}
	if rec.Arena.Empty() {
		return holderRecord{}, false
	}
	return rec, true
}

// Holder reports the arena currently named in the exclusion's file, and false
// when nothing is named there.
//
// A READING, NOT A CHECK. It is what an operator asks to find out who has the
// machine; it establishes nothing about whether the exclusion is held now, and
// no caller may act on it — only taking the lock establishes that. That is what
// separates it from the read inside Acquire, which is made by a caller the
// kernel has just refused and so is already standing on the fact this one
// cannot supply. The file is
// blanked on release, so an empty read is an exclusion nobody holds and a
// non-empty one may be either a live holder or a name the kernel has already
// let go of.
func Holder() (ArenaIdentity, bool) {
	path, ok := Path()
	if !ok {
		return ArenaIdentity{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ArenaIdentity{}, false
	}
	return parseHolder(b)
}

// queueNoticeKey carries the notice OnQueue attaches.
type queueNoticeKey struct{}

// OnQueue returns a context carrying queued, which an acquisition on it calls
// when — and only when — it is about to block behind another party, naming the
// scope it is queued for; the call it returns is made when the queue ends,
// however it ends.
//
// IT EXISTS SO THE PARTY ABOUT TO WAIT CAN SAY SO BEFORE IT DOES. The figure
// an acquisition returns is what the ledger needs, and it arrives after the
// wait — which is exactly not what a liveness record needs: a run twenty
// minutes into a queue behind a peer's suite looks, from another terminal, like
// a dispatched step and silence, the shape of a wedged run. The caller that
// keeps that record is not the one that acquires, so the notice travels on the
// context, which every party between them already passes along without having
// to know what it carries.
//
// An exclusion that was free, or that the caller's own arena already holds,
// queues for nothing and says nothing.
func OnQueue(ctx context.Context, queued func(Scope) (dequeued func())) context.Context {
	return context.WithValue(ctx, queueNoticeKey{}, queued)
}

// Queued announces, on ctx, that the caller is about to block for s, and
// returns the call that ends the announcement. It does nothing on a context
// that carries no notice, and the call it returns is idempotent.
func Queued(ctx context.Context, s Scope) (dequeued func()) {
	queued, ok := ctx.Value(queueNoticeKey{}).(func(Scope) func())
	if !ok || queued == nil {
		return func() {}
	}
	end := queued(s)
	var once sync.Once
	return func() {
		once.Do(func() {
			if end != nil {
				end()
			}
		})
	}
}

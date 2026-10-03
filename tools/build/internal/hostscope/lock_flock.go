//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package hostscope

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// THE KERNEL IS WHAT RELEASES IT. That is the reason this is flock and not a
// lockfile with a timestamp: docs/gates-and-commands.md § Two scopes requires
// that a process which dies releases the exclusion, and a holder that has
// crashed, been killed, or gone quiet is exactly the holder that cannot keep a
// promise to release. An advisory lock on an open descriptor is released when
// the descriptor closes, which the kernel does for every process it reaps — so
// the release is an authority independent of the holder, which is what the rule
// asks for and what no amount of care inside the holder could provide.
//
// It is also why there is no TTL and no stale-breaking. A lockfile mechanism
// has to guess when a holder has died, and every guess is wrong in one of two
// expensive directions: too short breaks a legitimate forty-minute suite and
// hands the machine to a second one, too long disables the machine after a
// crash. The kernel does not guess.

// tryLockExclusive asks for f's advisory lock without blocking, and reports
// whether it was granted.
//
// ASKING WITHOUT BLOCKING FIRST IS NOT AN OPTIMISATION — it buys two things
// nothing else can. An uncontended acquire is KNOWN to be uncontended rather
// than measured as a very short wait, and the caller files that figure to the
// ledger: a microsecond charged as contention on every run would put a write on
// the orchestrator for each one and report queueing on a machine where nothing
// queued. A clock cannot tell those apart; this syscall can. And a refusal here
// is the caller's one chance to look at WHO holds it before committing to a
// queue, which is what makes re-entrancy possible at all — a blocking call
// would already be behind its own arena by the time anyone could ask.
//
// It never closes f. The caller owns it on every path out of here.
func tryLockExclusive(f *os.File) (granted bool, err error) {
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, fmt.Errorf("hostscope: cannot take the host-scope exclusion on %s: %w", f.Name(), err)
	}
}

// waitLockExclusive blocks until f's advisory lock is granted or ctx ends.
//
// ON EVERY ERROR PATH THIS FUNCTION OWNS f. The flock syscall has no deadline,
// so a caller that gives up leaves a lock request outstanding that will be
// granted later whether anyone wants it or not — and a grant nobody unlocks is
// the machine wedged. The cleanup goroutine below is the only party that can
// know when that grant lands, so it takes the descriptor with it: it unlocks
// whatever it was given and closes. The caller must not touch f after an error.
func waitLockExclusive(ctx context.Context, f *os.File) error {
	fd := int(f.Fd())

	// Buffered: the cleanup path may not be reading when the grant lands, and
	// an unbuffered send would park this goroutine for the life of the process.
	granted := make(chan error, 1)
	go func() { granted <- syscall.Flock(fd, syscall.LOCK_EX) }()

	select {
	case err := <-granted:
		if err != nil {
			f.Close()
			return fmt.Errorf("hostscope: cannot take the host-scope exclusion on %s: %w", f.Name(), err)
		}
		return nil
	case <-ctx.Done():
		go func() {
			if err := <-granted; err == nil {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
			}
			f.Close()
		}()
		return fmt.Errorf("hostscope: gave up waiting for the host-scope exclusion on %s: %w", f.Name(), ctx.Err())
	}
}

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

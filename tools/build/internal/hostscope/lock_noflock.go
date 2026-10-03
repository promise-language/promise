//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || windows)

package hostscope

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

// The lock has no implementation here. Plan9, the wasm ports, solaris
// and aix either have no advisory lock in the standard library or spell it
// through a call `syscall` does not export, and reaching them would cost a
// dependency paid for by every machine that builds this.
//
// IT REFUSES RATHER THAN BEING ABSENT, and that is the whole point of the file:
// with no definition at all this package would not compile on those platforms,
// so nothing that imports it could be built there either — which would cost
// them the SDK, not just host-scoped gates.
//
// IT REFUSES RATHER THAN REPORTING A GRANT, which is the tempting reading and
// the expensive one. A granted lock said by something that locked nothing means
// every heavy gate on the machine runs beside every other, and
// docs/gates-and-commands.md § Two scopes forbids exactly that — a party that
// cannot take the exclusion does not run the measurement, it refuses and names
// what it could not take. A refusal costs those platforms the gates a project
// declared host-scoped and nothing else.
//
// tryLockExclusive is the one that has to refuse rather than report "not
// granted": a false with no error reads as "someone else holds it", which would
// send the caller to waitLockExclusive and turn an unsupported platform into a
// queue that never moves.
func tryLockExclusive(_ *os.File) (granted bool, err error) {
	return false, unsupported()
}

// waitLockExclusive is unreachable in practice — tryLockExclusive refuses
// before a caller can reach it — and refuses anyway rather than blocking, so a
// future path that got here waits for nothing.
func waitLockExclusive(_ context.Context, f *os.File) error {
	f.Close()
	return unsupported()
}

func unsupported() error {
	return fmt.Errorf("hostscope: the host-scope exclusion cannot be held on %s/%s — no advisory file lock is reachable from the standard library there",
		runtime.GOOS, runtime.GOARCH)
}

// unlock is unreachable: nothing on these platforms ever holds the exclusion,
// because lockExclusive refuses before a caller can. It exists so the package
// compiles, and it returns the same refusal rather than nil so that a future
// caller that found a way around the acquire path is not told it released
// something.
func unlock(_ *os.File) error {
	return fmt.Errorf("hostscope: the host-scope exclusion cannot be held on %s/%s, so nothing here holds one to release",
		runtime.GOOS, runtime.GOARCH)
}

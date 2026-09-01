# File I/O

> **Tag:** `io` — remaining work to complete this document: `mcp__tracker__list --tag io`

> The contract for `modules/io`: what a Promise program may *rely on* when it reads and writes files,
> across POSIX and Windows. Most of that contract is unremarkable and is covered by the API
> documentation itself. This document specifies the parts that are not — the operations whose
> correctness depends on platform guarantees a caller cannot see from the call site.

Today that means three: **atomic replace**, **forcing data to stable storage**, and **advisory
locking**. Buffered reading and writing, directory listing, and path handling carry no such hidden
guarantees; they are inventoried in [standard-library.md](standard-library.md) and
[platform-modules.md](platform-modules.md), and their behavior is the obvious one.

---

## 1. Why these three need a contract

Writing a file correctly is not `open` → `write` → `close`. A process that dies partway through that
sequence leaves a *truncated* file where the previous version used to be — the old contents are gone
and the new ones are incomplete. Any store that must survive a crash writes to a temporary file,
forces it to stable storage, and then atomically swaps it into place.

Each of those three steps has a platform-specific failure mode that is invisible at the call site:

- **`rename` is atomic, but only within one filesystem**, and it makes the *directory entry* durable,
  not the file — so a crash after a successful rename can still lose the swap.
- **`fsync` does not mean "on the platter" on macOS.** It returns once the data reaches the drive,
  which may hold it in a volatile write cache.
- **Advisory locks have three different ownership models on POSIX alone**, and one of them silently
  releases the lock when an unrelated part of the program closes any descriptor for the file.

The rule this document exists to enforce: **a caller writing ordinary Promise code gets durability by
default, and never has to know which of those traps applies to their platform.**

---

## 2. The public surface

```promise
// The composite — the durable write, done correctly.
File.replace_content!(string path, string content)  `global `public
File.replace_bytes!(string path, u8[] content)      `global `public

// Primitives, for callers building their own sequence.
File.rename!(string from, string to)                `global `public
File.sync!(~this)
Dir.sync!(string path)                              `global `public

// Advisory locking.
File.lock!(~this)                                  // blocking, exclusive
File.lock_shared!(~this)                           // blocking, shared
File.try_lock!(~this) bool                         // never blocks; false = held elsewhere
File.try_lock_for!(~this, Duration timeout) bool   // bounded wait; false = timed out (§5.7)
File.unlock!(~this)
```

`replace_content` is the one obvious way. The primitives exist because a caller streaming a large
file cannot buffer it into a `string` first, and must be able to assemble the same sequence by hand.
`Dir.sync!` is public **because** `File.rename!` is: a caller using the primitives without it would
believe they had built a durable write and be wrong (§3.2).

---

## 3. Atomic replace

### 3.1 What `replace_content` does

1. Create a temporary file **in the destination's own directory** (§3.3).
2. Take an exclusive lock on it (§5), truncate it, and write the content.
3. `sync` the temporary file — the *contents* are now durable.
4. `rename` the temporary over the destination — the swap is atomic.
5. `sync` the destination's directory — the *swap* is now durable.
6. Close, releasing the lock.

A reader concurrently opening the destination sees either the complete old file or the complete new
one. There is no window in which it sees a partial write, and no crash point that leaves a truncated
destination.

### 3.2 Why step 5 is not optional

`rename` modifies a directory entry. Syncing the *file* in step 3 says nothing about whether that
directory entry reached stable storage. Without step 5, a power loss immediately after a successful
`rename` can leave the directory still pointing at the old inode — or, on some filesystems, at
neither. The file's contents are durable and the swap is not.

On POSIX this is `open(dir, O_RDONLY)` followed by `fsync`. On Windows a directory handle cannot be
flushed; the equivalent guarantee comes from `MOVEFILE_WRITE_THROUGH` on `MoveFileEx`, so
`Dir.sync!` is a no-op there and the durability is carried by the rename itself.

### 3.3 The temporary file must be a sibling

`rename` fails with `EXDEV` across filesystem boundaries, and `/tmp` is very often a different
filesystem. The temporary is therefore always created in the destination's directory, never in a
system temporary directory.

### 3.4 Orphan reclamation — no cleanup daemon, no random names

A temporary file survives only a hard kill: every ordinary error path in `replace_content` unlinks it
before raising. `SIGKILL`, OOM, and power loss can still leave one behind, and a durable-write
primitive that slowly fills a directory with debris is not acceptable.

The temporary name is therefore **deterministic**, not random — `<name>.promise-sync<N>` beside the
destination — and liveness is decided by the lock:

1. Open `<name>.promise-sync0` with `O_RDWR|O_CREAT` — **not** `O_EXCL` — and `try_lock` it.
   **The lock alone decides ownership.** Acquired means the slot is ours and whatever it holds is
   stale, because the kernel releases locks on process death (§5.4), so truncate it to zero.
2. If `try_lock` fails, a live writer owns it. Try `<name>.promise-sync1`, `2`, … to a bound of 8,
   then raise.
3. Hold the lock for the whole write. The rename removes the name; closing releases the lock.

Creation is deliberately *not* the ownership test. `open(O_CREAT|O_EXCL)` and the lock are two
syscalls, and between them the temporary exists and is unlocked — indistinguishable from a crash
orphan. A second writer arriving in that window would classify a live writer's slot as stale,
truncate it, and rename it into place mid-write, violating §3.1 and §3.5. Deciding on the lock alone
collapses the two states into one observation that no window can separate.

This needs no random source, no process identifier, no clock, and no liveness probe. Process
identifier reuse cannot fool it, concurrent writers to one destination never collide, and every
temporary left by a crash is reclaimed by the next writer to that path.

### 3.5 `replace_content` does not lock the destination

Two processes replacing the same path both succeed; the last rename wins and neither observes a
partial file. That is the guarantee, and it is sufficient for a store whose records are written whole.

A **read-modify-write** cycle needs more than that, and the lock must be held by the caller across
the entire cycle. `replace_content` deliberately does not take a destination lock, because a caller
who already holds one would deadlock against it.

---

## 4. Forcing data to stable storage

`File.sync!` returns only once the file's contents are on stable storage.

| Platform | Call |
|---|---|
| Linux | `fsync(2)` |
| macOS | `fcntl(fd, F_FULLFSYNC)` — **not** `fsync(2)` |
| Windows | `FlushFileBuffers` |
| WASM | unsupported — raises |

**macOS uses `F_FULLFSYNC` deliberately, and it is significantly slower.** `fsync(2)` on macOS
returns once the data has been handed to the drive, which is free to keep it in a volatile write
cache; only `F_FULLFSYNC` issues the barrier that makes it durable. A durability primitive that is
not durable is worse than no primitive at all, because callers build on the guarantee it advertises.
Correctness is chosen over speed here without a knob to trade it back.

---

## 5. Advisory locking

### 5.1 The model: one lock per open file, whole-file, advisory

A lock is owned by the **open file description**, not by the process and not by the path. Locks are
whole-file; there are no byte ranges. `lock!` is exclusive and `lock_shared!` is shared; `unlock!`
releases early — otherwise the lock is released when the file closes.

One lock model, three ways to acquire it: **wait indefinitely** (`lock!`, `lock_shared!`), **never
wait** (`try_lock!`), or **wait up to a deadline** (`try_lock_for!`, §5.7). The mode is chosen by
which method is called rather than by a flag, so the call site says which one it is.

| Platform | Implementation |
|---|---|
| Linux, macOS | `flock(2)` — `LOCK_EX`, `LOCK_SH`, `LOCK_NB`, `LOCK_UN` |
| Windows | `LockFileEx` / `UnlockFileEx` over the whole file |
| WASM | unsupported — raises |

### 5.2 Why `flock`, and what it costs

POSIX offers three mechanisms and they are not interchangeable:

| | Ownership | Released when | Byte ranges | NFS | macOS |
|---|---|---|---|---|---|
| `flock(2)` | open file description | **all** descriptors for that description close | no | no | yes |
| `fcntl(2)` records | (process, inode) | **any** descriptor for the file closes | yes | yes | yes |
| `F_OFD_SETLK` | open file description | all descriptors close | yes | yes | **no** |

`F_OFD_SETLK` is the best of the three and macOS does not have it. `fcntl` record locks carry a
defect that a library cannot hide: an unrelated `close()` anywhere in the process silently drops the
lock, so a correct caller would have to audit every descriptor in the program.

`flock` is chosen because its per-open-file-description ownership is also what Windows `LockFileEx`
provides per `HANDLE` — giving **one** model to document rather than two.

The cost is NFS, and it is accepted rather than worked around (§5.5).

### 5.3 Advisory on POSIX, mandatory on Windows

`flock` is advisory: a process that never takes the lock can read and write the file freely. Windows
`LockFileEx` is mandatory: conflicting reads and writes from other handles actually fail.

This difference is irreducible and is **not** smoothed over. A correct program takes the lock and
does not depend on either behavior — it must not assume other writers are blocked (they are not, on
POSIX), and it must not assume unlocked access will succeed (it will not, on Windows).

### 5.4 Release on death is guaranteed

The kernel releases the lock when the process exits, including under `SIGKILL` and OOM kill — POSIX
tears down the descriptor table, Windows closes the handles. There is no stale-lock recovery to
write, and no lease timeout to tune.

This guarantee is what §3.4's orphan reclamation is built on. It does not extend to NFS.

### 5.5 NFS is out of scope

Locking over NFS is **not supported** and must not be relied on. Linux emulates `flock` over NFS with
`fcntl`, silently changing the ownership model out from under the caller; macOS returns `ENOTSUP`.
Rather than expose a mechanism that is subtly wrong on one platform and absent on another, network
filesystems are excluded from this contract.

### 5.6 No lock upgrade or downgrade

Converting a shared lock to exclusive (or back) is **not supported**. `flock` permits re-calling with
a different mode, but the conversion is not atomic — the lock is momentarily dropped, so another
waiter can acquire it in between, and a caller that assumed continuity would be wrong. Release and
re-acquire explicitly, and re-validate whatever was read under the shared lock.

### 5.7 Bounded waiting, and why it polls

`try_lock_for!(timeout)` waits up to `timeout` for the lock and returns `false` if it did not get it.

It exists because an *unbounded* wait is indistinguishable from a hang. A caller that serializes work
behind a lock — a build step, an exclusive resource, a single-instance daemon — needs to tell "someone
else holds this" apart from "I am stuck", and needs the waiting time to be attributable rather than
open-ended. Without a deadline that distinction cannot be made from inside the program.

**It is implemented by retrying `try_lock!` with capped exponential backoff, in Promise, and it does
not promise wakeup latency.** A caller that gets `false` knows only that the lock was unavailable for
at least `timeout`; a caller that succeeds may have waited up to one backoff interval longer than it
needed to. Neither platform offers a native timed whole-file acquire — `flock` has no timed form, and
Windows would need overlapped `LockFileEx` plus a wait object — so a native implementation would add
a third acquisition path to every backend in exchange for precision nothing here needs.

Choose the mode by what the caller does on failure: `try_lock!` when contention is an error worth
reporting immediately (a second instance starting), `try_lock_for!` when contention is expected and
waiting is legitimate but must stay bounded, `lock!` only when the holder is known to be brief.

---

## 6. Platform differences a caller can observe

Everything above is uniform across platforms except these, which are stated rather than hidden:

1. **Advisory vs mandatory locking** (§5.3).
2. **Delete-pending on Windows.** Files are opened with `FILE_SHARE_DELETE` so that an open file can
   be renamed over — see [windows-support.md](windows-support.md). This narrows the gap but does not
   close it: deleting an open file on Windows leaves the *name* visible until the last handle closes,
   and a new open of that name fails with `ERROR_ACCESS_DENIED`. POSIX `unlink` removes the name
   immediately and a fresh create succeeds.
3. **`Dir.sync!` is a no-op on Windows** (§3.2).
4. **WASM supports none of this.** `sync`, the lock operations, and `replace_content` raise. WASI has
   no advisory locking, and the target has no durability story to offer.

---

## 7. Errors

All operations raise `IoError`, carrying the platform error code as `code`. Beyond the codes
`modules/io` already maps, this contract adds:

| Code | Meaning here |
|---|---|
| `EXDEV` (18) | `rename` across filesystems — the temporary was not a sibling of the destination (§3.3) |
| `EAGAIN` / `EWOULDBLOCK` (11 Linux, 35 macOS) | contention. `try_lock!` reports it as `false` and never raises it; `replace_content` raises it when every temporary slot beside the destination is held by a live writer (§3.4) |
| `ENOSYS` | the operation is unsupported on this target (WASM) |

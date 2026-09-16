package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReadInstalledStubVersionMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// No sidecar present → version 0 (so a fresh install always forward-updates).
	if v := readInstalledStubVersion(dir); v != 0 {
		t.Fatalf("expected 0 for missing sidecar, got %d", v)
	}
}

func TestReadInstalledStubVersionValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte("7\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if v := readInstalledStubVersion(dir); v != 7 {
		t.Fatalf("expected 7, got %d", v)
	}
}

func TestReadInstalledStubVersionGarbage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}
	// Unparseable sidecar → 0, never panics, never executes the stub.
	if v := readInstalledStubVersion(dir); v != 0 {
		t.Fatalf("expected 0 for garbage sidecar, got %d", v)
	}
}

// TestForwardOnlyDecision exercises the version comparison that gates a stub
// update: the installer replaces the stub only when its embedded version is
// strictly newer than the installed sidecar value (never downgrades).
func TestForwardOnlyDecision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := []struct {
		installed string // sidecar contents ("" = absent)
		embedded  int
		replace   bool
	}{
		{"", 1, true},    // fresh install
		{"1", 2, true},   // newer embedded → replace
		{"2", 2, false},  // equal → keep
		{"3", 2, false},  // installed newer → never downgrade
		{"bad", 1, true}, // unreadable installed treated as 0 → replace
	}
	for _, c := range cases {
		if c.installed == "" {
			os.Remove(filepath.Join(dir, stubVersionSidecar))
		} else {
			if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte(c.installed), 0644); err != nil {
				t.Fatal(err)
			}
		}
		got := c.embedded > readInstalledStubVersion(dir)
		if got != c.replace {
			t.Errorf("installed=%q embedded=%d: expected replace=%v, got %v", c.installed, c.embedded, c.replace, got)
		}
	}
}

// TestReadEmbeddedStubDevBuild: dev builds (no embed_stub tag) carry no stub,
// so readEmbeddedStub reports a clear error rather than panicking or returning
// empty bytes. Release builds (T0773) supply the per-target binary.
func TestReadEmbeddedStubDevBuild(t *testing.T) {
	t.Parallel()
	if hasEmbeddedStub {
		t.Skip("build has an embedded stub; this guards the dev-build path")
	}
	_, err := readEmbeddedStub("promise")
	if err == nil {
		t.Fatal("expected an error reading an embedded stub in a dev build")
	}
	if !strings.Contains(err.Error(), "no embedded stub") {
		t.Fatalf("expected 'no embedded stub' error, got: %v", err)
	}
}

// TestWriteStubAndSidecarDevBuild: with no embedded stub, writeStubAndSidecar
// fails (because readEmbeddedStub fails) and must NOT leave a stub binary or a
// sidecar behind — a half-written launcher would be worse than none.
func TestWriteStubAndSidecarDevBuild(t *testing.T) {
	t.Parallel()
	if hasEmbeddedStub {
		t.Skip("build has an embedded stub; this guards the dev-build path")
	}
	dir := t.TempDir()
	if err := writeStubAndSidecar(dir, "promise"); err == nil {
		t.Fatal("expected writeStubAndSidecar to fail without an embedded stub")
	}
	if _, err := os.Stat(filepath.Join(dir, "promise")); !os.IsNotExist(err) {
		t.Error("no stub binary should be written when the embedded stub is absent")
	}
	if _, err := os.Stat(filepath.Join(dir, stubVersionSidecar)); !os.IsNotExist(err) {
		t.Error("no sidecar should be written when the embedded stub is absent")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "thing")
	if err := writeFileAtomic(path, []byte("hello"), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(data))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not model Unix permission bits — os.Stat reports 0666/0444
	// based solely on the read-only attribute, so skip the exact-mode check there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		t.Fatalf("expected mode 0755, got %v", info.Mode().Perm())
	}
	// Overwrite atomically — no leftover temp files in the dir.
	if err := writeFileAtomic(path, []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "world" {
		t.Fatalf("expected 'world', got %q", string(data))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file (no temp leftovers), got %d", len(entries))
	}
}

// TestWriteFileAtomicBadDir: when the destination directory does not exist,
// os.CreateTemp fails and writeFileAtomic returns that error (rather than
// panicking or silently succeeding) and writes nothing. writeStubAndSidecar
// relies on this error being propagated so a failed stub install aborts cleanly.
func TestWriteFileAtomicBadDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist", "thing")
	if err := writeFileAtomic(missing, []byte("data"), 0644); err == nil {
		t.Fatal("expected an error writing into a nonexistent directory")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("no file should be created when the directory is missing")
	}
}

// TestWriteFileAtomicLostReplaceRaceSucceeds: on Windows the replace step of a
// rename fails with ERROR_ACCESS_DENIED while any other handle to the destination
// lives, and os.Open holds exactly such a handle (no FILE_SHARE_DELETE). A writer
// that loses that race to a peer who wrote the *same* bytes has nothing left to
// do, so writeFileAtomic reports success rather than aborting the caller's build
// (T2132). Before the fix this returned "Access is denied." after the full retry
// budget — which is what turned a loaded bin/verify run red.
func TestWriteFileAtomicLostReplaceRaceSucceeds(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("rename(2) is atomic on POSIX; a held destination is a Windows-only failure")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "libSystem.B.tbd")
	content := []byte("--- !tapi-tbd\ntargets: [ x86_64-macos ]\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	// The handle a peer writer's replace, an antivirus, or the indexer would hold.
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if err := writeFileAtomic(path, content, 0644); err != nil {
		t.Fatalf("writing bytes the destination already holds must succeed, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination = %q (err %v), want it left holding the same bytes", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file (no temp leftovers), got %d", len(entries))
	}
}

// TestWriteFileAtomicHeldDestinationKeepsItsPromise is the other half, and guards
// against an over-eager version of the fix rather than against the original bug:
// whatever writeFileAtomic reports, success must mean the destination really holds
// the requested bytes. The existing file here is the same *length* as the wanted
// content and different from it, so a settled check that compared only sizes would
// report success over a destination it never wrote — and a corrupt stub of the
// right length would quietly stop being repaired.
func TestWriteFileAtomicHeldDestinationKeepsItsPromise(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("rename(2) is atomic on POSIX; a held destination is a Windows-only failure")
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "libSystem.B.tbd")
	want := []byte("hello")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(want)), 0644); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	werr := writeFileAtomic(path, want, 0644)
	got, rerr := os.ReadFile(path)
	if werr == nil && (rerr != nil || !bytes.Equal(got, want)) {
		t.Fatalf("reported success while the destination still holds %q (err %v)", got, rerr)
	}
}

// TestRenameWithRetryNonRetryableFailsFast: a non-retryable rename error (here a
// nonexistent source, which is real on every platform) must short-circuit
// immediately rather than spin through the full backoff budget (~0.55s). This
// proves the retry loop only burns wall-clock on transient Windows lock errors.
func TestRenameWithRetryNonRetryableFailsFast(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "does-not-exist")
	dst := filepath.Join(dir, "dst")
	start := time.Now()
	err := renameWithRetry(src, dst, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error renaming a nonexistent source")
	}
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("non-retryable error should fail fast, took %v", elapsed)
	}
}

// errRetryable is a stand-in for the transient Windows sharing/lock errors that
// isRetryableRenameError matches; the predicate below treats only it as retryable.
var errRetryable = errors.New("sharing violation")

func retryablePredicate(err error) bool { return errors.Is(err, errRetryable) }
func noBackoff(int) time.Duration       { return 0 }

// TestRenameRetryingSucceedsAfterTransient: the loop keeps retrying while the
// rename returns a retryable error and succeeds once the (simulated) lock clears.
// This is the Windows happy path that the T0793 fix targets, exercised on any OS.
func TestRenameRetryingSucceedsAfterTransient(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(src, dst string) error {
		calls++
		if calls < 4 { // fail the first 3, succeed on the 4th
			return errRetryable
		}
		return nil
	}
	if err := renameRetrying(rename, retryablePredicate, noBackoff, nil, "s", "d"); err != nil {
		t.Fatalf("expected success after transient errors, got %v", err)
	}
	if calls != 4 {
		t.Fatalf("expected 4 rename attempts, got %d", calls)
	}
}

// TestRenameRetryingExhausts: when every attempt returns a retryable error, the
// loop gives up after exactly renameAttempts tries and returns the last error —
// it never spins forever and never swallows the failure.
func TestRenameRetryingExhausts(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(src, dst string) error {
		calls++
		return errRetryable
	}
	err := renameRetrying(rename, retryablePredicate, noBackoff, nil, "s", "d")
	if !errors.Is(err, errRetryable) {
		t.Fatalf("expected the last retryable error, got %v", err)
	}
	if calls != renameAttempts {
		t.Fatalf("expected exactly %d attempts on exhaustion, got %d", renameAttempts, calls)
	}
}

// TestRenameRetryingNonRetryable: a non-retryable error short-circuits after a
// single attempt — no retries, the error propagates verbatim.
func TestRenameRetryingNonRetryable(t *testing.T) {
	t.Parallel()
	fatal := errors.New("no such file")
	calls := 0
	rename := func(src, dst string) error {
		calls++
		return fatal
	}
	err := renameRetrying(rename, retryablePredicate, noBackoff, nil, "s", "d")
	if !errors.Is(err, fatal) {
		t.Fatalf("expected the non-retryable error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt for a non-retryable error, got %d", calls)
	}
}

// TestRenameRetryingFirstTry: the common case — rename succeeds immediately, so
// the loop returns nil without consulting the retryable predicate or backoff.
func TestRenameRetryingFirstTry(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(src, dst string) error { calls++; return nil }
	if err := renameRetrying(rename, retryablePredicate, noBackoff, nil, "s", "d"); err != nil {
		t.Fatalf("expected immediate success, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", calls)
	}
}

// TestRenameRetryingAcceptsASettledDestination: the Windows lost-replace race
// (T2132). The rename keeps failing with a retryable error, but the destination
// already holds what this rename was for, so the loop stops and reports success
// instead of spending the rest of its budget on work another writer finished.
func TestRenameRetryingAcceptsASettledDestination(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(src, dst string) error { calls++; return errRetryable }
	checks := 0
	settled := func() bool { checks++; return checks >= 2 } // settles on the 2nd look
	if err := renameRetrying(rename, retryablePredicate, noBackoff, settled, "s", "d"); err != nil {
		t.Fatalf("a destination that already holds the outcome is success, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected the loop to stop after 2 attempts, got %d", calls)
	}
}

// TestRenameRetryingUnsettledDestinationStillExhausts: a destination that never
// settles is a real failure. The loop still spends the full budget and returns the
// last error, so a lost race is never confused with a broken one.
func TestRenameRetryingUnsettledDestinationStillExhausts(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(src, dst string) error { calls++; return errRetryable }
	err := renameRetrying(rename, retryablePredicate, noBackoff, func() bool { return false }, "s", "d")
	if !errors.Is(err, errRetryable) {
		t.Fatalf("expected the last retryable error, got %v", err)
	}
	if calls != renameAttempts {
		t.Fatalf("expected exactly %d attempts on exhaustion, got %d", renameAttempts, calls)
	}
}

// TestRenameRetryingNonRetryableIgnoresSettled: a non-retryable error is a real
// one and is never masked by a destination that happens to be right. The settled
// check is not even consulted — the question it answers ("did someone else finish
// this for me?") is only meaningful for a transient sharing failure.
func TestRenameRetryingNonRetryableIgnoresSettled(t *testing.T) {
	t.Parallel()
	fatal := errors.New("no such file")
	consulted := false
	rename := func(src, dst string) error { return fatal }
	settled := func() bool { consulted = true; return true }
	err := renameRetrying(rename, retryablePredicate, noBackoff, settled, "s", "d")
	if !errors.Is(err, fatal) {
		t.Fatalf("expected the non-retryable error, got %v", err)
	}
	if consulted {
		t.Error("settled must not be consulted for a non-retryable error")
	}
}

// TestFileHasBytes pins the settled check writeFileAtomic supplies: only a
// destination that already holds exactly the wanted bytes counts. Size equality is
// not enough — a same-size file with different content still needs writing.
func TestFileHasBytes(t *testing.T) {
	t.Parallel()
	want := []byte("hello")
	cases := []struct {
		name     string
		existing []byte // nil: no file at that path at all
		hasBytes bool
	}{
		{"missing", nil, false},
		{"shorter", []byte("hell"), false},
		{"longer", []byte("hello world"), false},
		{"same size different bytes", []byte("HELLO"), false},
		{"identical", []byte("hello"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f")
			if c.existing != nil {
				if err := os.WriteFile(path, c.existing, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if got := fileHasBytes(path, want); got != c.hasBytes {
				t.Errorf("fileHasBytes = %v, want %v", got, c.hasBytes)
			}
		})
	}
}

// TestFileHasBytesDirectoryIsNotContent: a directory is never a destination that
// already holds the wanted bytes. This is the read's error check, and it is
// load-bearing rather than defensive: on Windows os.Stat reports a directory's
// size as 0, so a zero-length write to a path a directory occupies gets past the
// size comparison, and os.ReadFile then fails and returns nil — which bytes.Equal
// considers equal to empty. Without the check, fileHasBytes would call that
// destination correct and writeFileAtomic would report a write it never did.
func TestFileHasBytesDirectoryIsNotContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, want := range [][]byte{nil, {}, []byte("x")} {
		if fileHasBytes(dir, want) {
			t.Errorf("a directory holds no content, but fileHasBytes(dir, %q) said it does", want)
		}
	}
}

// TestCopyFileContentAndPerm: copyFile reproduces the source bytes, applies the
// requested permissions, and leaves no temp files behind in the destination dir.
func TestCopyFileContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	copyFile(src, dst, 0755)
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("expected 'payload', got %q", string(data))
	}
	// Windows does not model Unix permission bits (see TestWriteFileAtomic).
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0755 {
			t.Fatalf("expected mode 0755, got %v", info.Mode().Perm())
		}
	}
	// No leftover temp files: just src and dst remain.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 files (src, dst; no temp leftovers), got %d", len(entries))
	}
}

// TestReplaceSymlinkRetryingDrawsANewNameOnCollision: when the sibling name is
// already taken, the loop must draw a *different* name and go on — a retry that
// reuses the same name would spin pointlessly and then fail. The collision needs
// two processes to draw the same pid+random name, so it is injected here rather
// than provoked (T2120).
func TestReplaceSymlinkRetryingDrawsANewNameOnCollision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "libSystem.tbd")
	if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	var names []string
	symlink := func(target, name string) error {
		names = append(names, name)
		if len(names) < 3 { // the first two names are "taken"
			return os.ErrExist
		}
		return os.Symlink(target, name)
	}

	if err := replaceSymlinkRetrying(symlink, "libSystem.B.tbd", path); err != nil {
		t.Fatalf("expected success once a free name is found, got %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 name attempts, got %d", len(names))
	}
	if names[0] == names[1] || names[1] == names[2] {
		t.Errorf("retry reused a name: %v", names)
	}
	if got, err := os.Readlink(path); err != nil || got != "libSystem.B.tbd" {
		t.Fatalf("link = %q (err %v), want libSystem.B.tbd", got, err)
	}
}

// TestReplaceSymlinkRetryingPropagatesSymlinkError: "that name is taken" is the
// only symlink failure worth another name. Anything else — a permission denial, a
// filesystem with no symlinks — is a real error about the operation itself, so it
// propagates on the first attempt instead of being retried eight times under
// different names. Nothing was created, so nothing is removed and the entry that
// was already at path is left exactly as it was.
func TestReplaceSymlinkRetryingPropagatesSymlinkError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "libSystem.tbd")
	if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	denied := errors.New("operation not permitted")
	calls := 0
	symlink := func(target, name string) error { calls++; return denied }

	err := replaceSymlinkRetrying(symlink, "libSystem.B.tbd", path)
	if !errors.Is(err, denied) {
		t.Fatalf("expected the symlink error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("a real symlink failure must not be retried, got %d attempts", calls)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil || string(got) != "stale" {
		t.Fatalf("existing entry = %q (err %v), want it untouched", got, rerr)
	}
	entries, rderr := os.ReadDir(dir)
	if rderr != nil {
		t.Fatal(rderr)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the original entry, got %d", len(entries))
	}
}

// TestReplaceSymlinkRetryingExhausts: when every name collides, the loop gives up
// after exactly symlinkNameAttempts tries with a clear error, leaves the existing
// entry untouched, and leaves no temp entry behind — it never spins forever and
// never destroys what is already there.
func TestReplaceSymlinkRetryingExhausts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "libSystem.tbd")
	if err := os.Symlink("wrong.tbd", path); err != nil {
		t.Fatal(err)
	}

	calls := 0
	symlink := func(target, name string) error {
		calls++
		return os.ErrExist
	}

	err := replaceSymlinkRetrying(symlink, "libSystem.B.tbd", path)
	if err == nil {
		t.Fatal("expected an error when every name collides")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should name the directory, got %v", err)
	}
	if calls != symlinkNameAttempts {
		t.Fatalf("expected exactly %d attempts on exhaustion, got %d", symlinkNameAttempts, calls)
	}
	if got, lerr := os.Readlink(path); lerr != nil || got != "wrong.tbd" {
		t.Errorf("existing entry must be left untouched, got %q (err %v)", got, lerr)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the original entry, got %d", len(entries))
	}
}

// TestCopyFileAtomicMultiBufferPayload pins copyFileAtomic's contract over a
// payload larger than one io.Copy buffer: byte-exact, right permissions, and
// still atomic — no staging file survives.
//
// Whether the implementation streams is not observable from out here (a
// whole-file read produces the same bytes); that it must is stated where the
// function is, and is why it replaced copyFile's os.ReadFile — a ~200 MB
// allocation per toolchain blob on the path a cold PROMISE_HOME takes before it
// can compile anything (T2133). What this catches is a chunked copy that drops
// or reorders a buffer, which is the way that change could have gone wrong.
func TestCopyFileAtomicMultiBufferPayload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "blob")
	// Several io.Copy buffers (32 KiB each) of a position-dependent pattern, so
	// a copy that dropped or reordered a chunk cannot pass.
	payload := make([]byte, 300*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "opt")
	if err := copyFileAtomic(src, dst, 0o755); err != nil {
		t.Fatalf("copyFileAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("copied %d bytes, want the source's %d, byte-identical", len(got), len(payload))
	}
	if runtime.GOOS != "windows" { // Windows does not model Unix permission bits.
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("perm = %v, want 0755", info.Mode().Perm())
		}
	}
	// Atomic: the staging file is renamed into place, never left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("staging file %q left behind", e.Name())
		}
	}
}

// TestCopyFileAtomicMissingSourceReports: a copy that cannot start returns the
// error rather than creating an empty destination. copyFile turns this into an
// exit; the view materializer turns it into a failed publish, and neither may
// be handed a truncated tool.
func TestCopyFileAtomicMissingSourceReports(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dst := filepath.Join(dir, "opt")
	if err := copyFileAtomic(filepath.Join(dir, "absent"), dst, 0o755); err == nil {
		t.Fatal("copyFileAtomic of a missing source returned nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a failed copy left %q behind: %v", dst, err)
	}
}

// TestCopyFileAtomicNoDestinationDirReports: the staging file is created beside
// the destination, so a view dir that vanished underneath — a concurrent clean,
// a publish that was rolled back — must surface as an error rather than as a
// silently missing tool the linker discovers later.
func TestCopyFileAtomicNoDestinationDirReports(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "blob")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "gone", "opt")
	if err := copyFileAtomic(src, dst, 0o755); err == nil {
		t.Fatal("copyFileAtomic into a missing directory returned nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a failed copy left %q behind: %v", dst, err)
	}
}

// TestCopyFileAtomicUnreadableSourceReports: a source that opens but cannot be
// read through — a blob path that is a directory — must fail the copy and clean
// up its staging file, never publish a truncated tool.
func TestCopyFileAtomicUnreadableSourceReports(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "not-a-blob")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "opt")
	if err := copyFileAtomic(src, dst, 0o755); err == nil {
		t.Fatal("copyFileAtomic of a directory returned nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a failed copy published %q: %v", dst, err)
	}
	// And the staging file it opened is gone, so a retry is not blocked by it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("a failed copy left the staging file %q behind", e.Name())
		}
	}
}

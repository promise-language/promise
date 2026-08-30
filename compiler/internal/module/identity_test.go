package module

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marker mirrors what the Go linker writes ahead of the build ID. Its presence
// in the hashed window is the precondition that makes hashing a prefix sound.
const testMarker = "\xff Go build ID: \"abc/def\"\n \xff"

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCompilerIdentityShape locks the contract every cache key depends on: 128
// bits, as 32 hex characters, and stable within a process.
func TestCompilerIdentityShape(t *testing.T) {
	id := CompilerIdentity()
	if len(id) != 32 {
		t.Errorf("identity = %q (%d chars), want 32 hex chars (128 bits)", id, len(id))
	}
	if strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("identity = %q, want lowercase hex only", id)
	}
	if again := CompilerIdentity(); again != id {
		t.Errorf("identity not stable within a process: %q then %q", id, again)
	}
}

// TestCompilerIdentityHashesPrefixWhenMarkerPresent — with the marker in the
// window, the identity is the prefix hash. That is the fast path, and it is
// sound because the window carries the build ID, itself derived from the whole
// binary.
func TestCompilerIdentityHashesPrefixWhenMarkerPresent(t *testing.T) {
	head := make([]byte, compilerIdentityBytes)
	copy(head, "MZ-or-whatever-header")
	copy(head[4096:], testMarker)

	body := append(append([]byte{}, head...), []byte("trailing content far past the window")...)
	p := writeFile(t, "withmarker", body)

	sum := sha256.Sum256(head)
	want := hex.EncodeToString(sum[:])[:32]
	got, err := compilerIdentityOf(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("identity = %s, want the 32KB prefix hash %s", got, want)
	}
}

// TestCompilerIdentityRejectsBinaryWithoutBuildID — the prefix identifies the
// whole binary only because the build ID sits in it. Without the marker that no
// longer holds, and the hash would describe headers alone, where two different
// compilers could collide — a stale cache HIT, i.e. wrong output.
//
// So this is an error, deliberately NOT a fallback to hashing the whole file. A
// fallback would be a second path taken only when the first had quietly stopped
// being valid, which is the failure nobody notices. If this fires, the
// assumption has broken and the scheme should be changed on purpose.
func TestCompilerIdentityRejectsBinaryWithoutBuildID(t *testing.T) {
	head := make([]byte, compilerIdentityBytes) // no marker anywhere
	p := writeFile(t, "nomarker", append(head, []byte("payload")...))

	if _, err := compilerIdentityOf(p); err == nil {
		t.Error("expected an error for a binary with no Go build ID in the window")
	} else if !strings.Contains(err.Error(), "no Go build ID") {
		t.Errorf("error should name the missing build ID, got: %v", err)
	}
}

// TestCompilerIdentityDocumentsPrefixLimit pins what the identity does NOT
// promise. With the marker present, two files identical in their first 32KB are
// indistinguishable — the identity discriminates BUILDS (a rebuild changes the
// build ID and LC_UUID inside the window) but is not tamper detection: a binary
// edited after linking keeps its build ID.
//
// That is the same trade the Go toolchain makes by storing contentID to avoid
// hashing whole binaries. If this ever needs to detect post-link edits, this
// test is the one that has to change, deliberately.
func TestCompilerIdentityDocumentsPrefixLimit(t *testing.T) {
	head := make([]byte, compilerIdentityBytes)
	copy(head[4096:], testMarker)

	a := append(append([]byte{}, head...), []byte("original tail")...)
	b := append(append([]byte{}, head...), []byte("modified tail")...)
	pa, pb := writeFile(t, "a", a), writeFile(t, "b", b)

	ida, _ := compilerIdentityOf(pa)
	idb, _ := compilerIdentityOf(pb)
	if ida != idb {
		t.Errorf("identities differ — the prefix-hash limit this test documents " +
			"no longer holds; if that was deliberate, update this test")
	}
}

// TestCompilerIdentityUnreadableFile — an identity we cannot compute must be an
// error, never a plausible-looking value that would key a cache incorrectly.
// CompilerIdentity turns this into a fatal, since every compiler-keyed cache
// depends on the value.
func TestCompilerIdentityUnreadableFile(t *testing.T) {
	if _, err := compilerIdentityOf(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Error("expected an error for a file that cannot be read")
	}
}

// synthELF builds a minimal 64-bit little-endian ELF carrying a
// .note.go.buildid section at noteOffset. Real Go binaries are what the ELF
// branch parses in anger; this exists so the placement rule can be tested at
// both sides of the window boundary without a toolchain, and on hosts that do
// not produce ELF at all.
func synthELF(t *testing.T, noteOffset int, extra []byte) []byte {
	t.Helper()

	const (
		ehdrSize  = 64
		shdrSize  = 64
		numShdr   = 3 // null, .shstrtab, .note.go.buildid
		shoff     = ehdrSize
		strOffset = shoff + numShdr*shdrSize
	)
	// Offsets of the two names within the string table below.
	strtab := []byte("\x00.shstrtab\x00.note.go.buildid\x00")
	const nameShstrtab, nameNote = 1, 11

	// An ELF note: namesz, descsz, type, then the padded name and the payload.
	desc := []byte("Zx1/actionID/contentID")
	note := make([]byte, 12)
	binary.LittleEndian.PutUint32(note[0:], 4) // namesz — "Go\0\0"
	binary.LittleEndian.PutUint32(note[4:], uint32(len(desc)))
	binary.LittleEndian.PutUint32(note[8:], 4) // NT_GO_BUILD_ID
	note = append(note, "Go\x00\x00"...)
	note = append(note, desc...)

	if noteOffset < strOffset+len(strtab) {
		t.Fatalf("note at %d would overlap the string table ending at %d",
			noteOffset, strOffset+len(strtab))
	}

	size := noteOffset + len(note)
	if n := compilerIdentityBytes + len(extra); n > size {
		size = n
	}
	buf := make([]byte, size)

	copy(buf, "\x7fELF")
	buf[4] = 2 // ELFCLASS64
	buf[5] = 1 // ELFDATA2LSB
	buf[6] = 1 // EV_CURRENT
	binary.LittleEndian.PutUint16(buf[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(buf[18:], uint16(elf.EM_X86_64))
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint64(buf[40:], shoff)
	binary.LittleEndian.PutUint16(buf[52:], ehdrSize)
	binary.LittleEndian.PutUint16(buf[58:], shdrSize)
	binary.LittleEndian.PutUint16(buf[60:], numShdr)
	binary.LittleEndian.PutUint16(buf[62:], 1) // e_shstrndx

	putShdr := func(idx int, name uint32, typ elf.SectionType, off, sz uint64) {
		s := buf[shoff+idx*shdrSize:]
		binary.LittleEndian.PutUint32(s[0:], name)
		binary.LittleEndian.PutUint32(s[4:], uint32(typ))
		binary.LittleEndian.PutUint64(s[24:], off)
		binary.LittleEndian.PutUint64(s[32:], sz)
	}
	putShdr(1, nameShstrtab, elf.SHT_STRTAB, strOffset, uint64(len(strtab)))
	putShdr(2, nameNote, elf.SHT_NOTE, uint64(noteOffset), uint64(len(note)))
	copy(buf[strOffset:], strtab)
	copy(buf[noteOffset:], note)
	copy(buf[compilerIdentityBytes:], extra)

	return buf
}

// TestCompilerIdentityAcceptsThisBinary is the regression test for T1870: the
// running test binary is a real Go binary in the host's own object format, so
// whatever shape the linker gives the build ID here, the identity must compute.
// Looking only for the raw marker made every Linux invocation fatal — the
// binary's ID was in a .note.go.buildid section instead, and no test built a
// binary the way the toolchain actually does.
//
// TestCompilerIdentityShape covers the same ground through CompilerIdentity,
// but that path exits the process; this one reports.
func TestCompilerIdentityAcceptsThisBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	if _, err := compilerIdentityOf(exe); err != nil {
		t.Errorf("this binary has no identity: %v\n"+
			"the build ID is embedded in whatever shape this platform's linker "+
			"uses; buildIDInWindow has to know that shape", err)
	}
}

// TestCompilerIdentityFindsELFBuildIDNote — on ELF the build ID lives in a note
// section with no marker anywhere. Finding it there is what makes the prefix
// hash sound on Linux.
func TestCompilerIdentityFindsELFBuildIDNote(t *testing.T) {
	bin := synthELF(t, 4096, []byte("trailing content far past the window"))
	p := writeFile(t, "elf-note", bin)

	sum := sha256.Sum256(bin[:compilerIdentityBytes])
	want := hex.EncodeToString(sum[:])[:32]
	got, err := compilerIdentityOf(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("identity = %s, want the 32KB prefix hash %s", got, want)
	}
}

// TestCompilerIdentityRejectsELFBuildIDNoteBeyondWindow — a note past the
// window is not in what we hash, so the hash would again describe headers
// alone. Present-but-out-of-range has to fail exactly like absent; the check
// is on the note's placement, not merely its existence.
func TestCompilerIdentityRejectsELFBuildIDNoteBeyondWindow(t *testing.T) {
	p := writeFile(t, "elf-far-note", synthELF(t, compilerIdentityBytes+64, nil))

	if _, err := compilerIdentityOf(p); err == nil {
		t.Error("expected an error for a build ID note outside the hashed window")
	} else if !strings.Contains(err.Error(), "no Go build ID") {
		t.Errorf("error should name the missing build ID, got: %v", err)
	}
}

// TestCompilerIdentityRejectsELFWithoutBuildIDNote — the marker is Mach-O/PE
// shape and carries no weight on ELF, where the note is the only place a build
// ID legitimately is. Accepting the byte sequence wherever it turned up would
// let arbitrary content satisfy the precondition.
func TestCompilerIdentityRejectsELFWithoutBuildIDNote(t *testing.T) {
	bin := synthELF(t, 4096, nil)
	// Erase the note's section header, keeping the file otherwise valid, and
	// scatter the raw marker through the window.
	for i := 64 + 2*64; i < 64+3*64; i++ {
		bin[i] = 0
	}
	copy(bin[8192:], testMarker)
	p := writeFile(t, "elf-marker-only", bin)

	if _, err := compilerIdentityOf(p); err == nil {
		t.Error("expected an error: on ELF the raw marker is not a build ID")
	}
}

// Package elfstub emits minimal ELF shared objects that satisfy a binary's
// dynamic dependency on a host library whose code it never actually runs.
//
// Why this exists: the pinned upstream LLVM Linux release binaries are not
// fully static. `lld` carries a DT_NEEDED on `libxml2.so.2` — used only by
// LLVM's WindowsManifestMerger (COFF `/manifestinput:`), a path Promise never
// takes — and it is linked BIND_NOW, so every one of those `xml*` symbols must
// resolve at load time. On a machine without libxml2 installed (a base Ubuntu
// install has none) the linker cannot even start, and every `promise build`
// fails with "error while loading shared libraries" (T1774). Requiring the user
// to install a system `.so` would break the zero-dependency bar that
// docs/distribution.md §2.2/§5.3 sets, so the compiler supplies the library
// itself: a ~2 KB stub, generated into the LLVM view dir that `runLLVMCmd`
// already puts on LD_LIBRARY_PATH.
//
// The stub is emitted byte-by-byte rather than compiled, because the host has
// no toolchain to compile it with — and the only linker we ship is the very
// binary that cannot load. It is the ELF counterpart of the Windows import libs
// generated from `.def` symbol lists (T0772): a name-and-version surface with
// no implementation behind it.
//
// Every exported symbol points at a trap instruction. If LLVM ever does reach
// the manifest path, it dies loudly at the call rather than silently reading
// nonsense — the stub is a load-time formality, never a working libxml2.
package elfstub

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"sort"
)

// Sym is one symbol a stub must define: the name the importer looks up, the
// symbol-version tag it looks it up under (empty for an unversioned reference),
// and the symbol type mirrored from the importing binary's undefined entry.
type Sym struct {
	Name    string
	Version string
	Type    elf.SymType
}

// ELF constants the stdlib does not name.
const (
	ehdrSize   = 64
	phdrSize   = 56
	shdrSize   = 64
	symSize    = 24
	dynSize    = 16
	verdefSize = 20
	verdauxSz  = 8

	verFlagBase = 1 // VER_FLG_BASE — the verdef entry naming the object itself

	trapSlot = 4 // bytes reserved per symbol in .text
)

// Build returns the bytes of a shared object named soname, for the given
// machine, defining exactly syms. The result is a complete ET_DYN image: one
// read+execute PT_LOAD covering every allocated section, a PT_DYNAMIC, a SysV
// hash table (DT_HASH — glibc needs one hash table and this is the simpler of
// the two), and .gnu.version/.gnu.version_d so versioned references resolve.
//
// The version definitions are not optional. glibc will not accept an
// unversioned library for a versioned reference from a BIND_NOW binary: it
// fails an internal assertion in dl-lookup.c rather than falling back, so a
// stub without .gnu.version_d aborts the linker instead of running it.
//
// Output is deterministic: the same inputs produce identical bytes, so a
// rebuilt view dir does not churn.
func Build(machine elf.Machine, soname string, syms []Sym) ([]byte, error) {
	if soname == "" {
		return nil, fmt.Errorf("elfstub: empty soname")
	}
	if len(syms) == 0 {
		return nil, fmt.Errorf("elfstub: no symbols for %s", soname)
	}

	// Distinct version tags, in first-seen order made deterministic by sorting.
	// Index 1 is the base definition (the object itself); user versions start
	// at 2, which is what .gnu.version entries reference.
	var versions []string
	seen := map[string]bool{}
	for _, s := range syms {
		if s.Version != "" && !seen[s.Version] {
			seen[s.Version] = true
			versions = append(versions, s.Version)
		}
	}
	sort.Strings(versions)
	verIndex := map[string]uint16{}
	for i, v := range versions {
		verIndex[v] = uint16(i + 2)
	}

	// String table: index 0 is the mandatory empty name.
	dynstr := &strtab{}
	dynstr.add("")
	sonameOff := dynstr.add(soname)
	verNameOff := make([]uint32, len(versions))
	for i, v := range versions {
		verNameOff[i] = dynstr.add(v)
	}
	symNameOff := make([]uint32, len(syms))
	for i, s := range syms {
		symNameOff[i] = dynstr.add(s.Name)
	}

	nsym := len(syms) + 1 // + the reserved null symbol at index 0
	nbucket := nsym

	// Lay the image out. Every allocated section is mapped identity (file
	// offset == virtual address), which keeps the single PT_LOAD trivially
	// page-congruent and the addresses in .dynamic equal to their offsets.
	var (
		hashSize   = (2 + nbucket + nsym) * 4
		dynsymSize = nsym * symSize
		versymSize = nsym * 2
		verdefNum  = 1 + len(versions)
		verdefSz   = verdefNum * (verdefSize + verdauxSz)
		textSize   = len(syms) * trapSlot
	)

	off := uint64(ehdrSize + 2*phdrSize)
	hashOff := align(off, 8)
	dynsymOff := align(hashOff+uint64(hashSize), 8)
	dynstrOff := dynsymOff + uint64(dynsymSize)
	versymOff := align(dynstrOff+uint64(dynstr.len()), 8)
	verdefOff := align(versymOff+uint64(versymSize), 8)
	textOff := align(verdefOff+uint64(verdefSz), 16)
	dynamicOff := align(textOff+uint64(textSize), 8)

	dyn := []struct{ tag, val uint64 }{
		{uint64(elf.DT_SONAME), uint64(sonameOff)},
		{uint64(elf.DT_HASH), hashOff},
		{uint64(elf.DT_STRTAB), dynstrOff},
		{uint64(elf.DT_SYMTAB), dynsymOff},
		{uint64(elf.DT_STRSZ), uint64(dynstr.len())},
		{uint64(elf.DT_SYMENT), symSize},
		{uint64(elf.DT_VERSYM), versymOff},
		{uint64(elf.DT_VERDEF), verdefOff},
		{uint64(elf.DT_VERDEFNUM), uint64(verdefNum)},
		{uint64(elf.DT_NULL), 0},
	}
	dynamicSize := uint64(len(dyn) * dynSize)
	loadSize := dynamicOff + dynamicSize

	// .shstrtab and the section headers live past the loaded image; they are
	// inspection aids (readelf, our own tests), not something ld.so reads.
	shstr := &strtab{}
	shstr.add("")
	nameHash := shstr.add(".hash")
	nameDynsym := shstr.add(".dynsym")
	nameDynstr := shstr.add(".dynstr")
	nameVersym := shstr.add(".gnu.version")
	nameVerdef := shstr.add(".gnu.version_d")
	nameText := shstr.add(".text")
	nameDynamic := shstr.add(".dynamic")
	nameShstr := shstr.add(".shstrtab")

	shstrOff := align(loadSize, 8)
	shoff := align(shstrOff+uint64(shstr.len()), 8)

	buf := make([]byte, shoff+9*shdrSize)
	w := &imageWriter{buf: buf}

	// --- ELF header ---
	copy(buf, []byte{0x7f, 'E', 'L', 'F'})
	buf[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	buf[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	buf[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	w.u16(16, uint16(elf.ET_DYN))
	w.u16(18, uint16(machine))
	w.u32(20, uint32(elf.EV_CURRENT))
	w.u64(24, 0) // e_entry
	w.u64(32, ehdrSize)
	w.u64(40, shoff)
	w.u32(48, 0) // e_flags
	w.u16(52, ehdrSize)
	w.u16(54, phdrSize)
	w.u16(56, 2)
	w.u16(58, shdrSize)
	w.u16(60, 9)
	w.u16(62, 8) // e_shstrndx

	// --- program headers ---
	// PT_LOAD covers the whole image r-x: the symbols point at trap
	// instructions, so the mapping has to be executable even though nothing
	// should ever branch there.
	ph := uint64(ehdrSize)
	w.u32(ph, uint32(elf.PT_LOAD))
	w.u32(ph+4, uint32(elf.PF_R|elf.PF_X))
	w.u64(ph+8, 0)  // p_offset
	w.u64(ph+16, 0) // p_vaddr
	w.u64(ph+24, 0) // p_paddr
	w.u64(ph+32, loadSize)
	w.u64(ph+40, loadSize)
	w.u64(ph+48, 0x1000)

	ph += phdrSize
	w.u32(ph, uint32(elf.PT_DYNAMIC))
	w.u32(ph+4, uint32(elf.PF_R))
	w.u64(ph+8, dynamicOff)
	w.u64(ph+16, dynamicOff)
	w.u64(ph+24, dynamicOff)
	w.u64(ph+32, dynamicSize)
	w.u64(ph+40, dynamicSize)
	w.u64(ph+48, 8)

	// --- .hash (SysV) ---
	buckets := make([]uint32, nbucket)
	chain := make([]uint32, nsym)
	for i, s := range syms {
		idx := uint32(i + 1)
		b := elfHash(s.Name) % uint32(nbucket)
		chain[idx] = buckets[b]
		buckets[b] = idx
	}
	w.u32(hashOff, uint32(nbucket))
	w.u32(hashOff+4, uint32(nsym))
	for i, b := range buckets {
		w.u32(hashOff+8+uint64(i)*4, b)
	}
	for i, c := range chain {
		w.u32(hashOff+8+uint64(nbucket)*4+uint64(i)*4, c)
	}

	// --- .dynsym --- (index 0 is the reserved null entry, left zeroed)
	for i, s := range syms {
		e := dynsymOff + uint64(i+1)*symSize
		typ := s.Type
		if typ != elf.STT_OBJECT {
			typ = elf.STT_FUNC
		}
		w.u32(e, symNameOff[i])
		buf[e+4] = byte(elf.ST_INFO(elf.STB_GLOBAL, typ))
		buf[e+5] = byte(elf.STV_DEFAULT)
		w.u16(e+6, 6) // st_shndx → .text
		w.u64(e+8, textOff+uint64(i)*trapSlot)
		w.u64(e+16, trapSlot)
	}

	// --- .dynstr ---
	copy(buf[dynstrOff:], dynstr.bytes())

	// --- .gnu.version --- (index 0 stays 0; defined symbols carry their tag,
	// and an unversioned symbol gets 1, the base definition)
	for i, s := range syms {
		idx := uint16(1)
		if s.Version != "" {
			idx = verIndex[s.Version]
		}
		w.u16(versymOff+uint64(i+1)*2, idx)
	}

	// --- .gnu.version_d --- base entry first, then one per version tag
	vd := verdefOff
	writeVerdef := func(o uint64, flags, ndx uint16, name uint32, last bool) {
		w.u16(o, 1) // vd_version
		w.u16(o+2, flags)
		w.u16(o+4, ndx)
		w.u16(o+6, 1) // vd_cnt — one verdaux, the name itself
		w.u32(o+8, elfHash(nameAt(dynstr, name)))
		w.u32(o+12, verdefSize)
		next := uint32(verdefSize + verdauxSz)
		if last {
			next = 0
		}
		w.u32(o+16, next)
		w.u32(o+verdefSize, name) // vda_name
		w.u32(o+verdefSize+4, 0)  // vda_next
	}
	writeVerdef(vd, verFlagBase, 1, sonameOff, len(versions) == 0)
	for i := range versions {
		vd += verdefSize + verdauxSz
		writeVerdef(vd, 0, uint16(i+2), verNameOff[i], i == len(versions)-1)
	}

	// --- .text --- one trap per symbol
	trap := trapBytes(machine)
	for i := range syms {
		copy(buf[textOff+uint64(i)*trapSlot:], trap)
	}

	// --- .dynamic ---
	for i, d := range dyn {
		o := dynamicOff + uint64(i)*dynSize
		w.u64(o, d.tag)
		w.u64(o+8, d.val)
	}

	// --- .shstrtab + section headers ---
	copy(buf[shstrOff:], shstr.bytes())
	// A section that is not allocated has no address, only a file offset; every
	// allocated one is mapped identity, so the two are equal.
	sh := func(i int, name uint32, typ elf.SectionType, flags elf.SectionFlag, fileOff, size uint64, link, info uint32, addralign, entsize uint64) {
		o := shoff + uint64(i)*shdrSize
		addr := fileOff
		if flags&elf.SHF_ALLOC == 0 {
			addr = 0
		}
		w.u32(o, name)
		w.u32(o+4, uint32(typ))
		w.u64(o+8, uint64(flags))
		w.u64(o+16, addr)
		w.u64(o+24, fileOff)
		w.u64(o+32, size)
		w.u32(o+40, link)
		w.u32(o+44, info)
		w.u64(o+48, addralign)
		w.u64(o+56, entsize)
	}
	const alloc = elf.SHF_ALLOC
	sh(0, 0, elf.SHT_NULL, 0, 0, 0, 0, 0, 0, 0)
	sh(1, nameHash, elf.SHT_HASH, alloc, hashOff, uint64(hashSize), 2, 0, 8, 4)
	sh(2, nameDynsym, elf.SHT_DYNSYM, alloc, dynsymOff, uint64(dynsymSize), 3, 1, 8, symSize)
	sh(3, nameDynstr, elf.SHT_STRTAB, alloc, dynstrOff, uint64(dynstr.len()), 0, 0, 1, 0)
	sh(4, nameVersym, elf.SHT_GNU_VERSYM, alloc, versymOff, uint64(versymSize), 2, 0, 2, 2)
	sh(5, nameVerdef, elf.SHT_GNU_VERDEF, alloc, verdefOff, uint64(verdefSz), 3, uint32(verdefNum), 8, 0)
	sh(6, nameText, elf.SHT_PROGBITS, alloc|elf.SHF_EXECINSTR, textOff, uint64(textSize), 0, 0, 16, 0)
	sh(7, nameDynamic, elf.SHT_DYNAMIC, alloc|elf.SHF_WRITE, dynamicOff, dynamicSize, 3, 0, 8, dynSize)
	sh(8, nameShstr, elf.SHT_STRTAB, 0, shstrOff, uint64(shstr.len()), 0, 0, 1, 0)

	return buf, nil
}

// trapBytes returns a trapSlot-sized instruction that faults if executed. An
// architecture we don't know gets zeros, which is an illegal encoding on every
// machine we would plausibly target.
func trapBytes(machine elf.Machine) []byte {
	switch machine {
	case elf.EM_X86_64:
		return []byte{0x0f, 0x0b, 0x0f, 0x0b} // ud2; ud2
	case elf.EM_AARCH64:
		return []byte{0x20, 0x00, 0x20, 0xd4} // brk #1
	default:
		return make([]byte, trapSlot)
	}
}

// elfHash is the SysV hash used by both DT_HASH buckets and verdef vd_hash.
func elfHash(name string) uint32 {
	var h uint32
	for i := 0; i < len(name); i++ {
		h = h<<4 + uint32(name[i])
		g := h & 0xf0000000
		if g != 0 {
			h ^= g >> 24
		}
		h &^= g
	}
	return h
}

type strtab struct {
	buf bytes.Buffer
	off map[string]uint32
}

func (s *strtab) add(str string) uint32 {
	if s.off == nil {
		s.off = map[string]uint32{}
	}
	if o, ok := s.off[str]; ok {
		return o
	}
	o := uint32(s.buf.Len())
	s.buf.WriteString(str)
	s.buf.WriteByte(0)
	s.off[str] = o
	return o
}

func (s *strtab) bytes() []byte { return s.buf.Bytes() }
func (s *strtab) len() int      { return s.buf.Len() }

// nameAt recovers the string a strtab offset points at (used for verdef hashes).
func nameAt(s *strtab, off uint32) string {
	b := s.bytes()[off:]
	name, _, _ := bytes.Cut(b, []byte{0})
	return string(name)
}

type imageWriter struct{ buf []byte }

func (w *imageWriter) u16(off uint64, v uint16) { binary.LittleEndian.PutUint16(w.buf[off:], v) }
func (w *imageWriter) u32(off uint64, v uint32) { binary.LittleEndian.PutUint32(w.buf[off:], v) }
func (w *imageWriter) u64(off uint64, v uint64) { binary.LittleEndian.PutUint64(w.buf[off:], v) }

func align(v, a uint64) uint64 { return (v + a - 1) &^ (a - 1) }

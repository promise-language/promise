package module

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/promise-language/promise/compiler/internal/types"
)

// CacheKind identifies what artifact a CacheMeta sidecar describes.
type CacheKind string

const (
	// CacheKindLLVMModule is the compiled object/bitcode for a whole Promise module.
	// "LLVM module" distinguishes this from the Promise-language module concept.
	CacheKindLLVMModule CacheKind = "llvm_module"
	// CacheKindInstance is the compiled object/bitcode for a single generic type instantiation.
	CacheKindInstance CacheKind = "instance"
	// CacheKindBinary is a fully linked test binary.
	CacheKindBinary CacheKind = "test_binary"
)

// CacheMeta is the unified JSON sidecar stored alongside every cached artifact
// (.o, .bin). All artifacts share this struct; kind-specific fields are
// populated only for the relevant kind and omitted (zero/nil) otherwise.
//
// Stored as <cacheKey>.o.meta (next to <cacheKey>.o) or
//
//	<cacheKey>.bin.meta (next to <cacheKey>.bin).
type CacheMeta struct {
	Kind      CacheKind `json:"kind"`
	Name      string    `json:"name"`       // human-readable: module IR prefix, mono name, or source path
	CacheKey  string    `json:"cache_key"`  // the hex key used to look up the primary artifact
	CreatedAt string    `json:"created_at"` // RFC3339 UTC

	// CacheKindLLVMModule-specific
	InterfaceHash string   `json:"interface_hash,omitempty"`
	Symbols       []string `json:"symbols,omitempty"` // sorted list of public exported symbol names

	// CacheKindInstance-specific
	TypeDeclHash  string   `json:"type_decl_hash,omitempty"`
	ArgDeclHashes []string `json:"arg_decl_hashes,omitempty"` // T1115: sorted DeclHashes of user types reachable from type args
	IRPrefix      string   `json:"ir_prefix,omitempty"`

	// CacheKindBinary-specific
	E2E              bool                `json:"e2e,omitempty"`
	ExpectedOutput   string              `json:"expected_output,omitempty"`
	ExcludeTargets   []string            `json:"exclude_targets,omitempty"`
	Tests            []string            `json:"tests,omitempty"`
	TestExcludes     map[string][]string `json:"test_excludes,omitempty"`
	ProcessTimeoutNs int64               `json:"process_timeout_ns,omitempty"` // sum of per-test timeouts + buffer
}

// saveCacheMeta atomically writes meta as JSON to path.
// Sets CreatedAt if not already set.
func saveCacheMeta(path string, meta *CacheMeta) error {
	m := *meta // copy so we don't mutate the caller's struct
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// loadCacheMeta reads and parses a JSON sidecar.
// Returns nil if not found or invalid.
func loadCacheMeta(path string) *CacheMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m CacheMeta
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

// CollectModuleSources walks a module directory recursively and returns all .pr
// source files. Subdirectories containing their own promise.toml (nested modules)
// are excluded. When includeTests is false, *_test.pr files are skipped.
// Returns sorted absolute paths for deterministic ordering.
func CollectModuleSources(modDir string, includeTests bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(modDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == modDir {
				return err // propagate root dir errors
			}
			return nil
		}
		if d.IsDir() && path != modDir {
			// Skip nested modules (subdirs with their own promise.toml)
			if _, err := os.Stat(filepath.Join(path, "promise.toml")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".pr") {
			return nil
		}
		if !includeTests && strings.HasSuffix(d.Name(), "_test.pr") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cannot walk module directory: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

// HashModuleSources computes the implementation hash from the module's source files.
// It walks the module directory recursively (excluding nested modules) and hashes
// all .pr files. When includeTests is false, *_test.pr files are excluded from the
// hash — used for non-test builds. When true, test files are included — used for
// test build caching.
func HashModuleSources(modDir string, includeTests bool) (string, error) {
	files, err := CollectModuleSources(modDir, includeTests)
	if err != nil {
		return "", err
	}

	h := fnv.New128a()
	for _, path := range files {
		// Use relative path as separator for determinism across machines
		rel, err := filepath.Rel(modDir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", rel, err)
		}
		fmt.Fprintf(h, "file:%s\n", rel)
		h.Write(content)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashModuleInterface computes the interface hash from a module's exported scope.
// It produces a deterministic string representation of all public symbols:
// function signatures, type fields+methods, enum variants+methods.
func HashModuleInterface(exported *types.Scope) string {
	if exported == nil {
		return hashString("")
	}

	var b strings.Builder
	names := exported.Names()
	sort.Strings(names)

	for _, name := range names {
		obj := exported.Lookup(name)
		switch o := obj.(type) {
		case *types.Func:
			sig := o.Type().(*types.Signature)
			fmt.Fprintf(&b, "func %s%s\n", name, sig.String())

		case *types.TypeName:
			switch t := o.Type().(type) {
			case *types.Named:
				fmt.Fprintf(&b, "type %s", name)
				writeTypeParams(&b, t.TypeParams())
				b.WriteByte('\n')
				// Fields
				for _, f := range t.Fields() {
					if f.IsExported() {
						fmt.Fprintf(&b, "  field %s %s\n", f.Name(), f.Type().String())
					}
				}
				// Methods (public only)
				for _, m := range t.Methods() {
					if m.IsExported() {
						fmt.Fprintf(&b, "  method %s%s\n", m.Name(), m.Sig().String())
					}
				}

			case *types.Enum:
				fmt.Fprintf(&b, "enum %s", name)
				writeTypeParams(&b, t.TypeParams())
				b.WriteByte('\n')
				// Variants
				for _, v := range t.Variants() {
					fmt.Fprintf(&b, "  variant %s", v.Name())
					for _, f := range v.Fields() {
						if f.Name() != "" {
							fmt.Fprintf(&b, " %s:%s", f.Name(), f.Type().String())
						} else {
							fmt.Fprintf(&b, " %s", f.Type().String())
						}
					}
					b.WriteByte('\n')
				}
				// Methods (public only)
				for _, m := range t.Methods() {
					if m.IsExported() {
						fmt.Fprintf(&b, "  method %s%s\n", m.Name(), m.Sig().String())
					}
				}
			}
		}
	}

	return hashString(b.String())
}

func writeTypeParams(b *strings.Builder, params []*types.TypeParam) {
	if len(params) == 0 {
		return
	}
	b.WriteByte('[')
	for i, p := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Obj().Name())
	}
	b.WriteByte(']')
}

func hashString(s string) string {
	h := fnv.New128a()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// BuildCacheKey combines a module's source hash with its build context to produce
// a cache key. The build context includes the compiler binary hash, target triple,
// sorted list of all module paths in the build, and dependency hashes. This ensures
// a module's cached artifact is only reused when compiled in the exact same context
// (same co-modules, same compiler, same target, same dependency state).
//
// depHashes is a list of "name:hash" strings identifying each direct dependency's
// content. Typically the hash is the dependency's ImplHash (source content hash),
// which is a conservative proxy for InterfaceHash — any source change in a
// dependency invalidates the consumer's cache. Catalog module dependencies are
// already covered by compilerHash (they're embedded in the binary).
func BuildCacheKey(implHash, compilerHash, target, buildMode string, depHashes []string) string {
	h := fnv.New128a()
	fmt.Fprintf(h, "impl:%s\n", implHash)
	fmt.Fprintf(h, "compiler:%s\n", compilerHash)
	fmt.Fprintf(h, "target:%s\n", target)
	fmt.Fprintf(h, "mode:%s\n", buildMode)
	sortedDeps := make([]string, len(depHashes))
	copy(sortedDeps, depHashes)
	sort.Strings(sortedDeps)
	for _, d := range sortedDeps {
		fmt.Fprintf(h, "dep:%s\n", d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CacheKeyInput represents a single input that was hashed into a cache key.
// Used for debug logging when PROMISE_CACHE_DEBUG is set.
type CacheKeyInput struct {
	Label string // e.g., "impl", "compiler", "target", "dep ./mymod"
	Value string // the hash or value that was hashed
}

// FormatCacheKeyInputs formats a list of cache key inputs for debug logging.
// Returns a multi-line string showing each input label and its value (truncated
// to 16 hex chars for readability).
func FormatCacheKeyInputs(name, cacheKey string, inputs []CacheKeyInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[cache DEBUG] %s\n", name)
	for _, inp := range inputs {
		v := inp.Value
		if len(v) > 16 {
			v = v[:16]
		}
		fmt.Fprintf(&b, "  %s: %s\n", inp.Label, v)
	}
	fmt.Fprintf(&b, "  key: %s", cacheKey)
	return b.String()
}

// InstanceCacheKey produces a cache key for a single generic type instantiation.
// The key is derived from:
//   - irPrefix: the module's IR symbol prefix (disambiguates same-named types
//     from different modules; empty for types in the main file)
//   - monoName: the mangled instance name including type args (e.g., "Vector[int]")
//   - typeDeclHash: FNV-128a hash of the TypeDecl/EnumDecl AST — covers all
//     fields, methods, variants, and their bodies, but NOT unrelated declarations
//     in the same file
//   - argDeclHashes: DeclHashes of every user type transitively reachable from
//     this instance's type ARGUMENTS (T1115). For a container like
//     Map[int, CollideName] the instance's compiled IR references the element
//     type's drop/clone/method symbols, but typeDeclHash is the container's own
//     hash and monoName encodes the element by NAME only. Without these,
//     two programs defining a same-named/different-bodied element type collide
//     in the shared cache → undefined-symbol link errors or wrong-code. The
//     caller passes this slice pre-sorted (so the key is order-independent).
//   - compilerHash: fingerprint of the compiler binary for invalidating stale entries
//   - target: LLVM target triple
//
// Two instances with the same key are guaranteed to produce identical LLVM IR.
// Changes to unrelated declarations in the same file do NOT change typeDeclHash
// and therefore do NOT invalidate the cached .o file.
//
// moduleContext is an optional sorted list of module IR prefixes present in the
// build. B0244: Cross-module enum clone/drop forward-declarations produce different
// IR when the same type appears in a module vs the main file (e.g., Map[string, JsonValue]
// references __mod_json_JsonValue.clone in cross-module builds but JsonValue.clone in
// module-internal tests). Including moduleContext ensures these contexts get separate
// cache entries.
func InstanceCacheKey(irPrefix, monoName, typeDeclHash string, argDeclHashes []string, compilerHash, target, buildMode string, moduleContext []string) string {
	h := fnv.New128a()
	fmt.Fprintf(h, "instance\n")
	fmt.Fprintf(h, "prefix:%s\n", irPrefix)
	fmt.Fprintf(h, "mono:%s\n", monoName)
	fmt.Fprintf(h, "decl:%s\n", typeDeclHash)
	for _, ah := range argDeclHashes {
		fmt.Fprintf(h, "argdecl:%s\n", ah)
	}
	fmt.Fprintf(h, "compiler:%s\n", compilerHash)
	fmt.Fprintf(h, "target:%s\n", target)
	fmt.Fprintf(h, "mode:%s\n", buildMode)
	for _, mc := range moduleContext {
		fmt.Fprintf(h, "modctx:%s\n", mc)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// compilerHashOnce memoizes CompilerHash — the sidecar read is cheap but
// no reason to repeat it within a single process.
var (
	compilerIdentityOnce sync.Once
	compilerIdentityVal  string
)

// compilerIdentityBytes is how much of the binary CompilerIdentity hashes. It
// matches the window cmd/internal/buildid reads for the same purpose.
const compilerIdentityBytes = 32 * 1024

// goBuildIDMarker precedes the Go build ID inside the binary. Every Go link
// emits one, and its presence in the hashed window is the precondition that
// makes hashing a prefix sound — see CompilerIdentity.
var goBuildIDMarker = []byte("\xff Go build ID: \"")

// CompilerIdentity returns a stable 128-bit fingerprint of the running compiler
// binary, as 32 hex characters: the SHA-256 of its first 32 KB, truncated. It is
// THE identity of this compiler. Every cache keyed on "which compiler produced
// this" uses it, and nothing else fingerprints the binary.
//
// It is derived from the binary's own bytes, so it travels with the binary. That
// is what a sidecar file could not do: `promise install` copies the executable
// alone, so a hash written beside it was lost on install, and the same bytes
// then reported a different identity depending on how they had been obtained.
//
// Hashing a prefix identifies the whole binary because that window carries
// values derived from all of it:
//
//   - the Go build ID, whose trailing component is contentID — the toolchain's
//     own hash of the finished binary, stored precisely so readers can identify
//     it "with minimal I/O, instead of reading and hashing the entire file".
//   - on Mach-O, LC_UUID, which the linker derives from the output.
//
// Measured: two binaries differing only in a same-size payload 660 KB deep —
// identical file size, so no header offsets shift — differ in 91 bytes within
// the first 32 KB (75 in the build ID, 16 in LC_UUID). A cold-cache rebuild of
// an unchanged tree reproduces the binary byte for byte, so identity is stable
// across rebuilds and caches survive them.
//
// A missing marker is treated as a fatal bug, NOT as a reason to fall back to
// hashing the whole file. Falling back would be a second code path taken only
// when the first silently stopped being valid — the failure mode nobody
// notices. If this ever fires, the assumption above has broken and the fix is to
// widen the window or change the scheme deliberately, not to paper over it at
// runtime. A full build reaches 200 MB, so hashing everything on every
// invocation is not the cheap safe default it looks like.
func CompilerIdentity() string {
	compilerIdentityOnce.Do(func() {
		id, err := computeCompilerIdentity()
		if err != nil {
			// Every compiler-keyed cache hangs on this value. Continuing with a
			// placeholder would make unrelated binaries share one namespace and
			// serve each other's artifacts, so there is nothing safe to go on
			// with.
			fmt.Fprintf(os.Stderr, "fatal: cannot determine this compiler's identity: %v\n", err)
			os.Exit(1)
		}
		compilerIdentityVal = id
	})
	return compilerIdentityVal
}

func computeCompilerIdentity() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return compilerIdentityOf(exe)
}

// compilerIdentityOf computes the identity of the binary at path. Split from
// CompilerIdentity so it is testable without being the running executable.
func compilerIdentityOf(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, compilerIdentityBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	buf = buf[:n]

	if !bytes.Contains(buf, goBuildIDMarker) {
		return "", fmt.Errorf("no Go build ID in the first %d bytes of %s: the "+
			"identity would then describe only headers, and two different "+
			"compilers could share it", compilerIdentityBytes, path)
	}

	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])[:32], nil
}

// BuildCacheDir returns the build cache directory (~/.promise/cache/build/ by default).
// Uses PromiseHome() which respects PROMISE_HOME env var.
// Creates it if it doesn't exist.
func BuildCacheDir() (string, error) {
	home, err := PromiseHome()
	if err != nil {
		return "", fmt.Errorf("cannot determine Promise home: %w", err)
	}
	dir := filepath.Join(home, "cache", "build")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cannot create build cache: %w", err)
	}
	return dir, nil
}

// BuildCachePath returns the path for a cached .o file in the build cache.
// Uses a two-level directory structure (first 2 hex chars of the cache key as subdirectory)
// to avoid slow directory lookups when thousands of entries accumulate.
// E.g., key "a3b4c5..." -> "<cacheDir>/a3/a3b4c5...o"
func BuildCachePath(cacheDir, cacheKey string) string {
	subdir := cacheKey[:2]
	return filepath.Join(cacheDir, subdir, cacheKey+".o")
}

// BuildCacheMetaPath returns the sidecar metadata path for a cached .o file.
func BuildCacheMetaPath(cacheDir, cacheKey string) string {
	subdir := cacheKey[:2]
	return filepath.Join(cacheDir, subdir, cacheKey+".o.meta")
}

// TestBinaryCachePath returns the path for a cached test binary in the build cache.
func TestBinaryCachePath(cacheDir, cacheKey string) string {
	subdir := cacheKey[:2]
	return filepath.Join(cacheDir, subdir, cacheKey+".bin")
}

// LookupTestBinaryCache checks if a cached test binary exists.
// Returns the path if found, empty string if not cached.
func LookupTestBinaryCache(cacheDir, cacheKey string) string {
	path := TestBinaryCachePath(cacheDir, cacheKey)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

// SaveTestBinaryCache stores a compiled test binary in the build cache.
// Uses executable permissions (0755) and atomic write with a unique temp file
// to prevent corruption when concurrent processes save the same cache key.
func SaveTestBinaryCache(cacheDir, cacheKey, binaryFile string) error {
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if err := os.MkdirAll(subdir, 0755); err != nil {
		return fmt.Errorf("cannot create cache subdir: %w", err)
	}

	data, err := os.ReadFile(binaryFile)
	if err != nil {
		return fmt.Errorf("cannot read binary file: %w", err)
	}

	binPath := TestBinaryCachePath(cacheDir, cacheKey)
	tmp, err := os.CreateTemp(subdir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("cannot write cached binary: %w", err)
	}
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("cannot set binary permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot finalize cached binary: %w", err)
	}
	return nil
}

// LookupBuildCache checks if a cached .o file exists in the build cache.
// Returns the path if found, empty string if not cached.
func LookupBuildCache(cacheDir, cacheKey string) string {
	path := BuildCachePath(cacheDir, cacheKey)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

// SaveBuildCache stores a compiled .o and interface hash in the build cache.
// Creates the two-level subdirectory if needed. Uses atomic write (unique temp
// file + rename) to prevent corruption from concurrent builds.
func SaveBuildCache(cacheDir, cacheKey string, meta *CacheMeta, objFile string) error {
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if err := os.MkdirAll(subdir, 0755); err != nil {
		return fmt.Errorf("cannot create cache subdir: %w", err)
	}

	// Atomic write of the object file
	data, err := os.ReadFile(objFile)
	if err != nil {
		return fmt.Errorf("cannot read object file: %w", err)
	}

	objPath := BuildCachePath(cacheDir, cacheKey)
	tmpObj, err := os.CreateTemp(subdir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpObjPath := tmpObj.Name()
	if _, err := tmpObj.Write(data); err != nil {
		tmpObj.Close()
		os.Remove(tmpObjPath)
		return fmt.Errorf("cannot write cached object: %w", err)
	}
	if err := tmpObj.Close(); err != nil {
		os.Remove(tmpObjPath)
		return fmt.Errorf("cannot close temp file: %w", err)
	}
	if err := os.Rename(tmpObjPath, objPath); err != nil {
		os.Remove(tmpObjPath)
		return fmt.Errorf("cannot finalize cached object: %w", err)
	}

	// Atomic write of the sidecar metadata
	if err := saveCacheMeta(BuildCacheMetaPath(cacheDir, cacheKey), meta); err != nil {
		os.Remove(objPath)
		return fmt.Errorf("cannot write cache metadata: %w", err)
	}

	return nil
}

// ReadBuildCacheMeta reads the sidecar metadata for a cached object.
// Returns nil if not found or invalid.
func ReadBuildCacheMeta(cacheDir, cacheKey string) *CacheMeta {
	return loadCacheMeta(BuildCacheMetaPath(cacheDir, cacheKey))
}

// --- Compiler stamp: tracks which binary populated the extraction caches ---

// compilerStampFile is the filename within ~/.promise/cache/ that records which
// compiler binary last extracted embedded resources (LLVM tools, CRT).
const compilerStampFile = ".compiler_hash"

// CompilerStampPath returns the path to the compiler stamp file.
func CompilerStampPath() (string, error) {
	home, err := PromiseHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "cache", compilerStampFile), nil
}

// WriteCompilerStamp records the current compiler identity as the one that
// populated the extraction caches. Atomic, so a concurrent reader sees either
// the old identity or the new one.
func WriteCompilerStamp(identity string) error {
	path, err := CompilerStampPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".stamp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(identity + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// CompilerChanged reports whether the running compiler differs from the one that
// last populated the extraction caches, and returns the current identity.
//
// A plain string comparison against the stamp. This used to be a size+mtime fast
// path guarding a content hash, to avoid hashing the binary; CompilerIdentity is
// computed once per process anyway, so the two-level scheme bought nothing.
func CompilerChanged() (changed bool, identity string) {
	identity = CompilerIdentity()
	path, err := CompilerStampPath()
	if err != nil {
		return true, identity
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true, identity
	}
	return strings.TrimSpace(string(data)) != identity, identity
}

// EmbeddedModuleCacheDir returns a persistent cache directory for an embedded
// catalog module: ~/.promise/cache/embedded_modules/<compilerIdentity>/<name>/
//
// The compiler hash in the path is what makes the cache safe across concurrent
// processes (T1616). Previously the path was .../embedded_modules/<name>/ and a
// binary whose stamp had changed wiped the whole embedded_modules tree on
// startup — including the staging dirs peers were extracting into, which
// surfaced as "open .../embedded_modules/.std.tmp.<pid>.<n>/bool.pr: no such
// file or directory" in an unrelated test. Two differently-hashed binaries now
// simply use different subtrees, so neither has any reason to touch the other's
// and the wipe is gone rather than merely made safer.
//
// CompilerHash is memoized and reads a sidecar written by the build, so this
// costs nothing per call.
func EmbeddedModuleCacheDir(name string) (string, error) {
	home, err := PromiseHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "cache", "embedded_modules", CompilerIdentity(), name), nil
}

// CleanEmbeddedModuleCache removes all cached embedded catalog modules, for
// every compiler hash. This is the explicit `promise clean` path, not something
// a normal run does.
//
// It renames the tree aside before deleting it. os.RemoveAll walks and unlinks
// entry by entry, so a concurrent reader can observe a module directory that
// exists but is half-empty; after an atomic rename the tree either is at that
// path or is not (T1616).
func CleanEmbeddedModuleCache() error {
	home, err := PromiseHome()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, "cache", "embedded_modules")
	trash := filepath.Join(home, "cache", fmt.Sprintf(".embedded_modules.trash.%d.%d",
		os.Getpid(), tmpTrashSeq.Add(1)))
	if err := os.Rename(dir, trash); err != nil {
		if os.IsNotExist(err) {
			return nil // nothing cached yet
		}
		// Rename can fail across filesystems or on a locked tree; deleting in
		// place is still better than leaving a stale cache behind.
		return os.RemoveAll(dir)
	}
	return os.RemoveAll(trash)
}

// tmpTrashSeq makes rename-aside targets unique within a process, complementing
// the PID which makes them unique across processes.
var tmpTrashSeq atomic.Uint64

// CleanLLVMCache removes all cached LLVM tool extractions, including the
// content-addressed view dirs (T0769). The CAS blobs themselves are
// content-addressed and never stale, so they are intentionally NOT removed here.
func CleanLLVMCache() error {
	home, err := PromiseHome()
	if err != nil {
		return err
	}
	err = os.RemoveAll(filepath.Join(home, "cache", "llvm"))
	if e := os.RemoveAll(filepath.Join(home, "cache", "llvm-view")); e != nil && err == nil {
		err = e
	}
	return err
}

// CleanCRTCache removes all cached CRT extractions (musl, WASM) and the
// compiler-rt builtins archive, including the content-addressed view dirs
// (T0769, T1676). CAS blobs are left intact.
//
// The builtins archive belongs here for the same reason the musl CRT does: it
// is an unconditional input to every Linux link, extracted from the compiler
// binary, so a compiler change must be able to invalidate it. (Its cache dir is
// additionally size-validated against the embedded copy — this is belt and
// braces, matching the musl CRT.)
func CleanCRTCache() error {
	home, err := PromiseHome()
	if err != nil {
		return err
	}
	var firstErr error
	for _, dir := range []string{"crt", "crt-view", "compiler-rt", "compiler-rt-view"} {
		if e := os.RemoveAll(filepath.Join(home, "cache", dir)); e != nil && firstErr == nil {
			firstErr = e
		}
	}
	return firstErr
}

// CleanBuildCache removes all entries from the build cache.
func CleanBuildCache() error {
	dir, err := BuildCacheDir()
	if err != nil {
		return err
	}
	return CleanAll(dir)
}

// CleanAll removes all entries from a cache directory, including subdirectories.
// Preserves .lock files to avoid breaking flock held by concurrent processes.
func CleanAll(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.Name() == ".lock" {
			continue
		}
		path := filepath.Join(cacheDir, e.Name())
		if e.IsDir() {
			os.RemoveAll(path)
		} else {
			os.Remove(path)
		}
	}
	return nil
}

// HashFile computes an FNV-128a hash of a single file's content.
func HashFile(filename string) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	h := fnv.New128a()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashDir computes a deterministic FNV-128a hash of all files matching suffix
// in dir (flat, not recursive, sorted by name).
func HashDir(dir, suffix string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	h := fnv.New128a()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "file:%s\n", e.Name())
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// IsGlobPattern returns true if path contains glob metacharacters (*, ?, [).
func IsGlobPattern(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

// embedAnnotationRe matches `embed("...") annotations in Promise source.
var embedAnnotationRe = regexp.MustCompile("`embed\\(\"([^\"]+)\"")

// HashEmbedFiles scans source content for `embed("...") annotations, resolves
// the referenced files relative to dir, and returns a sorted list of "path:hash"
// entries. Handles single files, directory trees (... suffix), and glob patterns.
// Returns (entries, ok). If ok is false, a referenced file could not be read.
func HashEmbedFiles(content []byte, dir string) ([]string, bool) {
	matches := embedAnnotationRe.FindAllSubmatch(content, -1)
	if len(matches) == 0 {
		return nil, true
	}

	var hashes []string
	for _, m := range matches {
		embedPath := string(m[1])
		entries, ok := hashEmbedPath(embedPath, dir)
		if !ok {
			return nil, false
		}
		hashes = append(hashes, entries...)
	}

	sort.Strings(hashes)
	return hashes, true
}

// HashEmbedFilesForInputs is like HashEmbedFiles but returns CacheKeyInput entries
// for debug logging. Silently skips unreadable files.
func HashEmbedFilesForInputs(content []byte, dir string) []CacheKeyInput {
	matches := embedAnnotationRe.FindAllSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}

	var inputs []CacheKeyInput
	for _, m := range matches {
		embedPath := string(m[1])
		entries, ok := hashEmbedPath(embedPath, dir)
		if !ok {
			continue
		}
		for _, e := range entries {
			if idx := strings.IndexByte(e, ':'); idx >= 0 {
				inputs = append(inputs, CacheKeyInput{
					Label: "embed " + e[:idx], Value: e[idx+1:],
				})
			}
		}
	}

	return inputs
}

// HashModuleEmbeds scans all .pr files in a module for `embed annotations,
// resolves and hashes all referenced files, and returns a combined FNV-128a hash.
// Returns empty string if no embeds are found. When includeTests is true,
// *_test.pr files are also scanned for embed annotations.
func HashModuleEmbeds(modDir string, includeTests bool) string {
	files, err := CollectModuleSources(modDir, includeTests)
	if err != nil {
		return ""
	}

	var allHashes []string
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fileDir := filepath.Dir(path)
		entries, ok := HashEmbedFiles(content, fileDir)
		if !ok {
			return ""
		}
		allHashes = append(allHashes, entries...)
	}

	if len(allHashes) == 0 {
		return ""
	}

	sort.Strings(allHashes)
	h := fnv.New128a()
	for _, entry := range allHashes {
		fmt.Fprintf(h, "embed:%s\n", entry)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashEmbedPath resolves a single embed path and returns "relpath:hash" entries.
func hashEmbedPath(embedPath, dir string) ([]string, bool) {
	if strings.HasSuffix(embedPath, "...") {
		return hashEmbedDir(embedPath, dir)
	}
	if IsGlobPattern(embedPath) {
		return hashEmbedGlob(embedPath, dir)
	}
	absPath := filepath.Join(dir, embedPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, false
	}
	fh := fnv.New128a()
	fh.Write(data)
	return []string{embedPath + ":" + hex.EncodeToString(fh.Sum(nil))}, true
}

// hashEmbedDir hashes all files in a directory tree (embed path ending with ...).
func hashEmbedDir(embedPath, dir string) ([]string, bool) {
	dirPath := strings.TrimSuffix(embedPath, "...")
	dirPath = strings.TrimRight(dirPath, "/")
	if dirPath == "" {
		dirPath = "."
	}
	absDir := filepath.Join(dir, dirPath)

	var hashes []string
	err := filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != absDir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(dir, path)
		relPath = filepath.ToSlash(relPath)
		fh := fnv.New128a()
		fh.Write(data)
		hashes = append(hashes, relPath+":"+hex.EncodeToString(fh.Sum(nil)))
		return nil
	})
	if err != nil {
		return nil, false
	}
	return hashes, true
}

// hashEmbedGlob hashes all files matching a glob pattern.
func hashEmbedGlob(embedPath, dir string) ([]string, bool) {
	matches, err := filepath.Glob(filepath.Join(dir, embedPath))
	if err != nil {
		return nil, false
	}
	sort.Strings(matches)

	var hashes []string
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			continue
		}
		if strings.HasPrefix(filepath.Base(path), ".") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		relPath, _ := filepath.Rel(dir, path)
		relPath = filepath.ToSlash(relPath)
		fh := fnv.New128a()
		fh.Write(data)
		hashes = append(hashes, relPath+":"+hex.EncodeToString(fh.Sum(nil)))
	}
	return hashes, true
}

// TestBinaryMetaPath returns the sidecar metadata path for a cached test binary.
func TestBinaryMetaPath(cacheDir, cacheKey string) string {
	return TestBinaryCachePath(cacheDir, cacheKey) + ".meta"
}

// SaveTestBinaryMeta writes test binary metadata to the cache.
// Uses atomic write with a unique temp file for concurrent safety.
func SaveTestBinaryMeta(cacheDir, cacheKey string, meta *CacheMeta) error {
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if err := os.MkdirAll(subdir, 0755); err != nil {
		return err
	}
	return saveCacheMeta(TestBinaryMetaPath(cacheDir, cacheKey), meta)
}

// LoadTestBinaryMeta reads cached test binary metadata.
// Returns nil if not found or invalid.
func LoadTestBinaryMeta(cacheDir, cacheKey string) *CacheMeta {
	return loadCacheMeta(TestBinaryMetaPath(cacheDir, cacheKey))
}

// LockBuildDirShared acquires a shared flock on the build cache directory.
// Multiple test/build processes can hold shared locks concurrently — the
// content-addressed cache is safe for concurrent reads and atomic writes.
// Only promise clean acquires an exclusive lock, waiting for all shared
// holders to finish before clearing the cache.
// Returns an unlock function (call via defer). No-op if locking fails.
func LockBuildDirShared() func() {
	return lockBuildDir(false, "Waiting for cache clean to finish...\n")
}

// LockBuildDirExclusive acquires an exclusive flock on the build cache directory.
// Used by promise clean to ensure no concurrent test/build processes are
// accessing the cache while it is being cleared.
// Returns an unlock function (call via defer). No-op if locking fails.
func LockBuildDirExclusive() func() {
	return lockBuildDir(true, "Waiting for concurrent build to finish...\n")
}

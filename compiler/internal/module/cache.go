package module

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"djabi.dev/go/promise_lang/internal/types"
)

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

	h := sha256.New()
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
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// BuildCacheKey combines a module's source hash with its build context to produce
// a cache key. The build context includes the compiler binary hash, target triple,
// and sorted list of all module paths in the build. This ensures a module's cached
// .o is only reused when compiled in the exact same context (same co-modules,
// same compiler, same target).
func BuildCacheKey(implHash, compilerHash, target string, allModulePaths []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "impl:%s\n", implHash)
	fmt.Fprintf(h, "compiler:%s\n", compilerHash)
	fmt.Fprintf(h, "target:%s\n", target)
	sorted := make([]string, len(allModulePaths))
	copy(sorted, allModulePaths)
	sort.Strings(sorted)
	for _, p := range sorted {
		fmt.Fprintf(h, "mod:%s\n", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CompilerHash computes a hash of the compiler binary for cache invalidation.
// When the compiler changes, all cached modules are invalidated.
func CompilerHash() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	// Resolve symlinks
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return "unknown"
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
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

// BuildCacheInterfacePath returns the interface hash path in the build cache.
func BuildCacheInterfacePath(cacheDir, cacheKey string) string {
	subdir := cacheKey[:2]
	return filepath.Join(cacheDir, subdir, cacheKey+".interface")
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
// Uses executable permissions (0755) and atomic write.
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
	tmpPath := binPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot write cached binary: %w", err)
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
// Creates the two-level subdirectory if needed. Uses atomic write (temp + rename)
// to prevent concurrent builds from corrupting entries.
func SaveBuildCache(cacheDir, cacheKey, interfaceHash, objFile string) error {
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if err := os.MkdirAll(subdir, 0755); err != nil {
		return fmt.Errorf("cannot create cache subdir: %w", err)
	}

	// Atomic write: write to temp file, then rename
	data, err := os.ReadFile(objFile)
	if err != nil {
		return fmt.Errorf("cannot read object file: %w", err)
	}

	objPath := BuildCachePath(cacheDir, cacheKey)
	tmpPath := objPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		os.Remove(tmpPath) // clean up partial write
		return fmt.Errorf("cannot write cached object: %w", err)
	}
	if err := os.Rename(tmpPath, objPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot finalize cached object: %w", err)
	}

	// Write interface hash (also atomic)
	ifacePath := BuildCacheInterfacePath(cacheDir, cacheKey)
	tmpIface := ifacePath + ".tmp"
	if err := os.WriteFile(tmpIface, []byte(interfaceHash), 0644); err != nil {
		os.Remove(tmpIface) // clean up partial write
		os.Remove(objPath)  // roll back .o to keep cache consistent
		return fmt.Errorf("cannot write interface hash: %w", err)
	}
	if err := os.Rename(tmpIface, ifacePath); err != nil {
		os.Remove(tmpIface)
		os.Remove(objPath) // roll back .o to keep cache consistent
		return fmt.Errorf("cannot finalize interface hash: %w", err)
	}

	return nil
}

// ReadBuildCacheInterfaceHash reads the cached interface hash for a cache key.
// Returns empty string if not found.
func ReadBuildCacheInterfaceHash(cacheDir, cacheKey string) string {
	path := BuildCacheInterfacePath(cacheDir, cacheKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
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

// TestCacheMeta holds metadata for a cached test binary.
type TestCacheMeta struct {
	E2E            bool                `json:"e2e"`
	ExpectedOutput string              `json:"expected_output,omitempty"`
	ExcludeTargets []string            `json:"exclude_targets,omitempty"`
	Tests          []string            `json:"tests,omitempty"`
	TestExcludes   map[string][]string `json:"test_excludes,omitempty"`
}

// TestBinaryMetaPath returns the metadata path for a cached test binary.
func TestBinaryMetaPath(cacheDir, cacheKey string) string {
	return TestBinaryCachePath(cacheDir, cacheKey) + ".meta"
}

// SaveTestBinaryMeta writes test binary metadata to the cache.
func SaveTestBinaryMeta(cacheDir, cacheKey string, meta *TestCacheMeta) error {
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if err := os.MkdirAll(subdir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(TestBinaryMetaPath(cacheDir, cacheKey), data, 0644)
}

// LoadTestBinaryMeta reads cached test binary metadata.
// Returns nil if not found or invalid.
func LoadTestBinaryMeta(cacheDir, cacheKey string) *TestCacheMeta {
	data, err := os.ReadFile(TestBinaryMetaPath(cacheDir, cacheKey))
	if err != nil {
		return nil
	}
	var meta TestCacheMeta
	if json.Unmarshal(data, &meta) != nil {
		return nil
	}
	return &meta
}

// LockBuildDirShared acquires a shared flock on the build cache directory.
// Multiple test/build processes can hold shared locks concurrently — the
// content-addressed cache is safe for concurrent reads and atomic writes.
// Only promise clean acquires an exclusive lock, waiting for all shared
// holders to finish before clearing the cache.
// Returns an unlock function (call via defer). No-op if locking fails.
func LockBuildDirShared() func() {
	return lockBuildDir(syscall.LOCK_SH, "Waiting for cache clean to finish...\n")
}

// LockBuildDirExclusive acquires an exclusive flock on the build cache directory.
// Used by promise clean to ensure no concurrent test/build processes are
// accessing the cache while it is being cleared.
// Returns an unlock function (call via defer). No-op if locking fails.
func LockBuildDirExclusive() func() {
	return lockBuildDir(syscall.LOCK_EX, "Waiting for concurrent build to finish...\n")
}

func lockBuildDir(mode int, waitMsg string) func() {
	dir, err := BuildCacheDir()
	if err != nil {
		return func() {}
	}
	lockPath := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return func() {}
	}
	// Try non-blocking first to detect contention.
	err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
	if err != nil {
		fmt.Fprint(os.Stderr, waitMsg)
		if err := syscall.Flock(int(f.Fd()), mode); err != nil {
			f.Close()
			return func() {}
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

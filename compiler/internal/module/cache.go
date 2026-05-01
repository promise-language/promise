package module

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"djabi.dev/go/promise_lang/internal/types"
)

// HashModuleSources computes the implementation hash from the module's source files.
// It reads all .pr files in the module directory, sorts them by name, and hashes
// their concatenated contents.
func HashModuleSources(modDir string) (string, error) {
	entries, err := os.ReadDir(modDir)
	if err != nil {
		return "", fmt.Errorf("cannot read module directory: %w", err)
	}

	// Collect and sort .pr file names for deterministic ordering
	var prFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pr") {
			prFiles = append(prFiles, e.Name())
		}
	}
	sort.Strings(prFiles)

	h := sha256.New()
	for _, name := range prFiles {
		content, err := os.ReadFile(filepath.Join(modDir, name))
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", name, err)
		}
		// Include filename as separator to avoid hash collisions from file splits
		fmt.Fprintf(h, "file:%s\n", name)
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
func CleanAll(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		path := filepath.Join(cacheDir, e.Name())
		if e.IsDir() {
			os.RemoveAll(path)
		} else {
			os.Remove(path)
		}
	}
	return nil
}

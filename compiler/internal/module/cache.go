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

// CacheDir returns the .promise-build/ directory for the given project root.
// Creates it if it doesn't exist.
func CacheDir(projectRoot string) (string, error) {
	dir := filepath.Join(projectRoot, ".promise-build")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cannot create cache directory: %w", err)
	}
	return dir, nil
}

// CachedObjPath returns the path where a cached .o file would be stored.
func CachedObjPath(cacheDir, moduleName, cacheKey string) string {
	return filepath.Join(cacheDir, fmt.Sprintf("%s-%s.o", moduleName, cacheKey[:16]))
}

// CachedInterfaceHashPath returns the path where the interface hash is stored.
func CachedInterfaceHashPath(cacheDir, moduleName, cacheKey string) string {
	return filepath.Join(cacheDir, fmt.Sprintf("%s-%s.interface", moduleName, cacheKey[:16]))
}

// LookupCachedObj checks if a cached .o file exists for the given module and cache key.
// Returns the path if found, empty string if not cached.
func LookupCachedObj(cacheDir, moduleName, cacheKey string) string {
	path := CachedObjPath(cacheDir, moduleName, cacheKey)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

// SaveCachedObj copies the compiled .o file and interface hash to the cache.
func SaveCachedObj(cacheDir, moduleName, cacheKey, interfaceHash, objFile string) error {
	// Copy .o file to cache
	data, err := os.ReadFile(objFile)
	if err != nil {
		return fmt.Errorf("cannot read object file: %w", err)
	}
	cachePath := CachedObjPath(cacheDir, moduleName, cacheKey)
	if err := os.WriteFile(cachePath, data, 0644); err != nil {
		return fmt.Errorf("cannot write cached object: %w", err)
	}

	// Write interface hash
	ifacePath := CachedInterfaceHashPath(cacheDir, moduleName, cacheKey)
	if err := os.WriteFile(ifacePath, []byte(interfaceHash), 0644); err != nil {
		return fmt.Errorf("cannot write interface hash: %w", err)
	}

	return nil
}

// ReadCachedInterfaceHash reads the cached interface hash for a module.
// Returns empty string if not found.
func ReadCachedInterfaceHash(cacheDir, moduleName, cacheKey string) string {
	path := CachedInterfaceHashPath(cacheDir, moduleName, cacheKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// CleanStaleCache removes cached .o and .interface files for a module
// that don't match the current cache key.
func CleanStaleCache(cacheDir, moduleName, currentCacheKey string) {
	prefix := moduleName + "-"
	currentSuffix := currentCacheKey[:16]
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Keep files matching the current key
		base := strings.TrimPrefix(name, prefix)
		if strings.HasPrefix(base, currentSuffix) {
			continue
		}
		// Remove stale files for this module
		if strings.HasSuffix(name, ".o") || strings.HasSuffix(name, ".interface") {
			os.Remove(filepath.Join(cacheDir, name))
		}
	}
}

// CleanAll removes all files from the cache directory.
func CleanAll(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		os.Remove(filepath.Join(cacheDir, e.Name()))
	}
	return nil
}

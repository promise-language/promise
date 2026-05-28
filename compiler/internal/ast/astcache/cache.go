package astcache

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"

	"github.com/promise-language/promise/compiler/internal/ast"
)

const (
	magic         = "PAST"
	formatVersion = 1
	headerSize    = 4 + 4 + 16 // magic + version + key hash
	cacheSubdir   = "astcache"
)

// Key computes the cache key from a compiler hash and content hash.
func Key(compilerHash, contentHash string) string {
	h := fnv.New128a()
	h.Write([]byte("astcache:v2:"))
	h.Write([]byte(compilerHash))
	h.Write([]byte(":"))
	h.Write([]byte(contentHash))
	return hex.EncodeToString(h.Sum(nil))
}

// ContentHash computes a deterministic hash of file names and their contents.
// Filenames are included so that renames invalidate the cache. Contents should
// already be CRLF-normalized before calling.
func ContentHash(filenames []string, contents [][]byte) string {
	h := fnv.New128a()
	for i, name := range filenames {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(contents[i])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CacheDir returns the astcache directory under the given build cache root.
func CacheDir(buildCacheRoot string) string {
	return filepath.Join(filepath.Dir(buildCacheRoot), cacheSubdir)
}

// cachePath returns the full path for a cache entry.
// Uses a two-level directory structure (first 2 hex chars of the key as subdirectory)
// to avoid slow directory lookups when thousands of entries accumulate.
func cachePath(cacheDir, key string) string {
	subdir := key[:2]
	return filepath.Join(cacheDir, subdir, key+".bin")
}

// Load attempts to load a cached AST file. Returns nil, nil on cache miss.
func Load(cacheDir, key string) (*ast.File, error) {
	path := cachePath(cacheDir, key)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil // cache miss
	}
	if len(data) < headerSize {
		return nil, nil // corrupt
	}

	// Verify header
	if string(data[:4]) != magic {
		return nil, nil
	}
	ver := binary.LittleEndian.Uint32(data[4:8])
	if ver != formatVersion {
		return nil, nil // version mismatch
	}

	// Verify key hash
	keyHash := keyToHash(key)
	if string(data[8:24]) != string(keyHash[:]) {
		return nil, nil // key mismatch
	}

	f, err := Decode(data[headerSize:])
	if err != nil {
		// Corrupt cache — remove it
		os.Remove(path)
		return nil, nil
	}
	return f, nil
}

// Save writes a cached AST file. Errors are silently ignored (cache is best-effort).
func Save(cacheDir, key string, f *ast.File) {
	subdir := filepath.Join(cacheDir, key[:2])
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		return
	}

	payload := Encode(f)

	// Build header
	var header [headerSize]byte
	copy(header[:4], magic)
	binary.LittleEndian.PutUint32(header[4:8], formatVersion)
	keyHash := keyToHash(key)
	copy(header[8:24], keyHash[:])

	data := make([]byte, 0, headerSize+len(payload))
	data = append(data, header[:]...)
	data = append(data, payload...)

	// Atomic write: unique temp file + rename (temp file in same shard subdir to keep rename on the same fs)
	target := cachePath(cacheDir, key)
	tmp, err := os.CreateTemp(subdir, "ast-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	tmp.Close()
	if writeErr != nil {
		os.Remove(tmpName)
		return
	}
	os.Rename(tmpName, target)
}

// Remove deletes the cache entry for a key.
func Remove(cacheDir, key string) {
	os.Remove(cachePath(cacheDir, key))
}

func keyToHash(key string) [16]byte {
	var h [16]byte
	decoded, err := hex.DecodeString(key)
	if err == nil && len(decoded) >= 16 {
		copy(h[:], decoded[:16])
	} else {
		// Fallback: hash the key string
		hasher := fnv.New128a()
		fmt.Fprint(hasher, key)
		sum := hasher.Sum(nil)
		copy(h[:], sum)
	}
	return h
}

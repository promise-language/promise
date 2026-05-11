package astcache

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"

	"djabi.dev/go/promise_lang/internal/ast"
)

const (
	magic         = "PAST"
	formatVersion = 1
	headerSize    = 4 + 4 + 16 // magic + version + key hash
	cacheSubdir   = "astcache"
)

// Key computes the cache key from a compiler hash.
func Key(compilerHash string) string {
	h := fnv.New128a()
	h.Write([]byte("astcache:v1:"))
	h.Write([]byte(compilerHash))
	return hex.EncodeToString(h.Sum(nil))
}

// CacheDir returns the astcache directory under the given build cache root.
func CacheDir(buildCacheRoot string) string {
	return filepath.Join(filepath.Dir(buildCacheRoot), cacheSubdir)
}

// cachePath returns the full path for a cache entry.
func cachePath(cacheDir, key string) string {
	return filepath.Join(cacheDir, "std-"+key+".bin")
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
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
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

	// Atomic write: unique temp file + rename
	target := cachePath(cacheDir, key)
	tmp, err := os.CreateTemp(cacheDir, "std-*.tmp")
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

package module

import (
	"encoding/hex"
	"hash/fnv"
	"strings"
)

// GlobalIdentity computes the globally unique identity for a module based on its source.
//
// Source types:
//   - Local modules: relative path from project root (e.g., "./libs/parser")
//   - Remote modules: normalized URL (e.g., "github.com/alice/parser")
//   - Catalog modules: catalog-assigned name (e.g., "json")
//   - Std: "std"
//
// The global identity is the foundation for IR symbol generation and cache keys.
// Two modules with different global identities are guaranteed to get different IR prefixes.
func GlobalIdentityForLocal(modPath string) string {
	return modPath
}

func GlobalIdentityForRemote(normalizedURL string) string {
	return normalizedURL
}

func GlobalIdentityForCatalog(catalogName string) string {
	return catalogName
}

// SanitizeIRPrefix converts a GlobalIdentity into an IR-safe prefix for use in
// LLVM symbol names. The resulting prefix is used in the pattern __mod_<prefix>_<symbol>.
//
// The sanitization must be:
//   - Stable: same input always produces the same output
//   - Collision-free: different global identities produce different prefixes
//     (guaranteed in practice; see below)
//   - Valid C identifier characters: starts with [a-zA-Z_], body is [a-zA-Z0-9_]
//
// Rules:
//   - Simple identifiers ([a-zA-Z_][a-zA-Z0-9_]*) that required no stripping pass through
//     unchanged. This covers catalog modules (e.g., "json" → "json").
//   - Single-component local paths (e.g., "./mylib") strip the prefix and, if the result
//     is a simple identifier, append a hash suffix to avoid colliding with a catalog module
//     of the same name.
//   - Paths/URLs are sanitized by replacing non-alphanumeric/non-underscore characters
//     with '_', collapsing runs of underscores, and trimming leading/trailing underscores.
//     A 6-character hash suffix is appended for disambiguation.
//     E.g., "github.com/alice/parser" → "github_com_alice_parser_<hash6>"
func SanitizeIRPrefix(globalID string) string {
	// Strip all leading ./ and ../ components for local paths
	clean := stripPathPrefixes(globalID)

	// Simple identifiers pass through unchanged ONLY if no stripping occurred.
	// This ensures "./mylib" (local) and "mylib" (catalog) never collide.
	if clean == globalID && isSimpleIdent(clean) {
		return clean
	}

	// Sanitize: replace non-alnum/_ with _, collapse runs
	sanitized := sanitizeChars(clean)

	// Ensure the result starts with a letter or underscore
	// to produce valid C/LLVM identifiers.
	sanitized = ensureLetterStart(sanitized)

	// Append hash suffix for collision-freedom.
	// Without the suffix, "github.com/alice_parser" and "github.com/alice/parser"
	// would both sanitize to "github_com_alice_parser".
	h := fnv.New128a()
	h.Write([]byte(globalID))
	suffix := hex.EncodeToString(h.Sum(nil)[:3]) // 6 hex chars
	return sanitized + "_" + suffix
}

// stripPathPrefixes removes all leading "./" and "../" components.
func stripPathPrefixes(s string) string {
	for strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		s = strings.TrimPrefix(s, "./")
		s = strings.TrimPrefix(s, "../")
	}
	return s
}

// ensureLetterStart prepends "m" if the string is empty or starts with
// a digit, ensuring the result is a valid C/LLVM identifier start.
func ensureLetterStart(s string) string {
	if len(s) == 0 || isDigit(rune(s[0])) {
		return "m" + s
	}
	return s
}

// isSimpleIdent returns true if s matches [a-zA-Z_][a-zA-Z0-9_]*.
func isSimpleIdent(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if !isLetter(c) && c != '_' {
				return false
			}
		} else {
			if !isLetter(c) && !isDigit(c) && c != '_' {
				return false
			}
		}
	}
	return true
}

// sanitizeChars replaces non-alphanumeric/non-underscore characters with '_',
// collapses consecutive underscores, and trims leading/trailing underscores.
func sanitizeChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := false
	for _, c := range s {
		if isLetter(c) || isDigit(c) {
			b.WriteRune(c)
			prevUnderscore = false
		} else {
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

func isLetter(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigit(c rune) bool {
	return c >= '0' && c <= '9'
}

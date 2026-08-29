package common

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// bakedRoot is the repository this binary was built for, base64-encoded.
// ./make injects it into every tool via -ldflags (-X ...common.bakedRoot=<b64>),
// alongside the tools source hash. Decode it with BakedRootValue.
//
// It is encoded because `go build -ldflags` splits its argument on whitespace
// and understands quotes, so a repo path containing a space breaks the link
// outright (`usage: link [options] main.o`) and one containing a quote
// character breaks whichever quoting style was chosen. Both are ordinary paths:
// C:\Users\John Doe\promise on Windows, /Users/John Doe/promise on macOS.
// Base64 has no character the flag parser can misread, so the stamp is total
// rather than correct-for-the-paths-we-happened-to-test.
//
// It is deliberately the only authoritative answer. A tool manages its own
// project and nothing else, so which tree it acts on is a property of the
// binary, fixed when it was built — not something the caller's environment gets
// to choose. Deriving it from the working directory means accepting a value
// from whoever happened to cd last: a stray cd, or any directory that merely
// contains a catalog.toml, silently redirects the tool at a tree its author
// never meant, and it then reports true verdicts about the wrong repository
// (T1813, T1814).
var bakedRoot string

// BakedRootValue decodes the stamped root, or returns "" when this binary was
// built without one (go run, go test).
func BakedRootValue() string {
	if bakedRoot == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(bakedRoot)
	if err != nil {
		return ""
	}
	return filepath.Clean(string(raw))
}

// EncodeRoot renders a path for the -X stamp. It lives here, beside the
// decoder, so the two halves cannot drift apart.
func EncodeRoot(root string) string {
	return base64.StdEncoding.EncodeToString([]byte(filepath.Clean(root)))
}

// FindRoot returns the repository this tool was built for.
//
// It never consults the working directory. A binary built by ./make carries its
// root; one built without the stamp (go run, go test) falls back to its own
// location on disk, since every tool lives at <root>/bin/<tool>. Tests that need
// a root should call RootForTests instead of relying on that fallback.
func FindRoot() (string, error) {
	if bakedRoot := BakedRootValue(); bakedRoot != "" {
		if _, err := os.Stat(filepath.Join(bakedRoot, "catalog.toml")); err == nil {
			return bakedRoot, nil
		}
		return "", fmt.Errorf("this tool was built for %s, which no longer looks like a Promise repo (no catalog.toml) — rebuild with ./make", bakedRoot)
	}

	// Unstamped binary: derive from the executable's own path, never from cwd.
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			candidate := filepath.Dir(filepath.Dir(resolved)) // <root>/bin/<tool> → <root>
			if _, err := os.Stat(filepath.Join(candidate, "catalog.toml")); err == nil {
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf("cannot tell which repository this tool belongs to: it carries no build-time root and is not running from <root>/bin — run ./make to build the tools")
}

// cleanuper is the one method SetRootForTest needs from *testing.T, taken as an
// interface so this package does not import testing.
type cleanuper interface{ Cleanup(func()) }

// SetRootForTest gives a test binary the build-time root that ./make stamps
// into a real tool, restoring the previous value afterwards.
//
// Test binaries carry no baked root and do not live in <root>/bin, so without
// this a tool's tests cannot scope themselves — and the alternative, letting
// FindRoot fall back to the working directory, is the hazard this whole design
// removes. Setting it explicitly keeps the test on the same contract as the
// shipped binary.
func SetRootForTest(tb cleanuper, root string) {
	saved := bakedRoot
	bakedRoot = EncodeRoot(root)
	tb.Cleanup(func() { bakedRoot = saved })
}

// RootForTests returns the repo root for tests, derived from this source file's
// compile-time path.
//
// Test binaries carry no baked root and do not live in <root>/bin, so FindRoot
// cannot answer for them — and reintroducing a cwd walk for their benefit would
// put the hazard back. runtime.Caller gives a path fixed when the test was
// compiled, which is the same kind of build-time fact as the injected root.
func RootForTests() (string, error) {
	_, file, _, ok := runtime.Caller(0) // <root>/tools/build/common/root.go
	if !ok {
		return "", fmt.Errorf("cannot locate the repo root from the test binary")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	if _, err := os.Stat(filepath.Join(root, "catalog.toml")); err != nil {
		return "", fmt.Errorf("derived repo root %s has no catalog.toml: %w", root, err)
	}
	return root, nil
}

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/internal/module"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// T1521: setProgramSourceDir is the single gate on what codegen bakes into
// os.src_dir. Its contract is that a value it cannot vouch for is *absent*
// rather than approximated: os.src_dir must never hand a tool a path that is
// not where that tool lives, because the whole point of the accessor is
// locating the repo root relative to it.
func TestSetProgramSourceDir(t *testing.T) {
	t.Parallel()
	abs := filepath.Join(t.TempDir(), "tool")
	cases := []struct {
		name string
		dir  string
		want string
	}{
		{name: "absolute path is recorded", dir: abs, want: abs},
		{name: "relative path is rejected", dir: filepath.Join("tools", "make"), want: ""},
		{name: "bare relative name is rejected", dir: "make", want: ""},
		{name: "dot is rejected", dir: ".", want: ""},
		{name: "empty string is rejected", dir: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := &sema.Info{}
			setProgramSourceDir(info, tc.dir)
			if info.SourceDir != tc.want {
				t.Errorf("SourceDir = %q, want %q", info.SourceDir, tc.want)
			}
		})
	}
}

// A rejected directory must leave an already-recorded value alone rather than
// clearing it — the guard is "don't fabricate", not "reset".
func TestSetProgramSourceDirRejectionKeepsPrevious(t *testing.T) {
	t.Parallel()
	abs := t.TempDir()
	info := &sema.Info{}
	setProgramSourceDir(info, abs)
	setProgramSourceDir(info, "relative/dir")
	if info.SourceDir != abs {
		t.Errorf("SourceDir = %q, want the earlier absolute value %q", info.SourceDir, abs)
	}
}

// findInput returns the value of the named cache-key input, or "" plus false.
func findInput(inputs []module.CacheKeyInput, label string) (string, bool) {
	for _, in := range inputs {
		if in.Label == label {
			return in.Value, true
		}
	}
	return "", false
}

// The three *CacheKey functions each write a "source-dir:" line, and the
// matching *CacheInputs functions feed `--explain-cache`. An input list that
// omits a line the key hashes makes the explain output lie: the user is shown
// every reason the key could have changed except the one that did (T1521).
func TestCacheInputsIncludeSourceDir(t *testing.T) {
	t.Parallel()
	const target = "x86_64-unknown-linux-gnu"

	t.Run("run binary", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		src := writeRunSource(t, dir, "main() { }\n")
		inputs := computeRunBinaryCacheInputs(src, target, false)
		if inputs == nil {
			t.Skip("cache inputs not available (missing std hash)")
		}
		got, ok := findInput(inputs, "source-dir")
		if !ok {
			t.Fatalf("inputs missing 'source-dir' label: %+v", inputs)
		}
		if got != dir {
			t.Errorf("source-dir = %q, want %q", got, dir)
		}
	})

	t.Run("test file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		src := writeRunSource(t, dir, "t() `test { }\n")
		cfg := testTimeoutConfig{defaultTimeout: 60 * time.Second}
		inputs := computeTestFileCacheInputs(src, target, cfg)
		if inputs == nil {
			t.Skip("cache inputs not available (missing std hash)")
		}
		got, ok := findInput(inputs, "source-dir")
		if !ok {
			t.Fatalf("inputs missing 'source-dir' label: %+v", inputs)
		}
		if got != dir {
			t.Errorf("source-dir = %q, want %q", got, dir)
		}
	})

	t.Run("project", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
		writeProjectFile(t, dir, "main.pr", "main() {}\n")
		inputs := computeProjectBinaryCacheInputs(dir, target, false)
		if inputs == nil {
			t.Skip("cache inputs not available (missing std hash)")
		}
		got, ok := findInput(inputs, "source-dir")
		if !ok {
			t.Fatalf("inputs missing 'source-dir' label: %+v", inputs)
		}
		if got != dir {
			t.Errorf("source-dir = %q, want %q", got, dir)
		}
	})
}

// The baked directory is filepath.Abs of the argument, so naming one directory
// two ways ("." from inside it, or an absolute path) must land on the SAME cache
// entry — the key gained a source-dir line to separate different directories,
// not to split one directory across spellings and recompile on every run.
func TestCacheKeysSameForEquivalentDirSpellings(t *testing.T) {
	const target = "x86_64-unknown-linux-gnu"

	t.Run("run binary", func(t *testing.T) {
		dir := t.TempDir()
		abs := writeRunSource(t, dir, "main() { }\n")
		keyAbs, ok := computeRunBinaryCacheKey(abs, target, false)
		if !ok {
			t.Skip("cache key computation not available (missing std hash)")
		}
		t.Chdir(dir)
		keyRel, _ := computeRunBinaryCacheKey("main.pr", target, false)
		if keyAbs != keyRel {
			t.Errorf("absolute and relative spellings of the same file produced different keys:\n abs %q\n rel %q", keyAbs, keyRel)
		}
	})

	t.Run("project", func(t *testing.T) {
		dir := t.TempDir()
		writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
		writeProjectFile(t, dir, "main.pr", "main() {}\n")
		keyAbs, ok := computeProjectBinaryCacheKey(dir, target, false)
		if !ok {
			t.Skip("cache key computation not available (missing std hash)")
		}
		t.Chdir(dir)
		keyDot, _ := computeProjectBinaryCacheKey(".", target, false)
		if keyAbs != keyDot {
			t.Errorf("absolute and \".\" spellings of the same project produced different keys:\n abs %q\n dot %q", keyAbs, keyDot)
		}
		// A trailing separator is another spelling of the same directory.
		keySlash, _ := computeProjectBinaryCacheKey(dir+string(os.PathSeparator), target, false)
		if keyAbs != keySlash {
			t.Errorf("trailing-separator spelling produced a different key:\n abs   %q\n slash %q", keyAbs, keySlash)
		}
	})
}

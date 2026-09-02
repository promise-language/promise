package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/promise-language/promise/compiler/internal/codegen"
)

// targetSpec describes one compile target offered by `promise targets`.
type targetSpec struct {
	Triple      string `json:"triple"`
	Display     string `json:"display"`
	Description string `json:"description"`
	Native      bool   `json:"native"`
}

// knownTargets returns every triple this compiler can generate LLVM IR for.
//
// The list is a static property of the compiler: it never varies with what is
// installed or cached on the host (docs/distribution.md §1.1/§4 — dependencies
// are content-addressed blobs named by the embedded manifest, so a cold cache
// means "not fetched yet", never "unsupported"). Generating IR needs no
// sysroot, CRT or linker, so `emit-ir` validates against this set, while the
// commands that must actually link validate against supportedTargets().
//
// Triples use the same spellings codegen.HostTargetTriple() produces, so a
// cross triple and its native counterpart are byte-identical.
func knownTargets() []targetSpec {
	specs := []targetSpec{
		{Triple: "x86_64-unknown-linux-musl", Description: "Linux x86_64 (static musl)"},
		{Triple: "aarch64-unknown-linux-musl", Description: "Linux ARM64 (static musl)"},
		{Triple: "x86_64-pc-windows-msvc", Description: "Windows x86_64 (MSVC ABI)"},
		{Triple: "aarch64-pc-windows-msvc", Description: "Windows ARM64 (MSVC ABI)"},
		{Triple: "arm64-apple-macosx14.0.0", Description: "macOS ARM64"},
		{Triple: "x86_64-apple-macosx10.15.0", Description: "macOS x86_64"},
		{Triple: "wasm32-wasi", Description: "WebAssembly + WASI (runs in wasmtime / wasmer / wasmedge)"},
		{Triple: "wasm32-web", Description: "WebAssembly for browsers / Node.js (emits bootstrap .js loader)"},
	}
	host := codegen.HostTargetTriple()
	seen := false
	for i := range specs {
		specs[i].Display = hostShortName(specs[i].Triple)
		if specs[i].Triple == host {
			specs[i].Native = true
			seen = true
		}
	}
	if !seen {
		// The host triple carries a version suffix the static table cannot
		// enumerate (e.g. "arm64-apple-macosx26.0.0"). It is always known.
		specs = append(specs, targetSpec{
			Triple:      host,
			Display:     hostShortName(host),
			Description: "native host build (default when -target omitted)",
			Native:      true,
		})
	}
	return specs
}

// supportedTargets returns the targets this binary can build all the way to an
// executable — the host plus the WebAssembly targets, whose payloads ship with
// the compiler. It is deliberately a constant per platform, never a function of
// on-disk cache state, so `promise targets` cannot change its answer depending
// on what a previous build happened to leave behind.
//
// Cross-native targets are absent because their link payloads (musl CRT,
// mingw-w64 CRT, macOS SDK stubs) are not part of any release yet; they are
// added here — statically — when T0530/T0531/T0532 land. They remain valid for
// `emit-ir` in the meantime, via knownTargets().
func supportedTargets() []targetSpec {
	host := codegen.HostTargetTriple()
	return []targetSpec{
		{
			Triple:      host,
			Display:     hostShortName(host),
			Description: "native host build (default when -target omitted)",
			Native:      true,
		},
		{
			Triple:      "wasm32-wasi",
			Display:     "wasm32-wasi",
			Description: "WebAssembly + WASI (runs in wasmtime / wasmer / wasmedge)",
		},
		{
			Triple:      "wasm32-web",
			Display:     "wasm32-web",
			Description: "WebAssembly for browsers / Node.js (emits bootstrap .js loader)",
		},
	}
}

// hostShortName produces a stable user-friendly label for a host triple.
// Unknown triples are returned unchanged.
func hostShortName(triple string) string {
	arch := ""
	switch {
	case strings.HasPrefix(triple, "x86_64"):
		arch = "x86_64"
	case strings.HasPrefix(triple, "aarch64"), strings.HasPrefix(triple, "arm64"):
		arch = "arm64"
	}
	switch {
	case strings.Contains(triple, "macosx"), strings.Contains(triple, "apple"), strings.Contains(triple, "darwin"):
		if arch == "" {
			return "darwin"
		}
		return "darwin-" + arch
	case strings.Contains(triple, "linux"):
		if arch == "" {
			return "linux"
		}
		return "linux-" + arch
	case strings.Contains(triple, "windows"):
		if arch == "" {
			return "windows"
		}
		return "windows-" + arch
	}
	return triple
}

// isSupportedTarget reports whether s is the empty string (meaning "use the
// host default") or matches a triple in supportedTargets().
func isSupportedTarget(s string) bool {
	if s == "" {
		return true
	}
	for _, t := range supportedTargets() {
		if s == t.Triple {
			return true
		}
	}
	return false
}

// isKnownTarget reports whether s is the empty string (meaning "use the host
// default") or a triple this compiler can generate IR for, whether or not it
// can be linked here.
func isKnownTarget(s string) bool {
	if s == "" {
		return true
	}
	for _, t := range knownTargets() {
		if s == t.Triple {
			return true
		}
	}
	return false
}

// invalidTargetMessage builds the user-facing error for an unsupported
// -target value. Output is a single block ending in a trailing newline; the
// caller writes it to stderr and exits non-zero.
func invalidTargetMessage(bad string) string {
	var b strings.Builder
	if isKnownTarget(bad) {
		// A real triple whose link payload this release does not carry. Say so
		// precisely — reporting it as "invalid" would send the reader looking
		// for a typo that isn't there.
		fmt.Fprintf(&b, "error: target '%s' cannot be built by this release\n", bad)
		fmt.Fprintln(&b, "  it is a known target, but its link payload (sysroot / CRT) does not ship yet")
		fmt.Fprintf(&b, "  `promise emit-ir -target %s` works today — only linking is unavailable\n", bad)
	} else {
		fmt.Fprintf(&b, "error: invalid target '%s'\n", bad)
	}
	fmt.Fprintln(&b, "targets this release can build:")
	for _, t := range supportedTargets() {
		if t.Native {
			fmt.Fprintf(&b, "  %s  (native)\n", t.Triple)
		} else {
			fmt.Fprintf(&b, "  %s\n", t.Triple)
		}
	}
	fmt.Fprintln(&b, "Run `promise targets` for details.")
	return b.String()
}

// invalidEmitTargetMessage builds the user-facing error for an unknown -target
// on an IR-only command, which is constrained by knownTargets() rather than by
// what can be linked.
func invalidEmitTargetMessage(bad string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: invalid target '%s'\n", bad)
	fmt.Fprintln(&b, "targets this release can emit IR for:")
	for _, t := range knownTargets() {
		if t.Native {
			fmt.Fprintf(&b, "  %s  (native)\n", t.Triple)
		} else {
			fmt.Fprintf(&b, "  %s\n", t.Triple)
		}
	}
	return b.String()
}

// checkTargetFlag validates a user-supplied -target value. On a bad value it
// writes the formatted error to stderr and terminates the process with exit
// code 1. Call once, immediately after the surrounding subcommand has
// finished argument parsing — before any frontend or module loading work.
func checkTargetFlag(target string) {
	if isSupportedTarget(target) {
		return
	}
	fmt.Fprint(os.Stderr, invalidTargetMessage(target))
	os.Exit(1)
}

// checkEmitTargetFlag validates a -target value for a command that only needs
// to generate LLVM IR (emit-ir). IR generation needs no sysroot, CRT or linker,
// so it accepts every triple in knownTargets() — including cross targets this
// release cannot yet link. On a bad value it writes the error to stderr and
// exits 1, matching checkTargetFlag.
func checkEmitTargetFlag(target string) {
	if isKnownTarget(target) {
		return
	}
	fmt.Fprint(os.Stderr, invalidEmitTargetMessage(target))
	os.Exit(1)
}

// runTargets implements `promise targets`.
func runTargets(args []string) {
	jsonOut := false
	for _, a := range args {
		switch a {
		case "-json":
			jsonOut = true
		case "-h", "-help":
			printTargetsUsage(os.Stdout)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", a)
			printTargetsUsage(os.Stderr)
			os.Exit(1)
		}
	}
	writeTargets(os.Stdout, supportedTargets(), jsonOut)
}

// printTargetsUsage writes the `promise targets` usage line to w. Shared between
// the in-command help/error paths and the central help tree (T1006).
func printTargetsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: promise targets [-json]")
}

// writeTargets renders specs to w in either text or JSON format. Split out
// so tests can capture output through an io.Writer without redirecting
// os.Stdout.
func writeTargets(w io.Writer, specs []targetSpec, jsonOut bool) {
	if jsonOut {
		out := struct {
			Host    string       `json:"host"`
			Targets []targetSpec `json:"targets"`
		}{Host: codegen.HostTargetTriple(), Targets: specs}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	fmt.Fprintln(w, "Supported compile targets")
	fmt.Fprintln(w)

	maxDisplay, maxTriple := 0, 0
	for _, s := range specs {
		if len(s.Display) > maxDisplay {
			maxDisplay = len(s.Display)
		}
		if len(s.Triple) > maxTriple {
			maxTriple = len(s.Triple)
		}
	}
	for _, s := range specs {
		marker := ""
		if s.Native {
			marker = "  (native)"
		}
		fmt.Fprintf(w, "  %-*s  %-*s  %s%s\n", maxDisplay, s.Display, maxTriple, s.Triple, s.Description, marker)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Use:  promise build -target <triple> file.pr")
}

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

// supportedTargets returns the set of compile targets the current promise
// binary can build for. The host triple comes first (marked native) followed
// by the WebAssembly cross-targets. The slice is rebuilt on every call so the
// caller sees a fresh, mutable list.
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

// invalidTargetMessage builds the user-facing error for an unsupported
// -target value. Output is a single block ending in a trailing newline; the
// caller writes it to stderr and exits non-zero.
func invalidTargetMessage(bad string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: invalid target '%s'\n", bad)
	fmt.Fprintln(&b, "supported targets:")
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

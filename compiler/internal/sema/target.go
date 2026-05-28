package sema

import (
	"runtime"
	"strings"

	"github.com/promise-language/promise/compiler/internal/ast"
)

// HostTargetInfo returns a TargetInfo for the current host platform.
// Useful for Go unit tests that parse std files containing `target(cond)` annotations.
func HostTargetInfo() TargetInfo {
	ti := TargetInfo{}
	switch runtime.GOOS {
	case "linux":
		ti.OS = "linux"
	case "darwin":
		ti.OS = "macos"
	case "windows":
		ti.OS = "windows"
	}
	switch runtime.GOARCH {
	case "amd64":
		ti.Arch = "x86_64"
	case "arm64":
		ti.Arch = "aarch64"
	}
	return ti
}

// TargetInfo holds compile-time platform information used for `target(cond)` filtering.
// Zero value means "unknown" — no `target annotations are filtered out.
type TargetInfo struct {
	OS   string // "linux", "macos", "windows", "wasm", or ""
	Arch string // "x86_64", "aarch64", "wasm32", or ""
	Env  string // "wasi", "web", or "" (empty for native targets)
}

// ParseTargetInfo derives TargetInfo from an LLVM target triple.
// Returns zero TargetInfo if triple is empty.
//
// Supported triples:
//
//	x86_64-unknown-linux-musl  → OS=linux,   Arch=x86_64
//	x86_64-pc-windows-msvc     → OS=windows, Arch=x86_64
//	x86_64-apple-macosx14.0.0  → OS=macos,   Arch=x86_64
//	aarch64-apple-macosx14.0.0 → OS=macos,   Arch=aarch64
//	wasm32-wasi                → OS=wasm,    Arch=wasm32, Env=wasi
//	wasm32-web                 → OS=wasm,    Arch=wasm32, Env=web
func ParseTargetInfo(triple string) TargetInfo {
	if triple == "" {
		return TargetInfo{}
	}
	ti := TargetInfo{}

	// Determine OS from triple components.
	switch {
	case strings.Contains(triple, "windows"):
		ti.OS = "windows"
	case strings.Contains(triple, "apple") || strings.Contains(triple, "darwin") ||
		strings.Contains(triple, "macos"):
		ti.OS = "macos"
	case strings.Contains(triple, "linux"):
		ti.OS = "linux"
	case strings.Contains(triple, "wasm"):
		ti.OS = "wasm"
	}

	// Determine Arch from the first dash-separated component.
	if idx := strings.IndexByte(triple, '-'); idx >= 0 {
		switch triple[:idx] {
		case "x86_64":
			ti.Arch = "x86_64"
		case "aarch64", "arm64":
			ti.Arch = "aarch64"
		case "wasm32":
			ti.Arch = "wasm32"
		}
	}

	// Determine Env for WASM sub-targets.
	if ti.OS == "wasm" {
		switch {
		case strings.Contains(triple, "web"):
			ti.Env = "web"
		case strings.Contains(triple, "wasi"):
			ti.Env = "wasi"
		}
	}

	return ti
}

// matchesTarget returns true if the declaration should be compiled for the current target.
// Returns true when:
//   - c.target is zero (no filtering — unknown target or test context)
//   - the declaration has no `target annotation
//   - the declaration's `target(cond)` condition evaluates to true
func (c *Checker) matchesTarget(annotations []*ast.MetaAnnotation) bool {
	if c.target.OS == "" && c.target.Arch == "" {
		return true // zero target = no filtering
	}
	for _, ann := range annotations {
		if ann.Name == "target" && len(ann.Params) > 0 {
			return c.evalTargetExpr(ann.Params[0].Value)
		}
	}
	return true // no `target annotation — always included
}

// evalTargetExpr evaluates a `target(cond)` condition expression against c.target.
// Supported forms:
//
//	windows          — identifier
//	!windows         — logical not
//	linux || macos   — logical or
//	linux && x86_64  — logical and
//	(linux || macos) — grouping (parentheses are transparent in the AST)
func (c *Checker) evalTargetExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		return c.matchTargetIdent(e.Name)
	case *ast.UnaryExpr:
		if e.Op == ast.UnaryNot {
			return !c.evalTargetExpr(e.Operand)
		}
	case *ast.BinaryExpr:
		switch e.Op {
		case ast.BinOr:
			return c.evalTargetExpr(e.Left) || c.evalTargetExpr(e.Right)
		case ast.BinAnd:
			return c.evalTargetExpr(e.Left) && c.evalTargetExpr(e.Right)
		}
	}
	return true // unknown expression form — include by default (safe)
}

// ValidExcludeIdents is the set of valid identifier names for test(exclude:) parameters.
var ValidExcludeIdents = map[string]bool{
	"windows": true, "linux": true, "macos": true,
	"wasm": true, "wasi": true, "web": true,
	"posix": true, "x86_64": true, "aarch64": true, "arm64": true,
}

// MatchTargetIdent returns true if the target identifier matches the given TargetInfo.
// This is the public counterpart to the private matchTargetIdent method.
func MatchTargetIdent(ti TargetInfo, name string) bool {
	switch name {
	case "windows":
		return ti.OS == "windows"
	case "linux":
		return ti.OS == "linux"
	case "macos":
		return ti.OS == "macos"
	case "wasm":
		return ti.OS == "wasm"
	case "wasi":
		return ti.Env == "wasi"
	case "web":
		return ti.Env == "web"
	case "posix":
		return ti.OS == "linux" || ti.OS == "macos"
	case "x86_64":
		return ti.Arch == "x86_64"
	case "aarch64", "arm64": // arm64 is Apple's convention for the same architecture
		return ti.Arch == "aarch64"
	default:
		return false // unknown target identifier — does not match
	}
}

// matchTargetIdent returns true if the target identifier matches c.target.
//
// Supported identifiers:
//
//	windows       — OS is Windows (x86_64-pc-windows-msvc)
//	linux         — OS is Linux (any Linux triple)
//	macos         — OS is macOS/Darwin
//	wasm          — OS is WASM (either wasm32-wasi or wasm32-web)
//	wasi          — Env is WASI (wasm32-wasi)
//	web           — Env is web (wasm32-web)
//	posix         — OS is linux or macos (convenience alias for linux || macos)
//	x86_64        — Arch is x86-64
//	aarch64, arm64 — Arch is AArch64/ARM64 (arm64 is accepted as an alias)
//
// Unknown identifiers return false (does not match).
func (c *Checker) matchTargetIdent(name string) bool {
	return MatchTargetIdent(c.target, name)
}

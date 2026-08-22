//go:build !(linux && (amd64 || arm64))

package main

import "embed"

var embeddedCompilerRT embed.FS // empty — no musl compiler-rt builtins on this platform

const hasEmbeddedCompilerRT = false

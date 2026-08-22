//go:build linux && arm64

package main

import "embed"

// See compiler_rt_linux_amd64.go (T1676).
//
//go:embed resources/compiler-rt/aarch64-linux-musl/*
var embeddedCompilerRT embed.FS

const hasEmbeddedCompilerRT = true

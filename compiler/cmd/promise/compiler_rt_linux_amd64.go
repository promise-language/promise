//go:build linux && amd64

package main

import "embed"

// The staged dir always contains the real archive: EmbedCompilerRT in
// tools/build/common/resources.go is fatal on failure, so unlike OpenSSL there
// is no placeholder sentinel to tolerate here (T1676).
//
//go:embed resources/compiler-rt/x86_64-linux-musl/*
var embeddedCompilerRT embed.FS

const hasEmbeddedCompilerRT = true

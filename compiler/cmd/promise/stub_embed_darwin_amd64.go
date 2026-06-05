//go:build darwin && amd64 && embed_stub

package main

import "embed"

// embeddedStub carries the per-target Promise-built launcher stub compiled by
// `bin/release build` (T0773 step 5) and embedded back into the compiler so the
// installer can extract it (writeStubAndSidecar). Mirrors llvm_darwin_amd64.go.
//
//go:embed resources/stub/darwin-amd64/*
var embeddedStub embed.FS

const hasEmbeddedStub = true

// stubVersion is the embedded stub's contract version. Keep in lockstep with
// STUB_VERSION() in tools/stub/main.pr (currently 1).
const stubVersion = 1

const stubEmbedPrefix = "resources/stub/darwin-amd64"

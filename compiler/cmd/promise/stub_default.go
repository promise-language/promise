//go:build !embed_stub

package main

import "embed"

// embeddedStub holds the per-target Promise-built launcher stub (tools/stub).
// In dev builds (no embed_stub tag) it is empty; release builds set the
// embed_stub tag and a per-platform file (added by T0773) supplies the real
// embedded binary. See stub_embed.go for the install-time extraction logic.
var embeddedStub embed.FS

// hasEmbeddedStub reports whether this binary carries an embedded stub. Dev
// builds do not (the stub is a release-time per-target artifact), so install
// falls back to placing the compiler itself at the launcher path (T0770).
const hasEmbeddedStub = false

// stubVersion is the contract version of the embedded stub. Keep in lockstep
// with STUB_VERSION() in tools/stub/main.pr. 0 in dev builds means "no embedded
// stub", which never forward-updates an installed stub.
const stubVersion = 0

// stubEmbedPrefix is the embed.FS path prefix for the stub binary.
const stubEmbedPrefix = ""

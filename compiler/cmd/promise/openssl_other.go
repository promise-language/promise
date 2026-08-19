//go:build !(linux && (amd64 || arm64))

package main

import "embed"

var embeddedOpenSSL embed.FS // empty — no static OpenSSL on this platform

const hasEmbeddedOpenSSL = false

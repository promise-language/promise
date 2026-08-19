//go:build linux && amd64

package main

import "embed"

// The staged dir always contains at least a PLACEHOLDER sentinel (see
// EmbedOpenSSL in tools/build/common/resources.go), so this glob always
// resolves even when the real archives aren't available. Availability is keyed
// on the actual libssl.a/libcrypto.a presence, not on this constant (T1596 / #28).
//
//go:embed resources/openssl/x86_64-linux-musl/*
var embeddedOpenSSL embed.FS

const hasEmbeddedOpenSSL = true

//go:build linux && arm64

package main

import "embed"

// See openssl_linux_amd64.go for the PLACEHOLDER-sentinel rationale (T1596 / #28).
//
//go:embed resources/openssl/aarch64-linux-musl/*
var embeddedOpenSSL embed.FS

const hasEmbeddedOpenSSL = true

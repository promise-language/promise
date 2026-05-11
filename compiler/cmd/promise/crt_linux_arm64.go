//go:build linux && arm64

package main

import "embed"

//go:embed resources/crt/aarch64-linux-musl/*
var embeddedMuslCRT embed.FS

const hasEmbeddedMuslCRT = true

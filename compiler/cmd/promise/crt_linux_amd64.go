//go:build linux && amd64

package main

import "embed"

//go:embed resources/crt/x86_64-linux-musl/*
var embeddedMuslCRT embed.FS

const hasEmbeddedMuslCRT = true

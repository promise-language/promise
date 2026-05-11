//go:build !(linux && (amd64 || arm64))

package main

import "embed"

var embeddedMuslCRT embed.FS // empty — no musl CRT on this platform

const hasEmbeddedMuslCRT = false

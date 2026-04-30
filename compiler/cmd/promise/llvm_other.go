//go:build !(linux && amd64 && embed_llvm)

package main

import "embed"

var embeddedLLVM embed.FS

const hasEmbeddedLLVM = false

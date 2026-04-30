//go:build linux && amd64 && embed_llvm

package main

import "embed"

//go:embed resources/llvm/linux-amd64/*
var embeddedLLVM embed.FS

const hasEmbeddedLLVM = true

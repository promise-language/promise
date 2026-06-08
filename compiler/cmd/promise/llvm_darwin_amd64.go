//go:build darwin && amd64 && embed_llvm

package main

import "embed"

//go:embed resources/llvm/darwin-amd64/*
var embeddedLLVM embed.FS

const hasEmbeddedLLVM = true

var embeddedLLVMFiles = []string{"opt.br", "llc.br", "lld.br"}

const llvmEmbedPrefix = "resources/llvm/darwin-amd64"

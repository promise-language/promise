//go:build darwin && arm64 && embed_llvm

package main

import "embed"

//go:embed resources/llvm/darwin-arm64/*
var embeddedLLVM embed.FS

const hasEmbeddedLLVM = true

var embeddedLLVMFiles = []string{"opt.gz", "llc.gz", "lld.gz", "libLLVM.dylib.gz"}

const llvmEmbedPrefix = "resources/llvm/darwin-arm64"
const llvmCacheSubdir = "darwin-arm64"
const llvmLibEnvKey = "DYLD_LIBRARY_PATH"

//go:build linux && amd64 && embed_llvm

package main

import "embed"

//go:embed resources/llvm/linux-amd64/*
var embeddedLLVM embed.FS

const hasEmbeddedLLVM = true

var embeddedLLVMFiles = []string{"opt.gz", "llc.gz", "lld.gz", "libLLVM.so.gz"}

const llvmEmbedPrefix = "resources/llvm/linux-amd64"
const llvmCacheSubdir = "linux-amd64"
const llvmLibEnvKey = "LD_LIBRARY_PATH"

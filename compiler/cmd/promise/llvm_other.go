//go:build !embed_llvm

package main

import "embed"

var embeddedLLVM embed.FS

const hasEmbeddedLLVM = false

var embeddedLLVMFiles []string

const llvmEmbedPrefix = ""
const llvmCacheSubdir = ""
const llvmLibEnvKey = "LD_LIBRARY_PATH"

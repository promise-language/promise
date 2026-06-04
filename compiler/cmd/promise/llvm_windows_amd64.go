//go:build windows && amd64 && embed_llvm

package main

import "embed"

//go:embed resources/llvm/windows-amd64/*
var embeddedLLVM embed.FS

const hasEmbeddedLLVM = true

var embeddedLLVMFiles = []string{"opt.exe.gz", "llc.exe.gz", "lld.exe.gz"}

const llvmEmbedPrefix = "resources/llvm/windows-amd64"

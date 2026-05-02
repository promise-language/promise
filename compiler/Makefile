ANTLR_VERSION := 4.13.1
ANTLR_JAR     := tools/antlr-$(ANTLR_VERSION)-complete.jar
ANTLR_URL     := https://www.antlr.org/download/antlr-$(ANTLR_VERSION)-complete.jar
GRAMMAR_DIR   := grammar
PARSER_PKG    := internal/parser
BINARY        := ../bin/promise
RESOURCES     := cmd/promise/resources
UNAME         := $(shell uname -s)

# musl CRT paths (Linux only)
MUSL_CRT_SRC  := /usr/lib/x86_64-linux-musl
MUSL_CRT_DST  := $(RESOURCES)/crt/x86_64-linux-musl

# LLVM tool bundling (release builds only)
LLVM_DST      := $(RESOURCES)/llvm/linux-amd64

# macOS LLVM bundling — detect GOARCH for darwin-arm64 or darwin-amd64
GOARCH        := $(shell go env GOARCH 2>/dev/null || echo amd64)
LLVM_DST_DARWIN := $(RESOURCES)/llvm/darwin-$(GOARCH)

.PHONY: all generate build test clean download-antlr fmt install resources musl-crt llvm-bundle llvm-bundle-darwin release wasm-alloc

all: generate build

download-antlr: $(ANTLR_JAR)

$(ANTLR_JAR):
	@mkdir -p tools
	curl -L -o $@ $(ANTLR_URL)

generate: $(ANTLR_JAR)
	@mkdir -p $(PARSER_PKG)
	@cd $(GRAMMAR_DIR) && java -jar ../$(ANTLR_JAR) \
		-Dlanguage=Go \
		-package parser \
		-visitor \
		-o ../$(PARSER_PKG) \
		PromiseLexer.g4
	@cd $(GRAMMAR_DIR) && java -jar ../$(ANTLR_JAR) \
		-Dlanguage=Go \
		-package parser \
		-visitor \
		-lib ../$(PARSER_PKG) \
		-o ../$(PARSER_PKG) \
		PromiseParser.g4

TESTDATA_STD  := internal/testutil/testdata/std

resources:
	@cp ../catalog.toml $(RESOURCES)/catalog.toml
	@rm -rf $(RESOURCES)/modules
	@mkdir -p $(RESOURCES)/modules
	@touch $(RESOURCES)/modules/.keep
	@if [ -d ../modules ]; then for d in ../modules/*/; do [ -d "$$d" ] && cp -r "$${d%/}" $(RESOURCES)/modules/; done; fi
	@rm -rf $(TESTDATA_STD)
	@mkdir -p $(TESTDATA_STD)
	@cp ../modules/std/*.pr $(TESTDATA_STD)/
	@(cd .. && find modules/ catalog.toml -type f | sort | xargs sha256sum) > $(RESOURCES)/.sources.sha256

musl-crt:
	@mkdir -p $(MUSL_CRT_DST)
	@cp $(MUSL_CRT_SRC)/crt1.o $(MUSL_CRT_DST)/
	@cp $(MUSL_CRT_SRC)/crti.o $(MUSL_CRT_DST)/
	@cp $(MUSL_CRT_SRC)/crtn.o $(MUSL_CRT_DST)/
	@cp $(MUSL_CRT_SRC)/libc.a $(MUSL_CRT_DST)/

ifeq ($(UNAME),Linux)
build: generate resources musl-crt
else
build: generate resources
endif
	@mkdir -p $(dir $(BINARY))
	go build -o $(BINARY) ./cmd/promise
	@shasum -a 256 $(BINARY) | cut -d' ' -f1 > $(dir $(BINARY)).promise.hash

ifeq ($(UNAME),Linux)
test: generate resources musl-crt
else
test: generate resources
endif
	go test ./...

clean:
	rm -f $(BINARY) promise
	rm -rf $(PARSER_PKG)/*.go $(PARSER_PKG)/*.interp $(PARSER_PKG)/*.tokens
	rm -rf $(RESOURCES)
	rm -rf $(TESTDATA_STD)

wasm-alloc:
	clang --target=wasm32-unknown-wasi -O2 -nostdlib -c \
		cmd/promise/crt/wasm32/wasm_alloc.c -o cmd/promise/crt/wasm32/wasm_alloc.o

llvm-bundle:
	@LLVM_VER=$$(for v in $$(seq 25 -1 22); do \
		if command -v opt-$$v >/dev/null 2>&1; then echo $$v; break; fi; done); \
	if [ -z "$$LLVM_VER" ]; then echo "ERROR: no LLVM 22+ found"; exit 1; fi; \
	LLVM_DIR=/usr/lib/llvm-$$LLVM_VER; \
	LLVM_LIB=$$(readlink -f $$LLVM_DIR/lib/libLLVM.so); \
	echo "Bundling LLVM $$LLVM_VER tools ($$LLVM_DIR)..."; \
	mkdir -p $(LLVM_DST); \
	gzip -9 -c $$LLVM_DIR/bin/opt > $(LLVM_DST)/opt.gz; \
	gzip -9 -c $$LLVM_DIR/bin/llc > $(LLVM_DST)/llc.gz; \
	gzip -9 -c $$LLVM_DIR/bin/lld > $(LLVM_DST)/lld.gz; \
	gzip -9 -c $$LLVM_LIB > $(LLVM_DST)/libLLVM.so.gz; \
	echo "  opt.gz: $$(du -h $(LLVM_DST)/opt.gz | cut -f1)"; \
	echo "  llc.gz: $$(du -h $(LLVM_DST)/llc.gz | cut -f1)"; \
	echo "  lld.gz: $$(du -h $(LLVM_DST)/lld.gz | cut -f1)"; \
	echo "  libLLVM.so.gz: $$(du -h $(LLVM_DST)/libLLVM.so.gz | cut -f1)"

llvm-bundle-darwin:
	@BREW_LLVM=""; \
	for v in $$(seq 25 -1 22); do \
		for prefix in /opt/homebrew/opt/llvm@$$v /usr/local/opt/llvm@$$v; do \
			if [ -x "$$prefix/bin/opt" ]; then BREW_LLVM="$$prefix"; break 2; fi; \
		done; \
	done; \
	if [ -z "$$BREW_LLVM" ]; then \
		for prefix in /opt/homebrew/opt/llvm /usr/local/opt/llvm; do \
			if [ -x "$$prefix/bin/opt" ]; then BREW_LLVM="$$prefix"; break; fi; \
		done; \
	fi; \
	if [ -z "$$BREW_LLVM" ]; then echo "ERROR: no Homebrew LLVM 22+ found (brew install llvm)"; exit 1; fi; \
	VER=$$($$BREW_LLVM/bin/opt --version 2>&1 | grep -oE 'LLVM version [0-9]+' | grep -oE '[0-9]+'); \
	if [ -z "$$VER" ] || [ "$$VER" -lt 22 ]; then echo "ERROR: LLVM $$VER < 22 at $$BREW_LLVM"; exit 1; fi; \
	BREW_LLD_DIR=""; \
	if [ -x "$$BREW_LLVM/bin/lld" ]; then BREW_LLD_DIR="$$BREW_LLVM"; \
	else \
		for prefix in /opt/homebrew/opt/lld /usr/local/opt/lld; do \
			if [ -x "$$prefix/bin/lld" ]; then BREW_LLD_DIR="$$prefix"; break; fi; \
		done; \
	fi; \
	if [ -z "$$BREW_LLD_DIR" ]; then echo "ERROR: lld not found (brew install lld)"; exit 1; fi; \
	echo "Bundling LLVM $$VER tools from $$BREW_LLVM + lld from $$BREW_LLD_DIR (darwin-$(GOARCH))..."; \
	mkdir -p $(LLVM_DST_DARWIN); \
	gzip -9 -c $$BREW_LLVM/bin/opt > $(LLVM_DST_DARWIN)/opt.gz; \
	gzip -9 -c $$BREW_LLVM/bin/llc > $(LLVM_DST_DARWIN)/llc.gz; \
	gzip -9 -c $$BREW_LLD_DIR/bin/lld > $(LLVM_DST_DARWIN)/lld.gz; \
	gzip -9 -c $$BREW_LLVM/lib/libLLVM.dylib > $(LLVM_DST_DARWIN)/libLLVM.dylib.gz; \
	for f in $$BREW_LLD_DIR/lib/liblld*.dylib; do \
		name=$$(basename $$f); \
		gzip -9 -c $$f > $(LLVM_DST_DARWIN)/$$name.gz; \
		echo "  $$name.gz: $$(du -h $(LLVM_DST_DARWIN)/$$name.gz | cut -f1)"; \
	done; \
	for dep in $$(otool -L $$BREW_LLVM/lib/libLLVM.dylib 2>/dev/null | awk '{print $$1}' | grep -E '^/opt/(homebrew|local)/' | grep -v libLLVM); do \
		name=$$(basename $$dep); \
		gzip -9 -c $$dep > $(LLVM_DST_DARWIN)/$$name.gz; \
		echo "  $$name.gz: $$(du -h $(LLVM_DST_DARWIN)/$$name.gz | cut -f1)"; \
	done; \
	echo "  opt.gz: $$(du -h $(LLVM_DST_DARWIN)/opt.gz | cut -f1)"; \
	echo "  llc.gz: $$(du -h $(LLVM_DST_DARWIN)/llc.gz | cut -f1)"; \
	echo "  lld.gz: $$(du -h $(LLVM_DST_DARWIN)/lld.gz | cut -f1)"; \
	echo "  libLLVM.dylib.gz: $$(du -h $(LLVM_DST_DARWIN)/libLLVM.dylib.gz | cut -f1)"

ifeq ($(UNAME),Linux)
release: generate resources musl-crt llvm-bundle
	@mkdir -p $(dir $(BINARY))
	go build -tags embed_llvm -o $(BINARY) ./cmd/promise
	@shasum -a 256 $(BINARY) | cut -d' ' -f1 > $(dir $(BINARY)).promise.hash
	@echo "Release binary: $$(du -h $(BINARY) | cut -f1) (self-contained)"
endif

ifeq ($(UNAME),Darwin)
release: generate resources llvm-bundle-darwin
	@mkdir -p $(dir $(BINARY))
	go build -tags embed_llvm -o $(BINARY) ./cmd/promise
	@shasum -a 256 $(BINARY) | cut -d' ' -f1 > $(dir $(BINARY)).promise.hash
	@echo "Release binary: $$(du -h $(BINARY) | cut -f1) (self-contained, requires macOS SDK)"
endif

install: build
	$(BINARY) install

fmt:
	go fmt ./...

ANTLR_VERSION := 4.13.1
ANTLR_JAR     := tools/antlr-$(ANTLR_VERSION)-complete.jar
ANTLR_URL     := https://www.antlr.org/download/antlr-$(ANTLR_VERSION)-complete.jar
GRAMMAR_DIR   := grammar
PARSER_PKG    := internal/parser
BINARY        := promise
RESOURCES     := cmd/promise/resources
UNAME         := $(shell uname -s)

# musl CRT paths (Linux only)
MUSL_CRT_SRC  := /usr/lib/x86_64-linux-musl
MUSL_CRT_DST  := $(RESOURCES)/crt/x86_64-linux-musl

.PHONY: all generate build test clean download-antlr fmt install resources musl-crt

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

resources:
	@rm -rf $(RESOURCES)/std
	@mkdir -p $(RESOURCES)/std
	@cp ../std/*.pr $(RESOURCES)/std/

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
	go build -o $(BINARY) ./cmd/promise

ifeq ($(UNAME),Linux)
test: generate resources musl-crt
else
test: generate resources
endif
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf $(PARSER_PKG)/*.go $(PARSER_PKG)/*.interp $(PARSER_PKG)/*.tokens
	rm -rf $(RESOURCES)

install: build
	./promise install

fmt:
	go fmt ./...

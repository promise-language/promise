ANTLR_VERSION := 4.13.1
ANTLR_JAR     := tools/antlr-$(ANTLR_VERSION)-complete.jar
ANTLR_URL     := https://www.antlr.org/download/antlr-$(ANTLR_VERSION)-complete.jar
GRAMMAR_DIR   := grammar
PARSER_PKG    := internal/parser
BINARY        := promise

.PHONY: all generate build test clean download-antlr fmt

all: generate build

download-antlr: $(ANTLR_JAR)

$(ANTLR_JAR):
	@mkdir -p tools
	curl -L -o $@ $(ANTLR_URL)

generate: $(ANTLR_JAR)
	@mkdir -p $(PARSER_PKG)
	cd $(GRAMMAR_DIR) && java -jar ../$(ANTLR_JAR) \
		-Dlanguage=Go \
		-package parser \
		-visitor \
		-o ../$(PARSER_PKG) \
		PromiseLexer.g4
	cd $(GRAMMAR_DIR) && java -jar ../$(ANTLR_JAR) \
		-Dlanguage=Go \
		-package parser \
		-visitor \
		-lib ../$(PARSER_PKG) \
		-o ../$(PARSER_PKG) \
		PromiseParser.g4

build: generate
	go build -o $(BINARY) ./cmd/promise

test: generate
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf $(PARSER_PKG)/*.go $(PARSER_PKG)/*.interp $(PARSER_PKG)/*.tokens

fmt:
	go fmt ./...

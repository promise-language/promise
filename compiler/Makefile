ANTLR_VERSION := 4.13.1
ANTLR_JAR     := tools/antlr-$(ANTLR_VERSION)-complete.jar
ANTLR_URL     := https://www.antlr.org/download/antlr-$(ANTLR_VERSION)-complete.jar
GRAMMAR_DIR   := grammar
PARSER_PKG    := internal/parser
BINARY        := promise
RESOURCES     := cmd/promise/resources

.PHONY: all generate build test clean download-antlr fmt install resources

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

resources:
	@mkdir -p $(RESOURCES)/std $(RESOURCES)/runtime
	@cp ../std/*.pr $(RESOURCES)/std/
	@cp ../runtime/*.c ../runtime/*.h $(RESOURCES)/runtime/

build: generate resources
	go build -o $(BINARY) ./cmd/promise

test: generate resources
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf $(PARSER_PKG)/*.go $(PARSER_PKG)/*.interp $(PARSER_PKG)/*.tokens
	rm -rf $(RESOURCES)

install: build
	./promise install

fmt:
	go fmt ./...

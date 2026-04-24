# Compiler Stages

Implementation stages for the Promise compiler pipeline.

| Stage | Package | Description | Status |
|-------|---------|-------------|--------|
| 1 | `compiler/internal/parser/` | ANTLR4 lexer/parser from grammar | Done |
| 2 | `compiler/internal/ast/` | AST builder: parse tree → typed AST nodes | Done |
| 3 | `compiler/internal/types/` | Type system: Named, Enum, Signature, Scope, Universe | Done |
| 4 | `compiler/internal/sema/` | Semantic analysis: type checking, name resolution | Planned |
| 5 | `compiler/internal/ownership/` | Borrow checker implementation | Planned |
| 6 | `compiler/internal/meta/` | Meta annotation processing | Planned |
| 7 | `compiler/internal/codegen/` | LLVM IR generation | Planned |
| 8 | `compiler/internal/module/` | Module resolution, dependency graph | Planned |
| 9 | `cmd/promise/` | CLI entry point (build, run, test, fmt, etc.) | Planned |
| 10 | `pkg/` | Package manager: fetch, resolve, lock | Planned |

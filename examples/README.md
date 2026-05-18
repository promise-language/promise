# Promise Examples

Learn Promise by running examples — from hello world to concurrent pipelines.

Every example is a runnable program **and** a verified test. Run any example to see its output, or run the full suite to verify everything works with the current compiler.

## Running Examples

```bash
# Run a single example as a program
bin/promise run examples/01_basics/hello.pr

# Test a single example (verify output matches expected)
bin/promise test examples/01_basics/hello.pr

# Test all examples
bin/promise test examples/...
```

## Learning Path

| Section | Topic | What you'll learn |
|---------|-------|-------------------|
| [01_basics/](01_basics/) | Fundamentals | Variables, functions, control flow, strings |
| [02_types/](02_types/) | Type system | Structs, enums, inheritance, generics, value types |
| [03_error_handling/](03_error_handling/) | Error model | Failable functions, custom errors, optionals |
| [04_ownership/](04_ownership/) | Ownership | Move/borrow semantics, destructors |
| [05_collections/](05_collections/) | Data structures | Vectors, maps, iterators, sorting |
| [06_functions_advanced/](06_functions_advanced/) | Advanced functions | Lambdas, higher-order functions, generators |
| [07_concurrency/](07_concurrency/) | Concurrency | Goroutines, channels, select, tasks, pipelines |
| [08_modules/](08_modules/) | Modules | File I/O, JSON, OS interaction |
| [09_patterns/](09_patterns/) | Design patterns | Builder, state machine, command dispatch |
| [11_wasm/](11_wasm/) | WebAssembly | Cross-target compilation, raw `wasm_import`, WIT bindings via `wasi_preview_2` |

WebAssembly examples are exercised on their target during `bin/verify --wasm` (wasm32-wasi via wasmtime) and `bin/verify --wasm-web` (wasm32-web via Node.js).

## Further Reading

- [Language Design](../docs/language-design.md) — full language specification
- [Standard Library](../docs/standard-library.md) — module inventory and API reference

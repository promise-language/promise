# mcp — Model Context Protocol

**Status: proposed.** Tracker task: T0451. Blocked on T0450 (modules/ai), T0446
(schema), T0447 (http).

`modules/mcp` provides MCP server and client support for Promise programs.
Server creation is nearly zero-boilerplate: tool schemas are derived from
function signatures via `schema.for_func[F]()`.

## Quick start

```promise
use mcp;
use io;

read_file!(string path `doc("Path to read.")) string
    `tool `doc("Read a UTF-8 file.") {
    return io.File.read!(path);
}

main!() {
    server := mcp.Server(name: "file-server", version: "1.0.0");
    // `tool-annotated functions auto-register via the per-module manifest.
    server.serve_stdio();
}
```

## API surface (summary)

- `McpError is error` with optional `int? code` (JSON-RPC) and `string? data`.
- `Transport` structural interface — `StdioTransport`, `SseTransport`,
  `HttpTransport` (stdio first per `docs/ai-platform.md` §12 Q3).
- `Server` — tool / resource / prompt registration; `serve_stdio!`, `serve_sse!`,
  `serve_http!`.
- `Resource`, `ResourceRequest`, `ResourceResponse`.
- `Prompt`, `PromptArgument`.
- `Client` — `connect_stdio!` / `connect_sse!` / `connect_http!` factories;
  `list_tools!`, `list_resources!`, `list_prompts!`, `call_tool!`, etc.

Discovered MCP tools are directly assignable into `ai.Agent` (zero glue).

## Compiler dependency

Auto-registration relies on the `` `tool `` meta annotation and the per-module
`_tool_manifest()` getter — both implemented under T0448.

## Full design

See [`docs/ai-platform.md`](../../docs/ai-platform.md) §6 for the full module
specification, transport details, resource template syntax, and end-to-end
examples.

# AI Platform Support — Design Proposal

Promise is designed for AI agents. This document defines the modules, types, and conventions
that make Promise a complete AI-centric platform — not just a language that AI can generate,
but a language that AI can *run in*, *build with*, and *be orchestrated from*.

The scope covers: MCP server creation, AI agent orchestration, sandboxed execution,
session management, authentication, structured I/O for LLMs, and the runtime primitives
that tie it all together.

---

## 1. Design Principles

### 1.1 AI as First-Class Runtime Target

Most languages treat AI integration as a library concern — bolted on via HTTP clients and
JSON serialization. Promise treats AI as a **runtime target**: the language, type system,
and module catalog are designed so that an AI agent can generate, execute, inspect, and
orchestrate Promise programs with minimal friction.

### 1.2 Agent-Agnostic by Default

The AI modules do not couple to any specific provider (OpenAI, Anthropic, Google, etc.).
Instead, they define **provider-neutral interfaces** that concrete providers implement.
A Promise program that uses `ai.Agent` works with any backend — the provider is a
configuration choice, not a code change.

### 1.3 Sandbox-First Execution

AI-generated code runs in a sandbox by default. The sandbox is not an afterthought bolted
onto an unrestricted runtime — it is the *default execution mode* for AI-invoked code.
Escaping the sandbox requires explicit capability grants, visible at the call site.

### 1.4 Structured Over Unstructured

LLMs work best with structured input and output. Promise's type system (algebraic types,
`doc()` annotations, structural interfaces) provides machine-readable schemas that
eliminate prompt-engineering guesswork. Tool definitions are derived from type signatures,
not hand-written JSON schemas.

---

## 2. Module Layout

The AI platform is split between **catalog modules** (shipped with the compiler, listed
in `catalog.toml`) and **community modules** (external git repos, pinned per project):

**Catalog modules** (provider-neutral, agent-agnostic):

```
modules/
  schema/           — Type-to-schema derivation (JSON Schema, tool/function descriptors)
  auth/             — Authentication primitives (API keys, env tokens, credential store)
  ai/               — Agent orchestration, provider interface, Tool, Agent, Session
                       (NO concrete LLM provider implementations)
  mcp/              — MCP server and client framework
  sandbox/          — Sandboxed code execution with capability control
```

**Community modules** (one per provider, pinned via `catalog.toml`):

```
ai_anthropic/       — Concrete Anthropic provider (claude-* models)
ai_openai/          — Concrete OpenAI provider + OpenAI-compatible endpoints
ai_router/          — Multi-provider routing
ai_google/          — Google Gemini provider
... (one per vendor)
```

Concrete providers do not belong in the standard set: they evolve with vendor APIs,
require per-vendor wire-format work, and accumulate dependencies that the rest of the
platform should not pay for. The `ai` catalog module defines only the **`Provider`
interface** plus a built-in **`MockProvider`** for testing. Real providers are
community modules that implement `Provider` and call the appropriate HTTP API.

All AI modules are explicit imports (`use ai;`, `use mcp;`). None belong in `std/` —
AI orchestration is not universal to every Promise program.

### Prerequisites

The AI platform depends on two catalog modules that must be implemented first:

- **`modules/json`** — already implemented. Used by tool argument serialization
  (`json.decode_string[T]`, `json.encode_string`), session persistence, and MCP
  protocol messages.
- **`modules/http`** — currently a placeholder in `catalog.toml`. Used by community
  provider modules and MCP SSE/HTTP transports. Must be completed before any concrete
  provider can be implemented.

The standard library already provides what the platform needs at the language level:
the `Encoder`/`Decoder` interfaces (`modules/std/encode.pr`), the `` `serializable ``
auto-derive mechanism, structural interfaces (`Iterator[T]`, `Generator[T]`), the
`task[T]`/`channel[T]` concurrency primitives, and the `error` base type.

### Dependencies between modules

```
json/ ──────────▶  std (auto)                — JSON encoder/decoder for Encoder/Decoder
http/ ──────────▶  std (auto), net           — HTTP client (prerequisite)
schema/ ────────▶  std (auto), json          — Type descriptors + JSON Schema output
auth/  ─────────▶  std (auto)                — Credentials and tokens
ai/  ───────────▶  schema, auth, json, http  — Agent loop, Provider interface, MockProvider
mcp/ ───────────▶  schema, ai, json, http    — Tools/resources/prompts over JSON-RPC
sandbox/ ───────▶  std (auto)                — Capability-controlled subprocess execution

ai_anthropic/ ─▶  ai, auth, http, json   (community)
ai_openai/ ────▶  ai, auth, http, json   (community)
```

Catalog modules can depend only on `std` and on other catalog modules listed in
`catalog.toml` (per `docs/creating-modules.md` §6.6). Community modules can depend on
any catalog module plus other community modules they explicitly pin.

---

## 3. `modules/schema` — Shared with Cloud Persistence

`modules/schema` is described in full in **`docs/schema.md`** because it is the
shared foundation for both the AI platform and cloud persistence
(`docs/cloud-persistence.md`). This section summarizes only what AI tooling
specifically consumes and refers the reader to the schema doc for everything else.

**What `schema` provides** (see `docs/schema.md` for full detail):

- A tagged enum `schema.Type` with variants `Object` / `Array` / `Map` / `Scalar` /
  `Enum` / `Function` / `Optional` / `Reference`. One descriptor for types,
  enums, free functions, and methods.
- Free-function constructors `schema.of[T]()` (compile-time derivation from a type)
  and `schema.for_func[F]()` (compile-time derivation from a function declaration).
- Renderers `to_json_schema()`, `to_openapi()`, `to_tool_input_schema()` defined as
  methods on `Type`.
- A 128-bit content-addressed identity `Hash128` on every type / field / function /
  variant / parameter, and a `` `id("...") `` meta annotation that pins the identity
  across renames.
- A three-state field model (required / optional / has_default) honored by both
  schema rendering and `` `serializable `` encode/decode.
- Compile-time-only generation. No runtime reflection.

**What AI tooling adds on top:**

- `Tool.create[T, R]` (§5.3) calls `schema.of[T]()` for the input shape and
  `to_tool_input_schema()` for the on-the-wire descriptor LLMs consume.
- `Agent.run_typed[T]` (§8) calls `schema.of[T]()` to produce the JSON Schema it
  injects into the system prompt.
- The `` `tool `` annotation (§9.1) drives `schema.for_func[F]()` and registers each
  annotated function in a per-module manifest.

For the schema's design constraints, identity composition, project identity in
`promise.toml`, and the compiler extension contract, see `docs/schema.md`.

---

## 4. `modules/auth` — Authentication Primitives

Handles API key management, token refresh, and credential storage for AI provider
connections and MCP transport authentication.

```promise
use os;

type AuthError is error `public `doc("Raised when credentials cannot be loaded or refreshed.") {
    int code `doc("0 = missing source, 1 = invalid format, 2 = refresh failed.");
}

type Credential `public `doc("A named secret value loaded from environment or credential store.") {
    string name `doc("Logical name: \"openai\", \"anthropic\", \"github\".");
    string _value;

    // Load from an environment variable. Raises AuthError if unset.
    from_env!(string name, string var_name) Credential `factory `public;

    // Load from the credential store at ~/.promise/credentials.toml.
    from_store!(string name) Credential `factory `public;

    // Read the secret. The accessor is a method (not a getter) to keep the
    // call site loud — secrets shouldn't read like ordinary properties.
    value(this) string `public `instance => this._value;
}

type TokenProvider `structural `public `doc("Anything that can mint a valid bearer token on demand.") {
    token!(~this) string `abstract `instance `doc("Returns a valid token, refreshing if needed.");
}

type StaticToken is TokenProvider `public `doc("A bearer token that never changes.") {
    string _value;
    new(~this, string value) `public { this._value = value; }
    token!(~this) string `public `instance => this._value;
}

type EnvToken is TokenProvider `public `doc("A bearer token loaded from an environment variable on each access.") {
    string _var_name;
    new(~this, string var_name) `public { this._var_name = var_name; }
    token!(~this) string `public `instance {
        string? v = os.env[this._var_name];
        if val := v {
            return val;
        }
        raise AuthError(message: "environment variable '{this._var_name}' not set", code: 0);
    }
}
```

### 4.1 Credential Store

`~/.promise/credentials.toml` — a simple, user-managed credential file:

```toml
[credentials]
openai = "sk-..."
anthropic = "sk-ant-..."
github = "ghp_..."
```

The `Credential.from_store("openai")` factory reads this file. Promise never sends
credentials to any service other than the one they're configured for — the runtime
enforces credential scoping.

### 4.2 OAuth Support (Future)

```promise
type OAuthProvider is TokenProvider `public {
    string client_id;
    string client_secret;
    string token_url;
    string? refresh_token;

    token!(~this) string `instance; // auto-refreshes expired tokens
}
```

---

## 5. `modules/ai` — Agent Orchestration

The core AI module. Provides a provider-neutral interface for LLM interaction, tool use,
multi-turn conversations, agent loops, session management, and multi-agent orchestration.

### 5.1 Error Types

All AI-specific errors inherit from a common base. This enables typed error handling
(`? e is RateLimitError { ... }`) at every level of the stack.

```promise
type AiError is error `public {
    int? status_code;       // HTTP status if applicable
}

type RateLimitError is AiError `public {
    Duration? retry_after;  // how long to wait before retrying
}

type ContextLengthError is AiError `public {
    int max_tokens;         // model's context window limit
    int requested_tokens;   // estimated tokens in the request
}

type ApiError is AiError `public {
    string response_body;   // raw API response for debugging
}

type ProviderError is AiError `public {
    string provider_name;   // "anthropic", "openai", etc.
}
```

### 5.2 Core Types

```promise
// ── Provider interface ─────────────────────────────────────────────────

// Provider is a nominal interface — types must declare `is Provider` explicitly.
// This prevents accidental satisfaction: having a `complete()` method should not
// silently make a type usable as an LLM provider.
type Provider `public {
    // Send a completion request and get a response
    complete!(~this, Request &req) Response `abstract `instance;

    // Stream a completion request token-by-token.
    // The Generator yields StreamEvents. Errors are surfaced as StreamEvent.Error —
    // the call itself is failable only for transport-level failures (connection lost,
    // bad URL, etc.) raised before the first event.
    stream!(~this, Request &req) Generator[StreamEvent] `abstract `instance;

    // Generate embeddings for a batch of inputs
    embed!(~this, string[] &inputs, string model) f64[][] `abstract `instance;

    close!(~this) `instance;
}

// ── Messages ───────────────────────────────────────────────────────────

enum Role `public {
    System,
    User,
    Assistant,
    Tool,
}

type Message `public `clone {
    Role role;
    Content[] content;
    string? name;               // for tool results: the tool name
    string? tool_call_id;       // correlation ID for tool results
}

enum Content `public {
    Text(string text),
    Image(u8[] data, string media_type),
    ImageUrl(string url),
    ToolUse(string id, string name, string arguments_json),
    ToolResult(string tool_call_id, string content, bool? is_error),
}

// ── Request/Response ───────────────────────────────────────────────────

type Request `public `serializable {
    string model;
    Message[] messages;
    Tool[] tools;
    int? max_tokens;
    f64? temperature;
    string? system;

    new(string model) {
        this.model = model;
        this.messages = [];
        this.tools = [];
    }

    add_message(~this, Message msg) `public `instance {
        this.messages.push(msg);
    }

    add_tool(~this, Tool tool) `public `instance {
        this.tools.push(tool);
    }

    with_system(~this, string system) `public `instance {
        this.system = system;
    }

    with_max_tokens(~this, int max) `public `instance {
        this.max_tokens = max;
    }

    with_temperature(~this, f64 temp) `public `instance {
        this.temperature = temp;
    }
}

type Response `public {
    Message message;
    StopReason stop_reason;
    Usage usage;
}

enum StopReason `public {
    EndTurn,
    MaxTokens,
    ToolUse,
    StopSequence,
}

type Usage `public `clone {
    int input_tokens;
    int output_tokens;
    int? cache_read_tokens;
    int? cache_write_tokens;
}

// ── Streaming ──────────────────────────────────────────────────────────
// The provider's stream() returns a Generator[StreamEvent] (yield-based
// coroutine; std/iter.pr). Errors are surfaced as StreamEvent.Error rather
// than making each yielded element failable. This avoids a double-failable
// "Generator[StreamEvent!]!" shape and matches how streaming APIs actually
// work: the stream is opened (the open call is failable), then events flow
// until completion or error.

enum StreamEvent `public {
    Start(string id, string model),
    Delta(ContentDelta delta),
    ToolUseStart(string id, string name),
    ToolUseDelta(string partial_json),
    Stop(StopReason reason, Usage usage),
    Error(AiError err),
}

enum ContentDelta `public {
    TextDelta(string text),
    InputJsonDelta(string partial_json),
}
```

### 5.3 Tool System

Tools are Promise functions that AI models can call. The `Tool` type wraps a function
with its `Type` (a `Type.Object` variant describing the input), derived from the type via
`schema.of[T]()` (§3) — which in turn requires `T` to be `` `serializable ``.

```promise
use schema;
use json;

type ToolCallEvent `public `doc("Information about a pending tool invocation, surfaced to filters and hooks.") {
    string id;
    string name;
    string arguments_json;
}

type Tool `public `doc("A function exposed to the model, with a schema describing its input.") {
    string name;
    string? description;
    schema.Type input_schema;
    (string) -> string! _handler;   // takes JSON args, returns JSON result

    // Create a tool from a typed handler function.
    // T = input type (deserialized from JSON), R = output type (serialized to JSON).
    // T must be `serializable`; R must satisfy Encodable.
    create[T, R](
        string name,
        string description,
        (T) -> R! handler
    ) Tool `mono `factory `public {
        schema.Type input_schema = schema.of[T]();
        return Tool(
            name: name,
            description: description,
            input_schema: input_schema,
            _handler: |string args_json| -> string! {
                T input = json.decode_string[T](args_json)!;
                R result = handler(input)!;
                return json.encode_string(result)!;
            },
        );
    }

    // Wrap an agent as a tool. The tool input is a string prompt;
    // the output is the agent's response.
    from_agent(string name, string description, Agent ~agent) Tool `factory `public;
}
```

Usage:

```promise
use ai;
use schema;

type WeatherRequest `serializable
    `doc("Get current weather for a location.") {
    string location `doc("City name or coordinates.");
    string? units `doc("Temperature units: celsius or fahrenheit.");
}

type WeatherResponse `serializable {
    f64 temperature;
    string condition;
    int humidity;
}

get_weather!(WeatherRequest req) WeatherResponse {
    // ... actual weather API call
}

main!() {
    ai.Tool weather_tool = ai.Tool.create[WeatherRequest, WeatherResponse](
        name: "get_weather",
        description: "Get current weather for a location.",
        handler: get_weather,
    );

    // Tool schema is automatically derived from WeatherRequest's fields + `doc.
    // The optionality of `units` is honored — it appears in `properties` but not in `required`.
}
```

### 5.4 Agent Loop

The `Agent` type implements the standard agent loop: send messages, receive tool calls,
execute tools, feed results back, repeat until the model stops.

```promise
type Agent `public {
    Provider provider;
    string model;
    string? system;
    Tool[] tools;
    Message[] history;
    AgentConfig config;
    AgentHooks? hooks;

    new(~this, Provider ~provider, string model) {
        this.provider = provider;
        this.model = model;
        this.tools = [];
        this.history = [];
        this.config = AgentConfig();
    }

    // ── Configuration ──────────────────────────────────────────────────

    add_tool(~this, Tool tool) `public `instance {
        this.tools.push(tool);
    }

    set_system(~this, string system) `public `instance {
        this.system = system;
    }

    set_hooks(~this, AgentHooks hooks) `public `instance {
        this.hooks = hooks;
    }

    // ── One-shot mode ──────────────────────────────────────────────────

    // Send a message and run the agent loop until completion.
    // Returns the final assistant response text.
    run!(~this, string user_message) string `public `instance;

    // Send a message and run with streaming output.
    // Yields text deltas as they arrive, executes tool calls automatically.
    run_stream!(~this, string user_message) Generator[string] `public `instance;

    // ── Interactive (multi-turn) mode ──────────────────────────────────

    // Send a user message and get the next agent turn.
    // Returns the full Turn including tool calls and results.
    turn!(~this, string user_message) Turn `public `instance;

    // Send a user message with streaming.
    // Yields TurnEvents as they happen.
    turn_stream!(~this, string user_message) Generator[TurnEvent] `public `instance;

    // ── Structured output ──────────────────────────────────────────────

    // Run the agent and parse the response into a typed value.
    // The model is instructed to respond with JSON matching T's schema.
    // Retries once on parse failure with the error message (self-correction).
    run_typed![T](~this, string user_message) T `public `instance;

    // ── History management ─────────────────────────────────────────────

    // Clear conversation history. Keeps system prompt and tools.
    clear_history(~this) `public `instance {
        this.history = [];
    }

    // Get the full conversation history (shared borrow).
    get_history(this) Message[]& `public `instance {
        return this.history;
    }

    // Replace history (e.g. when restoring from a session).
    set_history(~this, Message[] history) `public `instance {
        this.history = history;
    }

    // Fork the agent — creates a copy with cloned history but shared tools/config.
    fork(this) Agent `public `instance;

    // ── Lifecycle ──────────────────────────────────────────────────────

    drop(~this) `instance {
        this.provider.close();
    }
}

type AgentConfig `public {
    int max_turns = 50;             // max tool-use iterations before stopping
    int? max_tokens;
    f64? temperature;
    bool parallel_tool_calls = true; // execute independent tool calls concurrently
    ((ToolCallEvent) -> bool)? tool_filter;  // optional: approve/deny tool calls
    RetryConfig retry;
}

type RetryConfig `public {
    int max_retries = 3;
    Duration initial_delay = Duration.from_millis(500);
    f64 backoff_multiplier = 2.0;
    Duration max_delay = Duration.from_secs(30);
    bool retry_on_rate_limit = true;
    bool retry_on_server_error = true;
}

type Turn `public {
    Message[] messages;         // all messages in this turn (assistant + tool results)
    string? text;               // final assistant text (if any)
    StopReason stop_reason;
    Usage usage;                // aggregate usage for the turn
}

enum TurnEvent `public {
    TextDelta(string text),
    ToolCallStart(string id, string name),
    ToolCallComplete(string id, string name, string result),
    ToolCallError(string id, string name, error err),
    TurnComplete(Turn turn),
}
```

### 5.5 One-Shot vs Interactive Mode

**One-shot** (`agent.run()`): Send a single prompt, get a complete result. The agent loop
runs internally — tool calls are executed, results fed back, until the model produces a
final response. Best for autonomous tasks.

```promise
use ai;
use ai_anthropic;

main!() {
    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a helpful assistant.");
    agent.add_tool(weather_tool);

    // One-shot: returns final text after all tool use is resolved
    string result = agent.run("What's the weather in Paris?");
    print_line(result);
}
```

**Interactive** (`agent.turn()`): Send messages one at a time, inspect each turn,
and decide whether to continue. Best for conversational UIs and human-in-the-loop flows.

```promise
use ai;
use ai_anthropic;
use io;

main!() {
    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a coding assistant.");

    while line := io.read_line!() {
        Turn turn = agent.turn(line);

        if text := turn.text {
            print_line(text);
        }
    }
}
```

### 5.6 Streaming

Both modes support streaming via `Generator[T]` (the coroutine generator from
`std/iter.pr`, which satisfies `Iterator[T]` and is therefore consumable with `for`):

```promise
// One-shot streaming — yields text as it arrives
for chunk in agent.run_stream("Explain quantum computing") {
    print(chunk);    // no newline — partial tokens
}
print_line("");

// Interactive streaming — yields structured events
for event in agent.turn_stream("Write a haiku") {
    match event {
        TurnEvent.TextDelta(text) => print(text),
        TurnEvent.ToolCallStart(id, name) => print_line("[calling {name}...]"),
        TurnEvent.ToolCallComplete(id, name, result) => print_line("[{name} done]"),
        TurnEvent.TurnComplete(turn) => {},
    }
}
```

### 5.7 Parallel Tool Execution

When the model returns multiple tool calls in a single response and
`config.parallel_tool_calls` is true, the agent executes them concurrently using `go`:

```promise
// Internal implementation sketch:
if config.parallel_tool_calls && tool_calls.len > 1 {
    Task[string!][] tasks = [];
    for call in tool_calls {
        tasks.push(go { execute_tool(call) });
    }
    string![] results = [];
    for t in tasks {
        results.push(t.await());
    }
}
```

This uses Promise's existing concurrency model (`std/task.pr`) — no new primitives needed.

### 5.8 Tool Call Filtering

The `tool_filter` callback in `AgentConfig` enables human-in-the-loop approval for
sensitive operations:

```promise
agent.config.tool_filter = |ToolCallEvent evt| -> bool {
    if evt.name == "delete_file" {
        print_line("Agent wants to delete: {evt.arguments_json}");
        print("Allow? (y/n): ");
        return io.read_line()!.trim() == "y";
    }
    return true;    // allow all other tools
};
```

### 5.9 Providers

The `ai` catalog module ships **only** the abstract `Provider` interface (§5.2) and a
built-in `MockProvider` for testing. Concrete vendor providers live in **community
modules** registered in `catalog.toml` and pinned per project. The split is deliberate:

- **Vendors evolve independently of the language.** Anthropic ships new models and
  endpoint changes on their own cadence; binding those revisions to the compiler
  release cycle would be the wrong dependency.
- **Vendor SDKs accumulate dependencies.** Token counting, retry semantics, SSE event
  formats, and tool-format quirks differ per vendor. Keeping that out of `ai/` keeps
  the orchestration layer small and reviewable.
- **Programs already use `Provider` polymorphically.** Code written against `Provider`
  works unchanged with any community implementation; swapping providers is a `use`
  statement and a constructor change.

#### `MockProvider` (built into `ai`)

```promise
type MockProvider is Provider `public `doc("Returns canned responses for tests. Never hits the network.") {
    Response[] _responses;
    int _call_count;

    new() {
        this._responses = [];
        this._call_count = 0;
    }

    add_response(~this, Response resp) `public `instance `doc("Queue a response. Responses are returned in order; panics if exhausted.");
    get call_count int `public `instance;

    complete!(~this, Request &req) Response `public `instance;
    stream!(~this, Request &req) Generator[StreamEvent] `public `instance;
    embed!(~this, string[] &inputs, string model) f64[][] `public `instance;
    close!(~this) `public `instance;
}
```

#### Community provider modules (illustrative — not part of the standard set)

| Module          | Source                          | Provides                                                |
|-----------------|---------------------------------|---------------------------------------------------------|
| `ai_anthropic`  | `community/promise_ai_anthropic`| `Anthropic is Provider` — claude-* models, MessagesAPI  |
| `ai_openai`     | `community/promise_ai_openai`   | `OpenAI is Provider`, `OpenAICompat is Provider`        |
| `ai_google`     | `community/promise_ai_google`   | `Google is Provider` — Gemini API                       |
| `ai_router`     | `community/promise_ai_router`   | `Router is Provider` — prefix-based fan-out             |

A community module's surface area is just the constructor and any vendor-specific
extras — the `Provider` interface itself is stable and lives in `ai`:

```promise
// In community module ai_anthropic:
use ai;
use auth;
use http;

type Anthropic is ai.Provider `public `doc("Anthropic Messages API provider.") {
    string _api_key;
    http.Client _client;

    new(~this, string api_key) `public;
    from_env!() Self `factory `public `doc("Reads the ANTHROPIC_API_KEY environment variable.");

    complete!(~this, ai.Request &req) ai.Response `public `instance;
    stream!(~this, ai.Request &req) Generator[ai.StreamEvent] `public `instance;
    embed!(~this, string[] &inputs, string model) f64[][] `public `instance;
    close!(~this) `public `instance;
}
```

Programs use it like this:

```promise
use ai;
use ai_anthropic;

main!() {
    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    print_line(agent.run!("Hello"));
}
```

`Provider` is a nominal interface (§5.2), so a community module declaring
`type Anthropic is ai.Provider` is a compile-time guarantee that the module
implements every abstract method.

### 5.10 Session Management

Sessions persist conversation state across multiple interactions, enabling long-running
agent workflows, resumable tasks, and multi-user applications.

```promise
type Session `public {
    string id;
    Message[] history;
    map[string, string] metadata;
    Instant created_at;
    Instant? last_active;

    // ── Construction ─────────────────────────────────────────────────

    // Create a new session with a generated ID.
    create() Session `factory `public;

    // ── Persistence ──────────────────────────────────────────────────

    // Save session to a file.
    save!(this, string path) `public `instance;

    // Load a session from a file.
    load!(string path) Session `factory `public;

    // ── History management ───────────────────────────────────────────

    // Add a message to the session history.
    add_message(~this, Message msg) `public `instance {
        this.history.push(msg);
        this.last_active = Instant.now();
    }

    // Replace the full history (e.g. syncing from an agent).
    set_history(~this, Message[] history) `public `instance {
        this.history = history;
        this.last_active = Instant.now();
    }

    // Trim history to the last N messages, keeping the system prompt.
    trim(~this, int keep_last) `public `instance;

    // Summarize old messages into a single context message to manage token usage.
    // Takes a summarizer callback — typically a closure that calls an LLM.
    // This avoids coupling Session to Provider.
    compact(~this, (Message[]) -> Message! summarizer)! `public `instance;

    // Fork the session — creates a branch with cloned history up to this point.
    fork(this) Session `public `instance;

    // Get total token count estimate for the session.
    estimate_tokens(this) int `public `instance;
}
```

### 5.11 Session Store

For applications that manage multiple users or conversations:

```promise
type SessionStore `public `structural {
    get!(~this, string id) Session? `abstract `instance;
    save!(~this, Session &session) `abstract `instance;
    delete!(~this, string id) `abstract `instance;
    list!(~this) string[] `abstract `instance; // list session IDs
}

// File-based implementation (one JSON file per session)
type FileSessionStore is SessionStore `public {
    string _dir;
    new(~this, string dir) `public;

    get!(~this, string id) Session? `public `instance;
    save!(~this, Session &session) `public `instance;
    delete!(~this, string id) `public `instance;
    list!(~this) string[] `public `instance;
}

// In-memory implementation (for testing / ephemeral use)
type MemorySessionStore is SessionStore `public {
    new() `public;

    get!(~this, string id) Session? `public `instance;
    save!(~this, Session &session) `public `instance;
    delete!(~this, string id) `public `instance;
    list!(~this) string[] `public `instance;
}
```

### 5.12 Agent with Session

```promise
use ai;
use ai_anthropic;
use io;

main!() {
    // Resume or create session
    session := ai.Session.load("session.json") ? { ai.Session.create() };

    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a project manager assistant.");

    // Restore agent history from session (clone — both agent and session keep a copy)
    agent.set_history(session.history.clone());

    // Interactive loop
    while line := io.read_line!() {
        if line == "/quit" { break; }
        if line == "/clear" {
            agent.clear_history();
            session.set_history([]);
            continue;
        }

        Turn turn = agent.turn(line);
        if text := turn.text { print_line(text); }

        // Sync session from agent (clone history)
        session.set_history(agent.get_history().clone());
        session.save("session.json");
    }
}
```

### 5.13 Observability

#### Event Hooks

```promise
type AgentHooks `public {
    (Request &)? on_request;                // before each LLM call
    (Response &)? on_response;              // after each LLM call
    (ToolCallEvent &, string &)? on_tool_call; // (event, result_json)
    (error &)? on_error;                    // on any error
    (Usage &)? on_usage;                    // after each completion (token tracking)
}
```

#### Usage Tracking

```promise
type UsageTracker `public {
    int total_input_tokens;
    int total_output_tokens;
    int total_requests;
    f64 estimated_cost_usd;

    new() {
        this.total_input_tokens = 0;
        this.total_output_tokens = 0;
        this.total_requests = 0;
        this.estimated_cost_usd = 0.0;
    }

    // Create hooks that automatically track usage.
    as_hooks(this) AgentHooks `public `instance;

    // Get a summary string.
    summary(this) string `public `instance;
}
```

### 5.14 Multi-Agent Orchestration

#### Agent Pipelines

```promise
type PipelineStep `public {
    string name;
    Agent agent;
    string? system;             // override system prompt for this step
}

type Pipeline `public {
    PipelineStep[] steps;

    new() {
        this.steps = [];
    }

    // Add a step that transforms the input string.
    add_step(~this, string name, Agent ~agent, string? system) `public `instance {
        this.steps.push(PipelineStep(name: name, agent: agent, system: system));
    }

    // Run the pipeline: each step's output becomes the next step's input.
    run!(~this, string input) string `public `instance;

    // Run with streaming — yields events from all steps.
    run_stream!(~this, string input) Generator[PipelineEvent] `public `instance;
}

enum PipelineEvent `public {
    StepStart(string name),
    StepDelta(string name, string text),
    StepComplete(string name, string result),
    PipelineComplete(string result),
}
```

#### Agent Delegation

An agent can delegate subtasks to other agents, each with their own tools and system
prompts:

```promise
use ai;
use ai_anthropic;

main!() {
    use provider := ai_anthropic.Anthropic.from_env!();

    // Specialist agents
    researcher := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    researcher.set_system("You are a research assistant. Find facts and cite sources.");
    researcher.add_tool(web_search_tool);

    writer := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    writer.set_system("You are a technical writer. Write clear, concise prose.");

    reviewer := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    reviewer.set_system("You are a code reviewer. Find bugs and suggest improvements.");

    // Orchestrator delegates to specialists via agent-as-tool
    orchestrator := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    orchestrator.set_system("You coordinate a team of specialists.");

    orchestrator.add_tool(ai.Tool.from_agent(
        name: "research",
        description: "Delegate a research question to the research specialist.",
        agent: researcher,
    ));
    orchestrator.add_tool(ai.Tool.from_agent(
        name: "write",
        description: "Delegate a writing task to the technical writer.",
        agent: writer,
    ));
    orchestrator.add_tool(ai.Tool.from_agent(
        name: "review",
        description: "Delegate a code review to the reviewer.",
        agent: reviewer,
    ));

    result := orchestrator.run(
        "Research Promise language design, write a blog post, and review it."
    );
    print_line(result);
}
```

---

## 6. `modules/mcp` — Model Context Protocol

MCP (Model Context Protocol) support for both server and client roles. Promise's type
system makes MCP server creation nearly zero-boilerplate — tool schemas are derived from
function signatures.

### 6.1 Error Types

```promise
type McpError is error `public {
    int? code;              // JSON-RPC error code
    string? data;           // optional error data
}
```

### 6.2 Transport

```promise
// Transport abstracts the communication channel for MCP protocol messages.
type Transport `public `structural {
    send!(~this, string message) `abstract `instance;
    receive!(~this) string `abstract `instance;
    close!(~this) `abstract `instance;
}

type StdioTransport is Transport `public {
    send!(~this, string message) `public `instance;
    receive!(~this) string `public `instance;
    close!(~this) `public `instance;
}

type SseTransport is Transport `public {
    string url;
    send!(~this, string message) `public `instance;
    receive!(~this) string `public `instance;
    close!(~this) `public `instance;
}

type HttpTransport is Transport `public {
    string url;
    send!(~this, string message) `public `instance;
    receive!(~this) string `public `instance;
    close!(~this) `public `instance;
}
```

### 6.3 MCP Server

```promise
type Server `public {
    string name;
    string version;
    ai.Tool[] _tools;
    Resource[] _resources;
    Prompt[] _prompts;
    ServerConfig config;

    new(~this, string name, string version) {
        this.name = name;
        this.version = version;
        this._tools = [];
        this._resources = [];
        this._prompts = [];
        this.config = ServerConfig();
    }

    // ── Tool registration ──────────────────────────────────────────────

    // Register a tool from a typed handler function.
    add_tool[T, R](~this, string name, string description, (T) -> R! handler) `public `instance {
        this._tools.push(ai.Tool.create[T, R](
            name: name,
            description: description,
            handler: handler,
        ));
    }

    // Register a pre-built Tool.
    add_raw_tool(~this, ai.Tool tool) `public `instance {
        this._tools.push(tool);
    }

    // ── Resource registration ──────────────────────────────────────────

    // Register a resource with a URI template.
    add_resource(~this, Resource resource) `public `instance {
        this._resources.push(resource);
    }

    // ── Prompt registration ────────────────────────────────────────────

    // Register a prompt template.
    add_prompt(~this, Prompt prompt) `public `instance {
        this._prompts.push(prompt);
    }

    // ── Transport ──────────────────────────────────────────────────────

    // Run the server on stdio transport (standard MCP transport).
    serve_stdio!(~this) `public `instance;

    // Run the server on SSE transport at the given address.
    serve_sse!(~this, string addr) `public `instance;

    // Run the server on streamable HTTP transport.
    serve_http!(~this, string addr) `public `instance;
}

type ServerConfig `public {
    bool logging = true;
    int? max_concurrent_tools;
    ((ai.ToolCallEvent) -> bool)? auth_filter;    // per-call authorization
}
```

### 6.4 MCP Resources

```promise
type Resource `public {
    string uri;                 // URI template: "file:///{path}", "db:///{table}"
    string name;
    string? description;
    string? mime_type;
    (ResourceRequest) -> ResourceResponse! handler;

    // Create from a typed handler
    create[T](
        string uri,
        string name,
        string description,
        (T) -> string! handler
    ) Resource `mono `public;
}

type ResourceRequest `public {
    string uri;
    map[string, string] params;     // template parameter values
}

type ResourceResponse `public {
    string content;
    string? mime_type;
}
```

### 6.5 MCP Prompts

```promise
type Prompt `public {
    string name;
    string? description;
    PromptArgument[] arguments;
    (map[string, string]) -> ai.Message[]! handler;
}

type PromptArgument `public {
    string name;
    string? description;
    bool required;
}
```

### 6.6 Complete MCP Server Example

```promise
use mcp;
use io;
use json;

type FileReadRequest `doc("Read a file from the filesystem.") {
    string path `doc("Absolute or relative file path.");
}

type FileWriteRequest `doc("Write content to a file.") {
    string path `doc("File path to write to.");
    string content `doc("Content to write.");
}

type FileInfo {
    string path;
    string content;
    int size;
}

read_file!(FileReadRequest req) FileInfo {
    string content = io.File.read(req.path);
    return FileInfo(path: req.path, content: content, size: content.len);
}

write_file!(FileWriteRequest req) string {
    io.File.write(req.path, req.content);
    return "Written {req.content.len} bytes to {req.path}";
}

main!() {
    server := mcp.Server(name: "file-server", version: "1.0.0");

    // Tools — schemas derived from FileReadRequest / FileWriteRequest types
    server.add_tool[FileReadRequest, FileInfo](
        name: "read_file",
        description: "Read a file from the filesystem.",
        handler: read_file,
    );

    server.add_tool[FileWriteRequest, string](
        name: "write_file",
        description: "Write content to a file.",
        handler: write_file,
    );

    // Resources — expose files as MCP resources
    server.add_resource(mcp.Resource(
        uri: "file:///{path}",
        name: "File",
        description: "Read a file by path.",
        handler: |mcp.ResourceRequest req| -> mcp.ResourceResponse! {
            string content = io.File.read(req.params["path"] ?: "");
            return mcp.ResourceResponse(content: content, mime_type: "text/plain");
        },
    ));

    // Run on stdio (standard MCP transport)
    server.serve_stdio();
}
```

### 6.7 MCP Client

```promise
type Client `public {
    string? _server_name;
    Transport _transport;

    // ── Connection ─────────────────────────────────────────────────────

    // Connect to an MCP server via stdio (spawn a subprocess).
    connect_stdio!(string command, string[] args) Client `factory `public;

    // Connect to an MCP server via SSE.
    connect_sse!(string url) Client `factory `public;

    // Connect to an MCP server via streamable HTTP.
    connect_http!(string url) Client `factory `public;

    // ── Discovery ──────────────────────────────────────────────────────

    // List available tools on the connected server.
    list_tools!(~this) ai.Tool[] `public `instance;

    // List available resources.
    list_resources!(~this) Resource[] `public `instance;

    // List available prompts.
    list_prompts!(~this) Prompt[] `public `instance;

    // ── Invocation ─────────────────────────────────────────────────────

    // Call a tool by name with JSON arguments.
    call_tool!(~this, string name, string arguments_json) string `public `instance;

    // Read a resource by URI.
    read_resource!(~this, string uri) ResourceResponse `public `instance;

    // Get a prompt by name with arguments.
    get_prompt!(~this, string name, map[string, string] args) ai.Message[] `public `instance;

    // ── Lifecycle ──────────────────────────────────────────────────────

    close!(~this) `public `instance;
}
```

### 6.8 Connecting MCP Tools to an Agent

MCP tools discovered from a server can be directly plugged into an `ai.Agent`:

```promise
use ai;
use ai_anthropic;
use mcp;

main!() {
    // Connect to MCP servers
    use file_server := mcp.Client.connect_stdio("file-server", []);
    use db_server := mcp.Client.connect_stdio("db-server", []);

    // Discover tools
    ai.Tool[] file_tools = file_server.list_tools();
    ai.Tool[] db_tools = db_server.list_tools();

    // Create agent with all discovered tools
    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    for tool in file_tools { agent.add_tool(tool); }
    for tool in db_tools { agent.add_tool(tool); }

    // Agent can now use tools from both MCP servers
    result := agent.run("Read the config file and update the database");
    print_line(result);
}
```

---

## 7. `modules/sandbox` — Sandboxed Execution

Provides capability-controlled execution of Promise code. AI-generated code runs in a
restricted environment where system access is explicitly granted, not implicitly available.

### 7.1 Error Types

```promise
type SandboxError is error `public {
    int? exit_code;
    bool timed_out;
    bool memory_exceeded;
}
```

### 7.2 Capability Model

```promise
enum Capability `public {
    // Filesystem
    FileRead(string[] paths),           // read specific paths/globs
    FileWrite(string[] paths),          // write specific paths/globs
    FileReadAll,                        // read any path
    FileWriteAll,                       // write any path

    // Network
    NetConnect(string[] hosts),         // connect to specific hosts
    NetListen(int[] ports),             // listen on specific ports
    NetAll,                             // unrestricted network

    // Process
    Exec(string[] programs),            // run specific programs
    ExecAll,                            // run any program

    // Environment
    EnvRead(string[] vars),             // read specific env vars
    EnvReadAll,                         // read any env var

    // System
    Stdin,                              // read from stdin
    Stdout,                             // write to stdout
    Stderr,                             // write to stderr

    // Time
    Clock,                              // access system clock
    Sleep,                              // sleep/delay
}
```

### 7.3 Sandbox Type

```promise
type Sandbox `public {
    Capability[] _capabilities;
    SandboxConfig config;

    new(~this) {
        this._capabilities = [];
        this.config = SandboxConfig();
    }

    // ── Capability grants ──────────────────────────────────────────────

    // Grant a capability to code running in this sandbox.
    allow(~this, Capability cap) `public `instance {
        this._capabilities.push(cap);
    }

    // Grant multiple capabilities.
    allow_all(~this, Capability[] caps) `public `instance {
        for cap in caps { this._capabilities.push(cap); }
    }

    // ── Preset configurations ──────────────────────────────────────────

    // Minimal sandbox: stdout only. No filesystem, network, or process access.
    minimal() Self `factory `public {
        sb := Self();
        sb.allow(Capability.Stdout);
        return sb;
    }

    // Standard sandbox: stdout, stderr, clock. Common for compute tasks.
    standard() Self `factory `public {
        sb := Self();
        sb.allow(Capability.Stdout);
        sb.allow(Capability.Stderr);
        sb.allow(Capability.Clock);
        return sb;
    }

    // Full access: all capabilities granted. Use only for trusted code.
    unrestricted() Self `factory `public;

    // ── Execution ──────────────────────────────────────────────────────

    // Execute a Promise source file in this sandbox.
    run_file!(~this, string path) ExecutionResult `public `instance;

    // Execute inline Promise source code in this sandbox.
    run_code!(~this, string code) ExecutionResult `public `instance;

    // Execute a compiled Promise binary in this sandbox.
    run_binary!(~this, string path, string[] args) ExecutionResult `public `instance;
}

type SandboxConfig `public {
    Duration timeout = Duration.from_secs(30);
    int max_memory_mb = 256;
    int max_output_bytes = 1_048_576;   // 1MB stdout/stderr cap
    string? working_dir;
}

type ExecutionResult `public {
    int exit_code;
    string stdout;
    string stderr;
    Duration elapsed;
    bool timed_out;
    bool memory_exceeded;
}
```

### 7.4 How the Sandbox Works

The sandbox compiles (if needed) and executes Promise code in a restricted subprocess.
**Capability enforcement is implemented at the PAL layer** — the same layer that
mediates filesystem, network, process, environment, and time access for ordinary
Promise programs (`compiler/internal/codegen/pal/`). The PAL inspects each
capability-gated call against the active `Capability[]` and refuses operations not in
the grant set:

- **Filesystem**: `pal_file_open`, `pal_file_create`, `pal_dir_open`, etc. check the
  requested path against `FileRead` / `FileWrite` glob lists.
- **Network**: `pal_tcp_connect`, `pal_tcp_listen`, `pal_udp_send` check the host/port
  against `NetConnect` / `NetListen` lists.
- **Process**: `pal_exec` checks the program path against `Exec` / `ExecAll`.
- **Environment**: `pal_get_env` filters by `EnvRead` allowlist.
- **Time / Memory**: `pal_sleep` and process resource limits are enforced via PAL
  shims plus `setrlimit` where the platform supports it.

**Why PAL-level instead of syscall-level?** There is no syscall-sandboxing primitive
that works consistently across the four platforms Promise targets (Linux, macOS,
Windows, WASM). seccomp and landlock are Linux-only; sandbox-exec is macOS-only;
Windows has Job Objects and AppContainer with a different model again; WASM has no
syscalls at all and relies entirely on host-imposed import restrictions. Putting the
authoritative check at the PAL — which already sits in front of every syscall the
runtime makes — gives one consistent semantics on every target. Specific PAL
implementations may *additionally* invoke the platform's syscall sandbox (seccomp on
Linux, sandbox-exec on macOS) as a defense-in-depth layer, but that is an
implementation detail; the `Capability` enum and the sandbox config are the portable
contract.

The Promise compiler can optionally compile in **sandbox mode** (`promise build --sandbox`),
which statically verifies that the code does not use any module whose capabilities exceed
the sandbox grants. This is a *compile-time check* — the program won't even build if it
imports `io` without `FileRead`/`FileWrite` capability.

### 7.5 Agent + Sandbox Integration

```promise
use ai;
use ai_anthropic;
use sandbox;

main!() {
    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You write Promise code. Return only code, no explanation.");

    // Create a sandbox for executing AI-generated code
    sb := sandbox.Sandbox.standard();
    sb.allow(sandbox.Capability.FileRead(["/data/*"]));
    sb.config.timeout = Duration.from_secs(10);

    // Generate code
    string code = agent.run("Write a program that counts lines in /data/input.txt");

    // Execute in sandbox
    sandbox.ExecutionResult result = sb.run_code(code);

    if result.exit_code == 0 {
        print_line("Output: {result.stdout}");
    } else {
        print_line("Error: {result.stderr}");
        // Feed error back to agent for self-correction
        string fixed = agent.run("The code failed with: {result.stderr}. Fix it.");
        result = sb.run_code(fixed);
    }
}
```

---

## 8. Structured Output

LLMs increasingly support structured output (JSON matching a schema). Promise's type
system makes this natural — request a typed response and get back a deserialized value.

```promise
use ai;
use ai_anthropic;

type MovieRecommendation `serializable {
    string title `doc("Movie title.");
    int year `doc("Release year.");
    string reason `doc("Why this movie was recommended.");
    f64 confidence `doc("Confidence score 0.0 to 1.0.");
}

main!() {
    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");

    MovieRecommendation rec = agent.run_typed[MovieRecommendation]!(
        "Recommend a sci-fi movie for someone who loved Arrival."
    );

    print_line("{rec.title} ({rec.year}): {rec.reason}");
}
```

The `run_typed[T]` method:
1. Generates a JSON Schema from `T` via `schema.of[T]()` (requires `T` to be `` `serializable ``).
2. Appends schema instructions to the system prompt.
3. Parses the model's JSON response into `T` via `json.decode_string[T]()`.
4. Retries once on parse failure with the error message (self-correction).

---

## 9. `promise ai` — CLI Integration

The `promise` binary includes AI-related subcommands for development workflows:

```
promise ai serve file.pr            # Run a .pr file as an MCP server (stdio)
promise ai serve --sse file.pr      # Run as MCP server with SSE transport
promise ai tools file.pr            # List tools defined in file (JSON output)
promise ai schema Type              # Print JSON Schema for a type
promise ai run file.pr              # Run an agent program
promise ai sandbox file.pr          # Run file.pr in minimal sandbox
```

### 9.1 The `` `tool `` Annotation — A Compiler Extension

`` `tool `` is the single compiler change required to make MCP server creation
zero-boilerplate. It is added to the built-in metas table (language-design §8.3,
implemented in `compiler/internal/sema/meta.go`) as a function-and-method annotation:

| Meta | Applies To | Description |
|------|------------|-------------|
| `` `tool `` | functions, methods | Mark as exposable as an MCP/agent tool. The compiler builds a `schema.Type` (kind `Function`) descriptor and registers it on the enclosing module so `promise ai serve` and runtime helpers can enumerate annotated tools. |

**What the compiler must do**

1. **Recognize `` `tool ``** as a valid annotation on `func` and method declarations,
   with optional named parameters (e.g., `` `tool(name: "wire_name") ``). Reject on
   types, fields, or anything else.
2. **Synthesize a `Type` descriptor** for each `` `tool ``-annotated declaration in
   the same pass that handles `` `serializable `` schemas (see `docs/schema.md` §7). The descriptor
   captures parameter names, parameter `` `doc `` annotations, parameter defaults,
   parameter optionality (`T?`), return type, and failability — the function-declaration
   information the compiler always has and never erases (see `docs/schema.md` §3).
3. **Validate parameter types** are either primitive, `` `serializable ``, or
   primitive-of-`` `serializable ``-containers (`T[]`, `map[string, T]`, `T?`). Emit a
   precise diagnostic when not — `mcp tools cannot accept non-serializable parameter
   'config' of type Config; mark Config as `` `serializable ``` is far better than a
   runtime crash inside a JSON decoder.
4. **Register each tool in a per-module manifest** that `promise ai serve` and the
   runtime can read at startup. The manifest entry is the same `Type` value used at
   runtime — no second representation. Concretely, for each module the compiler emits
   a hidden module-level getter `_tool_manifest() Tool[]` that constructs all the
   tool wrappers; `promise ai serve` calls this getter.

**Why this minimal extension is sufficient**

- All the type information needed already exists — `` `tool `` does not introduce a
  new reflection facility, only a new label on top of facilities the compiler has.
- The schema synthesis hook is the same one that powers `schema.of[T]()` (see `docs/schema.md` §7) and
  `` `serializable `` (`docs/serialization-plan.md`); it does not need to be invented.
- Free-function manifest enumeration is already a well-defined operation — every Go
  unit-test discovery, every embedded-resource registry uses the same shape.

**What `` `tool `` does NOT do**

- It does not make the function callable from JSON automatically — wrapping the
  function in a JSON-decoding/encoding shim is the job of `mcp.Server` or
  `ai.Tool.create[T, R]()`. The annotation only carries the schema, the name, and
  the description.
- It does not opt in to network exposure. A `` `tool ``-annotated function is just
  metadata; it is exposed only when a `mcp.Server` (or the `promise ai serve` driver)
  reads the manifest and registers it.

### 9.2 `promise ai serve`

Takes a `.pr` file that defines tools and runs it as an MCP server. The file can be a
full MCP server program (with `main()` and explicit `mcp.Server` setup), or a simpler
"tool file" where functions annotated with `` `tool `` are automatically registered:

```promise
// tools.pr — no main(), no mcp import needed
// promise ai serve tools.pr  auto-registers `tool-annotated functions
use io;

add(int a `doc("First number."), int b `doc("Second number.")) int
    `tool `doc("Add two numbers.") {
    return a + b;
}

read_file!(string path `doc("File path to read.")) string
    `tool `doc("Read the contents of a file.") {
    return io.File.read!(path);
}
```

`promise ai serve tools.pr` wraps these in an MCP server automatically — no
boilerplate. The tool schemas are derived from the function signatures and
`` `doc `` annotations as described in §9.1.

`` `tool `` is an explicit opt-in: only annotated functions are registered. This
follows Promise's "explicit over implicit" philosophy — a function must declare its
intent to be exposed as a tool. Functions without `` `tool `` are invisible to the
manifest even if their signature would otherwise be valid.

### 9.3 `promise ai schema`

Prints the JSON Schema for a type or function, useful for debugging tool definitions:

```
$ promise ai schema CreateUserRequest -f user.pr
{
  "type": "object",
  "description": "Request to create a new user.",
  ...
}

$ promise ai schema add -f tools.pr
{
  "type": "function",
  "name": "add",
  "description": "Add two numbers.",
  "parameters": {
    "type": "object",
    "properties": {
      "a": { "type": "integer", "description": "First number." },
      "b": { "type": "integer", "description": "Second number." }
    },
    "required": ["a", "b"]
  }
}
```

The CLI distinguishes types from functions automatically — types resolve via
`schema.of[T]()`, free functions and `` `tool ``-annotated declarations resolve via
`schema.for_func[F]()`.

---

## 10. End-to-End Examples

### 10.1 CLI Chatbot with Session Persistence

```promise
use ai;
use ai_anthropic;
use io;

main!() {
    // Load or create session
    session := ai.Session.load("chat.json") ? { ai.Session.create() };

    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a friendly assistant.");
    agent.set_history(session.history.clone());

    print_line("Chat (type /quit to exit, /clear to reset)");

    while line := io.read_line!() {
        match line.trim() {
            "/quit" => break,
            "/clear" => {
                agent.clear_history();
                session.set_history([]);
                print_line("History cleared.");
                continue;
            },
            "/tokens" => {
                print_line("Estimated tokens: {session.estimate_tokens()}");
                continue;
            },
            _ => {},
        }

        // Stream the response
        for event in agent.turn_stream(line) {
            match event {
                ai.TurnEvent.TextDelta(text) => print(text),
                ai.TurnEvent.TurnComplete(turn) => print_line(""),
                _ => {},
            }
        }

        session.set_history(agent.get_history().clone());
        session.save("chat.json");
    }
}
```

### 10.2 Code Generation Agent with Sandboxed Execution

```promise
use ai;
use ai_anthropic;
use sandbox;

main!() {
    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system(
        "You write Promise programs. Return ONLY the code, no markdown fences or explanation."
    );

    sb := sandbox.Sandbox.standard();
    sb.config.timeout = Duration.from_secs(10);

    prompt := "Write a program that prints the first 20 Fibonacci numbers.";

    for attempt in 0..3 {
        string code = agent.run(prompt);
        result := sb.run_code(code);

        if result.exit_code == 0 {
            print_line("Output:\n{result.stdout}");
            break;
        }

        // Self-correction: feed the error back
        prompt = "The code failed:\n{result.stderr}\nFix the code.";
        print_line("Attempt {attempt + 1} failed, retrying...");
    }
}
```

### 10.3 MCP Server with Database Tools

```promise
use mcp;
use io;

type QueryRequest `doc("Execute a SQL query.") {
    string sql `doc("The SQL query to execute.");
    string[] params `doc("Positional parameters for the query.");
}

type QueryResult {
    string[] columns;
    string[][] rows;
    int rows_affected;
}

type InsertRequest `doc("Insert a row into a table.") {
    string table `doc("Table name.");
    map[string, string] values `doc("Column-value pairs to insert.");
}

main!() {
    server := mcp.Server(name: "db-tools", version: "1.0.0");

    server.add_tool[QueryRequest, QueryResult](
        name: "query",
        description: "Execute a read-only SQL query.",
        handler: |QueryRequest req| -> QueryResult! {
            // ... execute query
        },
    );

    server.add_tool[InsertRequest, string](
        name: "insert",
        description: "Insert a row into a table.",
        handler: |InsertRequest req| -> string! {
            // ... insert row
            return "Inserted into {req.table}";
        },
    );

    server.serve_stdio();
}
```

### 10.4 Multi-MCP Agent with Tool Filtering

```promise
use ai;
use ai_anthropic;
use mcp;
use io;

main!() {
    // Connect to multiple MCP servers
    use files := mcp.Client.connect_stdio("file-server", []);
    use db := mcp.Client.connect_stdio("db-server", []);
    use web := mcp.Client.connect_stdio("web-search", []);

    use provider := ai_anthropic.Anthropic.from_env!();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");

    // Register all tools from all servers
    for tool in files.list_tools() { agent.add_tool(tool); }
    for tool in db.list_tools() { agent.add_tool(tool); }
    for tool in web.list_tools() { agent.add_tool(tool); }

    // Require approval for write operations
    agent.config.tool_filter = |ai.ToolCallEvent evt| -> bool {
        if evt.name == "write_file" || evt.name == "insert" || evt.name == "delete" {
            print_line("[{evt.name}] {evt.arguments_json}");
            print("Approve? (y/n): ");
            return io.read_line()!.trim() == "y";
        }
        return true;
    };

    // Usage tracking
    tracker := ai.UsageTracker();
    agent.set_hooks(tracker.as_hooks());

    // Interactive session
    while line := io.read_line!() {
        for event in agent.turn_stream(line) {
            match event {
                ai.TurnEvent.TextDelta(text) => print(text),
                ai.TurnEvent.TurnComplete(_) => print_line(""),
                ai.TurnEvent.ToolCallStart(_, name) => print_line("\n[calling {name}...]"),
                _ => {},
            }
        }
    }

    print_line(tracker.summary());
}
```

### 10.5 Testing with MockProvider

```promise
use ai;

test_agent_responds() `test {
    mock := ai.MockProvider();
    mock.add_response(ai.Response(
        message: ai.Message(
            role: ai.Role.Assistant,
            content: [ai.Content.Text(text: "Hello!")],
            name: none,
            tool_call_id: none,
        ),
        stop_reason: ai.StopReason.EndTurn,
        usage: ai.Usage(input_tokens: 10, output_tokens: 5, cache_read_tokens: none, cache_write_tokens: none),
    ));

    agent := ai.Agent(provider: mock, model: "test");
    result := agent.run!("Hi");

    assert(result == "Hello!");
    assert(mock.call_count == 1);
}
```

---

## 11. Implementation Order

### Phase 0 — Prerequisites (already covered or in progress)

1. **`modules/json`**: already implemented — `json.encode_string[T]()`,
   `json.decode_string[T]()`.
2. **`modules/http`**: currently a placeholder in `catalog.toml` — must be filled in
   (HTTP client with `get`, `post`, SSE streaming support; uses `modules/net`).
3. **`modules/schema` and shared compiler hooks** — see `docs/schema.md` for the full
   plan. The AI platform depends on:
   - The `Type` tagged enum and helper structs.
   - `schema.of[T]()` and `schema.for_func[F]()` free functions.
   - The `_schema_descriptor()` synthesis hook in
     `compiler/internal/sema/serialize.go`.
   - `Hash128`, `Origin`, the `` `id `` meta, and the `[executable]` table in
     `promise.toml`.
4. **Compiler — `` `tool `` meta**: add to `builtinMetas` in
   `compiler/internal/sema/meta.go`; emit per-module `_tool_manifest()` getter (§9.1).

### Phase 1 — AI-specific foundation (auth)

5. **`modules/auth`**: `AuthError`, `Credential`, `TokenProvider`, `StaticToken`,
   `EnvToken`. Credential store (`~/.promise/credentials.toml`) read.

### Phase 2 — Core AI (provider interface + agent + MockProvider)

7. **`modules/ai` error types**: `AiError`, `RateLimitError`, `ContextLengthError`,
   `ApiError`, `ProviderError`.
8. **`modules/ai` core types**: `Message`, `Content`, `Request`, `Response`, `Role`,
   `Usage`, `StopReason`, `StreamEvent`, `ContentDelta`, `ToolCallEvent`.
9. **`modules/ai` Provider interface + MockProvider**. **No** vendor providers ship
   here — those are Phase 7.
10. **`modules/ai` tools**: `Tool`, `Tool.create[T, R]` (depends on `schema`).
11. **`modules/ai` agent**: `Agent`, `AgentConfig`, `RetryConfig`, one-shot `run()`,
    interactive `turn()`.
12. **`modules/ai` streaming**: `run_stream()`, `turn_stream()` returning
    `Generator[T]` (uses `std/iter.pr`'s coroutine generator).

### Phase 3 — MCP

13. **`modules/mcp` transport**: `Transport`, `StdioTransport`.
14. **`modules/mcp` error types**: `McpError`.
15. **`modules/mcp` server**: `Server`, tool/resource/prompt registration, stdio transport.
16. **`modules/mcp` client**: `Client`, `connect_stdio()`, `list_tools()`, `call_tool()`.
17. **`modules/mcp` SSE + HTTP**: `SseTransport`, `HttpTransport`, additional serve modes.
18. **`promise ai serve`**: CLI command for auto-serving `` `tool ``-annotated files —
    reads the per-module `_tool_manifest()` emitted in Phase 0 step 4.

### Phase 4 — Sandbox

19. **`modules/sandbox`**: `Sandbox`, `Capability`, `ExecutionResult`, `SandboxError`.
20. **OS-level enforcement**: seccomp/landlock on Linux, sandbox-exec on macOS.
21. **`promise ai sandbox`**: CLI command.

### Phase 5 — Sessions + Advanced

22. **`modules/ai` sessions**: `Session`, `SessionStore`, `FileSessionStore`,
    `MemorySessionStore`.
23. **`modules/ai` structured output**: `run_typed[T]()`.
24. **`modules/ai` multi-agent**: `Pipeline`, `PipelineStep`, `Tool.from_agent()`.
25. **`modules/ai` observability**: `AgentHooks`, `UsageTracker`.

### Phase 6 — CLI integration

26. **`promise ai tools`**: list tools from a file.
27. **`promise ai schema`**: print JSON Schema for a type or function.

### Phase 7 — Community provider modules (out of catalog)

28. **`ai_anthropic`**: `Anthropic is ai.Provider` — claude-* models.
29. **`ai_openai`**: `OpenAI`, `OpenAICompat is ai.Provider` — OpenAI + Ollama/vLLM/LiteLLM.
30. **`ai_router`**: `Router is ai.Provider` — prefix-based fan-out across providers.
31. **(open-ended)** other vendors as community demand requires.

---

## 12. Resolved Design Decisions

All eleven open questions are now resolved. They are kept here as a decision log so
future readers can see *why* the platform looks the way it does, not only *what* it is.

**Q1: Schema derivation — compile-time or runtime?**
*Decision: compile-time, via monomorphization.* `schema.of[T]()` is a `` `mono `` free
function whose body is synthesized in the same sema pass that generates encode/decode
for `` `serializable `` types (see `docs/schema.md` §7). There is no runtime reflection facility to add
or maintain. `serialization-plan.md` §7.1 already rules out runtime reflection on cost
and philosophy grounds, and the schema mechanism reuses the hook the serializer already
needs.

**Q2: Sandbox enforcement granularity — syscall or PAL?**
*Decision: PAL is the authoritative layer.* No syscall-sandboxing primitive works
consistently across Promise's four targets — Linux (seccomp/landlock), macOS
(sandbox-exec), Windows (Job Objects / AppContainer), WASM (no syscalls at all, only
host imports). The portable contract is the `Capability` enum and the sandbox config;
the PAL is where `Capability[]` is checked against every gated call (§7.4). Specific
PAL implementations *may* additionally engage the platform's syscall sandbox as a
defense-in-depth layer on platforms where one exists, but that is an implementation
detail of that PAL, not part of the user-visible model.

**Q3: MCP transport — which to prioritize?**
*Decision: stdio first, SSE second, streamable HTTP third.* stdio covers local tool
use and `promise ai serve` (the dominant case). SSE covers remote deployment. Streamable
HTTP follows when the spec stabilizes further.

**Q4: Provider streaming protocol differences — abstract or expose?**
*Decision: abstract completely.* `StreamEvent` is the universal type. Provider
implementations translate their wire format to `StreamEvent` at the boundary.
Vendor-specific metadata can be attached as a `map[string, string]` on the event when
needed, so abstraction does not lose information.

**Q5: Session compaction strategy.**
*Decision: caller-supplied summarizer callback with a sensible default.*
`session.compact()` takes a `(Message[]) -> Message!` closure, decoupling `Session`
from `Provider`. The default callback summarizes once estimated tokens exceed 80% of
the model's context window; callers that want a different policy pass their own.

**Q6: Multi-agent communication model.**
*Decision: agent-as-tool for now.* Tools are simple, serial, and fit the existing
agent-loop model. Channels for concurrent agent-to-agent communication can be added
later if real-world patterns demand them; doing it speculatively before the patterns
exist would add surface area without a corresponding use case.

**Q7: `promise ai serve` — auto-discovery scope.**
*Decision: explicit `` `tool `` annotation required.* Following Promise's "explicit
over implicit" philosophy, a function must opt in to MCP exposure. Functions without
`` `tool `` are invisible to the manifest even if their signature would otherwise be
valid. Functions with `` `tool `` and `` `doc() `` produce rich schemas; those with
`` `tool `` alone produce name-only descriptions.

**Q8: Embeddings API — batch size and dimensions.**
*Decision: no compile-time dimension tracking.* `embed()` takes `string[]` and returns
`f64[][]`. Documented per-model batch limits live in each provider module's docs; the
caller checks `f64[]` length at runtime if it matters. Encoding dimensions in the type
system would require dependent types or per-model phantom parameters — too heavyweight
for the common case.

**Q9: `` `tool `` annotation — should it carry metadata?**
*Decision: bare `` `tool `` for now, parameters added if needed.* The annotation
starts with no parameters; the registered tool name is the function name as written.
Renaming via `` `tool(name: "...") `` is a syntactically compatible extension that
can be added later if renaming proves commonly needed in practice. The function name
is already a good tool name in nearly all cases.

**Q10: Should schema generation require `` `serializable ``?**
*Decision: yes, and a missing annotation is a hard compile error.* `schema.of[T]()`
on a non-serializable `T` produces a precise diagnostic of the form:

```
schema.of[Foo]() requires Foo to be marked `serializable`
  → add `serializable on the type definition, or build a schema.Type literal manually if Foo intentionally has no encode/decode
```

This keeps "what I describe" and "what I encode" aligned via a single sema hook
(see `docs/schema.md` §7). The rare type that should be visible to LLMs but intentionally never
serialized is handled by manually constructing a `schema.Type` literal rather than by
forking the synthesis machinery — that case is rare enough that the manual cost is
preferable to a second annotation.

**Q11: Where should community providers live?**
*Decision: separate repos in a community organization, embedded into the catalog by
url + commit pin.* Each provider (e.g. `ai_anthropic`, `ai_openai`) lives in its own
repository. The catalog (`catalog.toml`) embeds them by pinning a `url` plus a
`commit` SHA, identical to the existing pattern for `wasi` / `wasi_preview_2`. This
keeps version pins per-provider and lets vendor-specific contributors maintain their
own modules without monorepo coordination overhead.

**Deferred (not part of this proposal)**: how community catalog inclusion fits into
the monoversion / epoch release validation story. That topic spans more than the AI
platform — it affects every external module the catalog references — and is being
worked through separately. Until that story is settled, community provider modules
ship through the same mechanism as today's `wasi` entries: explicit `url` + `commit`
in `catalog.toml`, no additional epoch metadata.

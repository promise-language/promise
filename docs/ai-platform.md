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

```
modules/
  ai/               — Agent orchestration, provider-neutral LLM interface
  mcp/              — MCP server and client framework
  sandbox/          — Sandboxed code execution with capability control
  schema/           — Type-to-schema derivation (JSON Schema, tool definitions)
  auth/             — Authentication primitives (API keys, OAuth, tokens)
```

All AI modules are explicit imports (`use ai;`, `use mcp;`). None belong in `std/` —
AI orchestration is not universal to every Promise program.

### Prerequisites

The AI modules depend on two catalog modules that must be implemented first:

- **`modules/json`** — JSON encoding/decoding. Used by tool argument serialization
  (`json.decode[T]`, `json.encode`), session persistence, and MCP protocol messages.
- **`modules/http`** — HTTP client. Used by all `Provider` implementations to call
  LLM APIs. Also used by MCP SSE/HTTP transports.

These are listed as future modules in `platform-modules.md` (Section 10). They must be
implemented before Phase 2 of the AI platform.

### Dependencies between modules

```
json/ ──────────▶  (standalone — prerequisite for all AI modules)
http/ ──────────▶  (standalone — prerequisite for ai/ providers)
schema/ ────────▶  json/  (schema serialization)
auth/  ─────────▶  (standalone — credential management)
ai/  ───────────▶  schema/ + auth/ + json/ + http/
mcp/ ───────────▶  schema/ + ai/ + json/
sandbox/ ───────▶  (standalone — no AI dependency; used by ai/ and mcp/)
```

---

## 3. `modules/schema` — Type-Driven Schema Generation

The foundation layer. Derives machine-readable schemas from Promise types using the
`doc()` meta annotations, field types, and structural information already present in
the type system.

### 3.1 The `Schema` Type

```promise
type Schema `public {
    string name;
    string? description;
    SchemaField[] fields;
    map[string, Schema] definitions;    // referenced sub-schemas

    // Derive schema from any type at compile time via monomorphization
    from_type[T]() Schema `mono `public;

    // Serialize to standard formats
    to_json_schema(&this) string `public `instance;
    to_openapi(&this) string `public `instance;
}

type SchemaField `public {
    string name;
    string type_name;           // "string", "int", "bool", "array", "object", etc.
    string? description;        // from `doc on the field
    bool required;
    string? default_value;      // serialized default, if any
    Schema? item_schema;        // for arrays: schema of elements
    Schema? value_schema;       // for maps: schema of values
    string[] enum_values;       // for enums: variant names
}
```

### 3.2 Automatic Derivation

`Schema.from_type[T]()` inspects the type at compile time (monomorphization phase) and
produces a `Schema` value. This uses the same information available to `promise doc`:
field names, types, `doc()` annotations, defaults, optionality (`T?`), and enum variants.

```promise
type CreateUserRequest `doc("Request to create a new user.") {
    string name `doc("The user's full name.");
    string email `doc("Email address. Must be unique.");
    int? age `doc("Age in years. Optional.");
    string role = "viewer" `doc("Role assignment. One of: viewer, editor, admin.");
}

// Derive schema — all field docs, types, defaults, optionality preserved
Schema s = Schema.from_type[CreateUserRequest]();
string json_str = s.to_json_schema();
```

The generated JSON Schema:

```json
{
  "type": "object",
  "description": "Request to create a new user.",
  "properties": {
    "name":  { "type": "string", "description": "The user's full name." },
    "email": { "type": "string", "description": "Email address. Must be unique." },
    "age":   { "type": "integer", "description": "Age in years. Optional." },
    "role":  { "type": "string", "description": "Role assignment. One of: viewer, editor, admin.", "default": "viewer" }
  },
  "required": ["name", "email"]
}
```

### 3.3 Why Not `from_func[F]()`

A natural idea is `ToolDef.from_func[F]()` — derive a tool schema from a function type.
This does not work in Promise because function types erase parameter names and `doc()`
annotations (see language-design §9.5). The type `(string, int) -> bool` carries no names
or documentation — only the parameter types and return type survive.

Tool schema derivation instead works through the **input type**: `Tool.create[T, R]()`
uses `Schema.from_type[T]()` on a struct whose fields carry names, types, docs, defaults,
and optionality. This is more explicit and produces richer schemas than function-type
introspection ever could.

---

## 4. `modules/auth` — Authentication Primitives

Handles API key management, token refresh, and credential storage for AI provider
connections and MCP transport authentication.

```promise
type Credential `public {
    string name;            // logical name: "openai", "anthropic", "github"
    string value;           // the secret value

    // Load from environment variable
    from_env(string var_name) Credential! `factory `public;

    // Load from a credentials file (~/.promise/credentials.toml)
    from_store(string name) Credential! `factory `public;
}

type TokenProvider `public `structural {
    // Returns a valid token, refreshing if needed
    token(~this) string! `abstract `instance;
}

type StaticToken is TokenProvider `public {
    string _value;
    token(~this) string! `instance { return this._value; }
}

type EnvToken is TokenProvider `public {
    string _var_name;
    token(~this) string! `instance {
        return os.get_env(this._var_name) ?: {
            raise AuthError(message: "environment variable '{this._var_name}' not set");
        };
    }
}

type AuthError is error `public {
    // Inherits message from error
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

    token(~this) string! `instance;     // auto-refreshes expired tokens
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
    complete(~this, Request &req) Response! `abstract `instance;

    // Stream a completion request token-by-token.
    // Errors are surfaced as StreamEvent.Error — the stream itself is failable
    // only for transport-level failures (connection lost, etc.).
    stream(~this, Request &req) stream[StreamEvent]! `abstract `instance;

    // Generate embeddings for a batch of inputs
    embed(~this, string[] &inputs, string model) f64[][]! `abstract `instance;

    close(~this)! `instance;
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

type Request `public {
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
// Errors are surfaced as StreamEvent.Error rather than making each yielded
// element failable. This avoids double-failable `stream[StreamEvent!]!` and
// matches how streaming APIs actually work: the stream is opened (failable),
// then events flow until completion or error.

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
with its schema, derived automatically from the function signature and `doc()` annotations.

```promise
// ToolCallEvent carries information about a pending tool invocation.
// Used by tool filters for human-in-the-loop approval.
type ToolCallEvent `public {
    string id;
    string name;
    string arguments_json;
}

type Tool `public {
    string name;
    string? description;
    Schema input_schema;
    (string) -> string! handler;    // takes JSON args, returns JSON result

    // Create a tool from a typed handler function.
    // T = input type (deserialized from JSON), R = output type (serialized to JSON).
    // The schema is derived from T's fields and `doc() annotations.
    create[T, R](
        string name,
        string description,
        (T) -> R! handler
    ) Tool `mono `public {
        Schema schema = Schema.from_type[T]();
        return Tool(
            name: name,
            description: description,
            input_schema: schema,
            handler: |string args_json| -> string! {
                T input = json.decode[T](args_json);
                R result = handler(input);
                return json.encode(result);
            },
        );
    }

    // Wrap an agent as a tool. The tool input is a string prompt;
    // the output is the agent's response.
    from_agent(string name, string description, Agent ~agent) Tool `mono `public;
}
```

Usage:

```promise
use ai;
use json;

type WeatherRequest `doc("Get current weather for a location.") {
    string location `doc("City name or coordinates.");
    string? units `doc("Temperature units: celsius or fahrenheit.");
}

type WeatherResponse {
    f64 temperature;
    string condition;
    int humidity;
}

get_weather(WeatherRequest req) WeatherResponse! {
    // ... actual weather API call
}

main()! {
    Tool weather_tool = Tool.create[WeatherRequest, WeatherResponse](
        name: "get_weather",
        description: "Get current weather for a location.",
        handler: get_weather,
    );

    // Tool schema is automatically derived from WeatherRequest's fields + doc()
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
    run(~this, string user_message) string! `public `instance;

    // Send a message and run with streaming output.
    // Yields text deltas as they arrive, executes tool calls automatically.
    run_stream(~this, string user_message) stream[string]! `public `instance;

    // ── Interactive (multi-turn) mode ──────────────────────────────────

    // Send a user message and get the next agent turn.
    // Returns the full Turn including tool calls and results.
    turn(~this, string user_message) Turn! `public `instance;

    // Send a user message with streaming.
    // Yields TurnEvents as they happen.
    turn_stream(~this, string user_message) stream[TurnEvent]! `public `instance;

    // ── Structured output ──────────────────────────────────────────────

    // Run the agent and parse the response into a typed value.
    // The model is instructed to respond with JSON matching T's schema.
    // Retries once on parse failure with the error message (self-correction).
    run_typed[T](~this, string user_message) T! `public `instance;

    // ── History management ─────────────────────────────────────────────

    // Clear conversation history. Keeps system prompt and tools.
    clear_history(~this) `public `instance {
        this.history = [];
    }

    // Get the full conversation history (shared borrow).
    get_history(&this) Message[]& `public `instance {
        return &this.history;
    }

    // Replace history (e.g. when restoring from a session).
    set_history(~this, Message[] history) `public `instance {
        this.history = history;
    }

    // Fork the agent — creates a copy with cloned history but shared tools/config.
    fork(&this) Agent `public `instance;

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
    (ToolCallEvent) -> bool? tool_filter;  // optional: approve/deny tool calls
    RetryConfig retry;
}

type RetryConfig `public {
    int max_retries = 3;            // max retry attempts for transient errors
    Duration initial_delay = Duration.millis(500);
    f64 backoff_multiplier = 2.0;   // exponential backoff factor
    Duration max_delay = Duration.seconds(30);
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

main()! {
    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a helpful assistant.");
    agent.add_tool(weather_tool);

    // One-shot: returns final text after all tool use is resolved
    string result = agent.run("What's the weather in Paris?");
    println(result);
}
```

**Interactive** (`agent.turn()`): Send messages one at a time, inspect each turn,
and decide whether to continue. Best for conversational UIs and human-in-the-loop flows.

```promise
use ai;
use io;

main()! {
    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a coding assistant.");

    while line := io.read_line()! {
        Turn turn = agent.turn(line);

        if text := turn.text {
            println(text);
        }
    }
}
```

### 5.6 Streaming

Both modes support streaming via `stream[T]` (Promise's generator/iterator type):

```promise
// One-shot streaming — yields text as it arrives
for chunk in agent.run_stream("Explain quantum computing") {
    print(chunk);    // no newline — partial tokens
}
println("");

// Interactive streaming — yields structured events
for event in agent.turn_stream("Write a haiku") {
    match event {
        TurnEvent.TextDelta(text) => print(text),
        TurnEvent.ToolCallStart(id, name) => println("[calling {name}...]"),
        TurnEvent.ToolCallComplete(id, name, result) => println("[{name} done]"),
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
    task[string!][] tasks = [];
    for call in tool_calls {
        tasks.push(go { execute_tool(call) });
    }
    for i, t in tasks {
        results[i] = <-t;
    }
}
```

This uses Promise's existing concurrency model — no new primitives needed.

### 5.8 Tool Call Filtering

The `tool_filter` callback in `AgentConfig` enables human-in-the-loop approval for
sensitive operations:

```promise
agent.config.tool_filter = |ToolCallEvent evt| -> bool {
    if evt.name == "delete_file" {
        println("Agent wants to delete: {evt.arguments_json}");
        print("Allow? (y/n): ");
        return io.read_line()!.trim() == "y";
    }
    return true;    // allow all other tools
};
```

### 5.9 Built-in Providers

```promise
// Anthropic (Claude)
type Anthropic is Provider `public {
    new(~this, string api_key) `public;
    from_env() Self! `factory `public;     // reads ANTHROPIC_API_KEY

    complete(~this, Request &req) Response! `public `instance;
    stream(~this, Request &req) stream[StreamEvent]! `public `instance;
    embed(~this, string[] &inputs, string model) f64[][]! `public `instance;
    close(~this)! `public `instance;
}

// OpenAI
type OpenAI is Provider `public {
    new(~this, string api_key) `public;
    from_env() Self! `factory `public;     // reads OPENAI_API_KEY

    complete(~this, Request &req) Response! `public `instance;
    stream(~this, Request &req) stream[StreamEvent]! `public `instance;
    embed(~this, string[] &inputs, string model) f64[][]! `public `instance;
    close(~this)! `public `instance;
}

// Generic OpenAI-compatible endpoint (Ollama, vLLM, LiteLLM, etc.)
type OpenAICompat is Provider `public {
    new(~this, string base_url, string? api_key) `public;

    complete(~this, Request &req) Response! `public `instance;
    stream(~this, Request &req) stream[StreamEvent]! `public `instance;
    embed(~this, string[] &inputs, string model) f64[][]! `public `instance;
    close(~this)! `public `instance;
}

// Provider that routes to different models/providers based on model name prefix.
// Routes "anthropic/claude-..." to the Anthropic provider, "openai/gpt-..." to OpenAI, etc.
type Router is Provider `public {
    // Register a provider under a prefix. Requests whose model starts with
    // "prefix/" are routed to this provider (prefix stripped before forwarding).
    add_route(~this, string prefix, Provider ~provider) `public `instance;

    // Set the fallback provider for models that match no prefix.
    set_default(~this, Provider ~provider) `public `instance;

    complete(~this, Request &req) Response! `public `instance;
    stream(~this, Request &req) stream[StreamEvent]! `public `instance;
    embed(~this, string[] &inputs, string model) f64[][]! `public `instance;
    close(~this)! `public `instance;
}

// Mock provider for testing — returns canned responses without hitting any API.
type MockProvider is Provider `public {
    Response[] _responses;
    int _call_count;

    new() {
        this._responses = [];
        this._call_count = 0;
    }

    // Queue a response. Responses are returned in order; panics if exhausted.
    add_response(~this, Response resp) `public `instance {
        this._responses.push(resp);
    }

    // Number of times complete() has been called.
    get call_count int `public `instance;

    complete(~this, Request &req) Response! `public `instance;
    stream(~this, Request &req) stream[StreamEvent]! `public `instance;
    embed(~this, string[] &inputs, string model) f64[][]! `public `instance;
    close(~this)! `public `instance;
}
```

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
    save(&this, string path)! `public `instance;

    // Load a session from a file.
    load(string path) Session! `factory `public;

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
    fork(&this) Session `public `instance;

    // Get total token count estimate for the session.
    estimate_tokens(&this) int `public `instance;
}
```

### 5.11 Session Store

For applications that manage multiple users or conversations:

```promise
type SessionStore `public `structural {
    get(~this, string id) Session?! `abstract `instance;
    save(~this, Session &session)! `abstract `instance;
    delete(~this, string id)! `abstract `instance;
    list(~this) string[]! `abstract `instance;           // list session IDs
}

// File-based implementation (one JSON file per session)
type FileSessionStore is SessionStore `public {
    string _dir;
    new(~this, string dir) `public;

    get(~this, string id) Session?! `public `instance;
    save(~this, Session &session)! `public `instance;
    delete(~this, string id)! `public `instance;
    list(~this) string[]! `public `instance;
}

// In-memory implementation (for testing / ephemeral use)
type MemorySessionStore is SessionStore `public {
    new() `public;

    get(~this, string id) Session?! `public `instance;
    save(~this, Session &session)! `public `instance;
    delete(~this, string id)! `public `instance;
    list(~this) string[]! `public `instance;
}
```

### 5.12 Agent with Session

```promise
use ai;
use io;

main()! {
    // Resume or create session
    session := ai.Session.load("session.json") ? { ai.Session.create() };

    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a project manager assistant.");

    // Restore agent history from session (clone — both agent and session keep a copy)
    agent.set_history(session.history.clone());

    // Interactive loop
    while line := io.read_line()! {
        if line == "/quit" { break; }
        if line == "/clear" {
            agent.clear_history();
            session.set_history([]);
            continue;
        }

        Turn turn = agent.turn(line);
        if text := turn.text { println(text); }

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
    as_hooks(&this) AgentHooks `public `instance;

    // Get a summary string.
    summary(&this) string `public `instance;
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
    run(~this, string input) string! `public `instance;

    // Run with streaming — yields events from all steps.
    run_stream(~this, string input) stream[PipelineEvent]! `public `instance;
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

main()! {
    use provider := ai.Anthropic.from_env();

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
    println(result);
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
    send(~this, string message)! `abstract `instance;
    receive(~this) string! `abstract `instance;
    close(~this)! `abstract `instance;
}

type StdioTransport is Transport `public {
    send(~this, string message)! `public `instance;
    receive(~this) string! `public `instance;
    close(~this)! `public `instance;
}

type SseTransport is Transport `public {
    string url;
    send(~this, string message)! `public `instance;
    receive(~this) string! `public `instance;
    close(~this)! `public `instance;
}

type HttpTransport is Transport `public {
    string url;
    send(~this, string message)! `public `instance;
    receive(~this) string! `public `instance;
    close(~this)! `public `instance;
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
    serve_stdio(~this)! `public `instance;

    // Run the server on SSE transport at the given address.
    serve_sse(~this, string addr)! `public `instance;

    // Run the server on streamable HTTP transport.
    serve_http(~this, string addr)! `public `instance;
}

type ServerConfig `public {
    bool logging = true;
    int? max_concurrent_tools;
    (ai.ToolCallEvent) -> bool? auth_filter;    // per-call authorization
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

read_file(FileReadRequest req) FileInfo! {
    string content = io.File.read(req.path);
    return FileInfo(path: req.path, content: content, size: content.len);
}

write_file(FileWriteRequest req) string! {
    io.File.write(req.path, req.content);
    return "Written {req.content.len} bytes to {req.path}";
}

main()! {
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
    connect_stdio(string command, string[] args) Client! `factory `public;

    // Connect to an MCP server via SSE.
    connect_sse(string url) Client! `factory `public;

    // Connect to an MCP server via streamable HTTP.
    connect_http(string url) Client! `factory `public;

    // ── Discovery ──────────────────────────────────────────────────────

    // List available tools on the connected server.
    list_tools(~this) ai.Tool[]! `public `instance;

    // List available resources.
    list_resources(~this) Resource[]! `public `instance;

    // List available prompts.
    list_prompts(~this) Prompt[]! `public `instance;

    // ── Invocation ─────────────────────────────────────────────────────

    // Call a tool by name with JSON arguments.
    call_tool(~this, string name, string arguments_json) string! `public `instance;

    // Read a resource by URI.
    read_resource(~this, string uri) ResourceResponse! `public `instance;

    // Get a prompt by name with arguments.
    get_prompt(~this, string name, map[string, string] args) ai.Message[]! `public `instance;

    // ── Lifecycle ──────────────────────────────────────────────────────

    close(~this)! `public `instance;
}
```

### 6.8 Connecting MCP Tools to an Agent

MCP tools discovered from a server can be directly plugged into an `ai.Agent`:

```promise
use ai;
use mcp;

main()! {
    // Connect to MCP servers
    use file_server := mcp.Client.connect_stdio("file-server", []);
    use db_server := mcp.Client.connect_stdio("db-server", []);

    // Discover tools
    ai.Tool[] file_tools = file_server.list_tools();
    ai.Tool[] db_tools = db_server.list_tools();

    // Create agent with all discovered tools
    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    for tool in file_tools { agent.add_tool(tool); }
    for tool in db_tools { agent.add_tool(tool); }

    // Agent can now use tools from both MCP servers
    result := agent.run("Read the config file and update the database");
    println(result);
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
    run_file(~this, string path) ExecutionResult! `public `instance;

    // Execute inline Promise source code in this sandbox.
    run_code(~this, string code) ExecutionResult! `public `instance;

    // Execute a compiled Promise binary in this sandbox.
    run_binary(~this, string path, string[] args) ExecutionResult! `public `instance;
}

type SandboxConfig `public {
    Duration timeout = Duration.seconds(30);
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
Capability enforcement happens at the **PAL layer** — the sandbox sets up the subprocess
environment so that:

- **Filesystem**: Uses OS-level mechanisms (Linux: seccomp + landlock; macOS: sandbox-exec)
  to restrict file access to granted paths.
- **Network**: Restricts socket operations to allowed hosts/ports.
- **Process**: Restricts `execve` to allowed programs (or disables it entirely).
- **Environment**: Filters environment variables to only those granted.
- **Time/Memory**: Uses `setrlimit` and process-level timeouts.

The Promise compiler can optionally compile in **sandbox mode** (`promise build --sandbox`),
which statically verifies that the code does not use any module whose capabilities exceed
the sandbox grants. This is a *compile-time check* — the program won't even build if it
imports `io` without `FileRead`/`FileWrite` capability.

### 7.5 Agent + Sandbox Integration

```promise
use ai;
use sandbox;

main()! {
    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You write Promise code. Return only code, no explanation.");

    // Create a sandbox for executing AI-generated code
    sb := sandbox.Sandbox.standard();
    sb.allow(sandbox.Capability.FileRead(["/data/*"]));
    sb.config.timeout = Duration.seconds(10);

    // Generate code
    string code = agent.run("Write a program that counts lines in /data/input.txt");

    // Execute in sandbox
    sandbox.ExecutionResult result = sb.run_code(code);

    if result.exit_code == 0 {
        println("Output: {result.stdout}");
    } else {
        println("Error: {result.stderr}");
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
type MovieRecommendation {
    string title `doc("Movie title.");
    int year `doc("Release year.");
    string reason `doc("Why this movie was recommended.");
    f64 confidence `doc("Confidence score 0.0 to 1.0.");
}

main()! {
    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");

    MovieRecommendation rec = agent.run_typed[MovieRecommendation](
        "Recommend a sci-fi movie for someone who loved Arrival."
    );

    println("{rec.title} ({rec.year}): {rec.reason}");
}
```

The `run_typed[T]` method:
1. Generates a JSON Schema from `T` via `Schema.from_type[T]()`
2. Appends schema instructions to the system prompt
3. Parses the model's JSON response into `T` via `json.decode[T]()`
4. Retries once on parse failure with the error message (self-correction)

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

### 9.1 `promise ai serve`

Takes a `.pr` file that defines tools and runs it as an MCP server. The file can be a
full MCP server program (with `main()` and explicit `mcp.Server` setup), or a simpler
"tool file" where functions annotated with `` `tool `` are automatically registered:

```promise
// tools.pr — no main(), no mcp import needed
// promise ai serve tools.pr  auto-registers `tool-annotated functions

add(int a `doc("First number."), int b `doc("Second number.")) int
    `tool `doc("Add two numbers.") {
    return a + b;
}

read_file(string path `doc("File path to read.")) string!
    `tool `doc("Read the contents of a file.") {
    return io.File.read(path);
}
```

`promise ai serve tools.pr` wraps these in an MCP server automatically — no boilerplate.
The tool schemas are derived from the function signatures and `doc()` annotations.

The `` `tool `` annotation is an explicit opt-in — only annotated functions are registered.
This follows Promise's "explicit over implicit" philosophy: a function must declare its
intent to be exposed as a tool.

### 9.2 `promise ai schema`

Prints the JSON Schema for a type, useful for debugging tool definitions:

```
$ promise ai schema CreateUserRequest -f user.pr
{
  "type": "object",
  "description": "Request to create a new user.",
  ...
}
```

---

## 10. End-to-End Examples

### 10.1 CLI Chatbot with Session Persistence

```promise
use ai;
use io;

main()! {
    // Load or create session
    session := ai.Session.load("chat.json") ? { ai.Session.create() };

    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a friendly assistant.");
    agent.set_history(session.history.clone());

    println("Chat (type /quit to exit, /clear to reset)");

    while line := io.read_line()! {
        match line.trim() {
            "/quit" => break,
            "/clear" => {
                agent.clear_history();
                session.set_history([]);
                println("History cleared.");
                continue;
            },
            "/tokens" => {
                println("Estimated tokens: {session.estimate_tokens()}");
                continue;
            },
            _ => {},
        }

        // Stream the response
        for event in agent.turn_stream(line) {
            match event {
                ai.TurnEvent.TextDelta(text) => print(text),
                ai.TurnEvent.TurnComplete(turn) => println(""),
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
use sandbox;

main()! {
    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system(
        "You write Promise programs. Return ONLY the code, no markdown fences or explanation."
    );

    sb := sandbox.Sandbox.standard();
    sb.config.timeout = Duration.seconds(10);

    prompt := "Write a program that prints the first 20 Fibonacci numbers.";

    for attempt in 0..3 {
        string code = agent.run(prompt);
        result := sb.run_code(code);

        if result.exit_code == 0 {
            println("Output:\n{result.stdout}");
            break;
        }

        // Self-correction: feed the error back
        prompt = "The code failed:\n{result.stderr}\nFix the code.";
        println("Attempt {attempt + 1} failed, retrying...");
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

main()! {
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
use mcp;
use io;

main()! {
    // Connect to multiple MCP servers
    use files := mcp.Client.connect_stdio("file-server", []);
    use db := mcp.Client.connect_stdio("db-server", []);
    use web := mcp.Client.connect_stdio("web-search", []);

    use provider := ai.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");

    // Register all tools from all servers
    for tool in files.list_tools() { agent.add_tool(tool); }
    for tool in db.list_tools() { agent.add_tool(tool); }
    for tool in web.list_tools() { agent.add_tool(tool); }

    // Require approval for write operations
    agent.config.tool_filter = |ai.ToolCallEvent evt| -> bool {
        if evt.name == "write_file" || evt.name == "insert" || evt.name == "delete" {
            println("[{evt.name}] {evt.arguments_json}");
            print("Approve? (y/n): ");
            return io.read_line()!.trim() == "y";
        }
        return true;
    };

    // Usage tracking
    tracker := ai.UsageTracker();
    agent.set_hooks(tracker.as_hooks());

    // Interactive session
    while line := io.read_line()! {
        for event in agent.turn_stream(line) {
            match event {
                ai.TurnEvent.TextDelta(text) => print(text),
                ai.TurnEvent.TurnComplete(_) => println(""),
                ai.TurnEvent.ToolCallStart(_, name) => println("\n[calling {name}...]"),
                _ => {},
            }
        }
    }

    println(tracker.summary());
}
```

### 10.5 Testing with MockProvider

```promise
use ai;

test_agent_responds() `test {
    mock := ai.MockProvider();
    mock.add_response(ai.Response(
        message: ai.Message(
            role: Role.Assistant,
            content: [ai.Content.Text(text: "Hello!")],
        ),
        stop_reason: ai.StopReason.EndTurn,
        usage: ai.Usage(input_tokens: 10, output_tokens: 5),
    ));

    agent := ai.Agent(provider: mock, model: "test");
    result := agent.run("Hi")!;

    assert(result == "Hello!");
    assert(mock.call_count == 1);
}
```

---

## 11. Implementation Order

### Phase 0 — Prerequisites

1. **`modules/json`**: JSON encoding/decoding with `json.encode[T]()`, `json.decode[T]()`
2. **`modules/http`**: HTTP client with `get`, `post`, SSE streaming support

### Phase 1 — Foundation (schema + auth)

3. **`modules/schema`**: `Schema`, `SchemaField`, `from_type[T]()`, `to_json_schema()`
4. **`modules/auth`**: `Credential`, `TokenProvider`, `StaticToken`, `EnvToken`

### Phase 2 — Core AI (provider + agent)

5. **`modules/ai` error types**: `AiError`, `RateLimitError`, `ContextLengthError`, `ApiError`
6. **`modules/ai` core types**: `Message`, `Content`, `Request`, `Response`, `Role`, `ToolCallEvent`
7. **`modules/ai` providers**: `Anthropic`, `OpenAI`, `OpenAICompat`, `MockProvider`
8. **`modules/ai` tools**: `Tool`, `Tool.create[T, R]`
9. **`modules/ai` agent**: `Agent`, `AgentConfig`, `RetryConfig`, one-shot `run()`, interactive `turn()`
10. **`modules/ai` streaming**: `stream()`, `run_stream()`, `turn_stream()`

### Phase 3 — MCP

11. **`modules/mcp` transport**: `Transport`, `StdioTransport`
12. **`modules/mcp` error types**: `McpError`
13. **`modules/mcp` server**: `Server`, tool/resource/prompt registration, stdio transport
14. **`modules/mcp` client**: `Client`, `connect_stdio()`, `list_tools()`, `call_tool()`
15. **`modules/mcp` SSE + HTTP**: `SseTransport`, `HttpTransport`, additional serve modes
16. **`promise ai serve`**: CLI command for auto-serving `` `tool ``-annotated files

### Phase 4 — Sandbox

17. **`modules/sandbox`**: `Sandbox`, `Capability`, `ExecutionResult`, `SandboxError`
18. **OS-level enforcement**: seccomp/landlock on Linux, sandbox-exec on macOS
19. **`promise ai sandbox`**: CLI command

### Phase 5 — Sessions + Advanced

20. **`modules/ai` sessions**: `Session`, `SessionStore`, `FileSessionStore`, `MemorySessionStore`
21. **`modules/ai` structured output**: `run_typed[T]()`
22. **`modules/ai` multi-agent**: `Pipeline`, `PipelineStep`, `Tool.from_agent()`
23. **`modules/ai` observability**: `AgentHooks`, `UsageTracker`

### Phase 6 — CLI integration

24. **`promise ai tools`**: list tools from a file
25. **`promise ai schema`**: print JSON Schema for a type

---

## 12. Open Design Questions

**Q1: Schema derivation — compile-time or runtime?**
`Schema.from_type[T]()` is monomorphized at compile time, meaning the schema is a
compile-time constant embedded in the binary. This is efficient but means schema generation
requires compilation. Alternative: runtime reflection via the Type struct (T#t).
**Lean**: compile-time via monomorphization — it is faster, requires no runtime reflection
machinery, and Promise already monomorphizes generics.

**Q2: Sandbox enforcement granularity**
Should the sandbox enforce at the syscall level (seccomp) or at the Promise PAL level
(intercepting `pal_file_open` etc.)? Syscall-level is more secure but platform-specific.
PAL-level is portable but bypassable with `unsafe`.
**Lean**: both — PAL-level for portability and user-friendly error messages, syscall-level
as a defense-in-depth layer on supported platforms.

**Q3: MCP transport: which to prioritize?**
stdio is the most common MCP transport today. SSE is used for remote servers. Streamable
HTTP is the newest spec addition. All three should eventually be supported.
**Lean**: stdio first (covers local tool use and `promise ai serve`), SSE second (covers
remote deployment), streamable HTTP third.

**Q4: Provider streaming protocol differences**
Anthropic and OpenAI have different SSE event formats. Should the `Provider` interface
abstract over these differences completely, or expose provider-specific event types?
**Lean**: abstract completely — `StreamEvent` is the universal type. Provider implementations
translate their wire format to `StreamEvent`. Provider-specific metadata can be attached
via `map[string, string] metadata` on events if needed.

**Q5: Session compaction strategy**
`session.compact()` takes a summarizer callback rather than a provider+model directly.
This decouples Session from Provider — the caller wraps an LLM call in a closure.
The callback receives old messages and returns a single summary message.
**Lean**: configurable with a sensible default callback. Default: summarize when estimated
tokens exceed 80% of the model's context window.

**Q6: Multi-agent communication model**
Should agents communicate through tools (agent-as-tool, current design), through shared
channels, or through a message bus? Tools are simple but serial. Channels enable concurrent
agent communication but add complexity.
**Lean**: tools for now (simple, fits the existing model). Channels for agent-to-agent
communication can be explored later when real-world patterns emerge.

**Q7: `promise ai serve` — auto-discovery scope**
When `promise ai serve` auto-discovers tools from a file, should it register ALL public
functions or only those with a specific annotation?
**Lean**: require an explicit `` `tool `` annotation. This follows Promise's "explicit over
implicit" philosophy — a function must opt in to being exposed as an MCP tool. Functions
without `` `tool `` are not registered. Functions with `` `tool `` and `` `doc() `` get rich
schema descriptions; those with `` `tool `` alone get name-only descriptions.

**Q8: Embeddings API — batch size and dimensions**
The `embed()` method takes `string[]` for batching. Should there be a max batch size?
Should the return type expose vector dimensions for type safety?
**Lean**: no compile-time dimension tracking (too heavyweight for the common case). Document
the model-specific batch limits. The caller checks `f64[]` length at runtime if needed.

**Q9: `` `tool `` annotation — should it carry metadata?**
Should `` `tool `` accept parameters like `` `tool(name: "custom_name") `` to override the
function name in the MCP tool registration?
**Lean**: start with bare `` `tool `` (uses the function name as-is). Add parameterized
form later if renaming proves commonly needed. The function name is already a good tool
name in most cases.

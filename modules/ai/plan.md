# ai — Agent Orchestration (provider-neutral)

**Status: proposed.** Tracker tasks: T0450 (core), T0453 (advanced). Blocked on
T0446 (schema), T0447 (http), T0449 (auth).

`modules/ai` provides a provider-neutral interface for LLM interaction, tool use,
multi-turn conversations, agent loops, session management, and multi-agent
orchestration. The module ships **only** the abstract `Provider` interface and a
built-in `MockProvider` for testing — concrete vendors (Anthropic, OpenAI, etc.)
live in community modules pinned via `catalog.toml` (see T0455 / `ai_anthropic`,
`ai_openai`, `ai_router`).

## Quick start

```promise
use ai;
use ai_anthropic;        // community module — pinned in catalog.toml

main!() {
    use provider := ai_anthropic.Anthropic.from_env();
    agent := ai.Agent(provider: provider, model: "claude-sonnet-4-20250514");
    agent.set_system("You are a helpful assistant.");
    print_line(agent.run("What is the capital of France?"));
}
```

## API surface (summary)

- Error types: `AiError`, `RateLimitError`, `ContextLengthError`, `ApiError`,
  `ProviderError`.
- Core wire types: `Message`, `Content` enum, `Request`, `Response`, `Role`,
  `Usage`, `StopReason`, `StreamEvent`, `ContentDelta`, `ToolCallEvent`.
- `Provider` nominal interface — `complete!`, `stream!`, `embed!`, `close!`.
- `MockProvider is Provider` — built-in for tests.
- `Tool` with `Tool.create[T, R]` factory (consumes `schema.of[T]()`).
- `Agent` with `AgentConfig`, `RetryConfig`, one-shot `run!()` / `run_stream!()`,
  interactive `turn!()` / `turn_stream!()`, `run_typed[T]!` for structured output.
- Session management: `Session`, `SessionStore`, `FileSessionStore`,
  `MemorySessionStore`.
- Multi-agent: `Pipeline`, `PipelineStep`, `Tool.from_agent`.
- Observability: `AgentHooks`, `UsageTracker`.

## Provider story

Vendor providers are not part of this module. They are community modules that
implement `ai.Provider` and ship in their own repos, pinned in `catalog.toml` by
`url`. Examples (each is its own tracker bookkeeping entry under T0455):

- `ai_anthropic` — Claude Messages API.
- `ai_openai` — OpenAI + OpenAI-compatible endpoints (Ollama, vLLM, LiteLLM).
- `ai_router` — prefix-based routing across multiple providers.

## Full design

See [`docs/ai-platform.md`](../../docs/ai-platform.md) §5 for the full module
specification, the streaming model, parallel tool execution, tool-call filtering,
and worked end-to-end examples.

# auth — Authentication Primitives

**Status: proposed.** Tracker task: T0449. Blocked on T0446 (modules/schema).

`modules/auth` provides credentials, tokens, and the credential store used by AI
provider connections and MCP transport authentication. Currently scoped to
API-key / bearer-token use cases; OAuth support is a future extension.

## Quick start

```promise
use auth;

main!() {
    auth.Credential cred = auth.Credential.from_env("anthropic", "ANTHROPIC_API_KEY");
    print_line("loaded {cred.name}");
}
```

## API surface (summary)

- `AuthError is error` with `int code` (0 = missing, 1 = invalid, 2 = refresh failed).
- `Credential` with `from_env!` / `from_store!` factories and a `value()` accessor
  reading `~/.promise/credentials.toml`.
- `TokenProvider` structural interface: `token!(~this) string`.
- `StaticToken is TokenProvider` — fixed bearer.
- `EnvToken is TokenProvider` — re-reads an environment variable on each access.

## Full design

See [`docs/ai-platform.md`](../../docs/ai-platform.md) §4 for the full module
specification, error semantics, and the credential-store layout.

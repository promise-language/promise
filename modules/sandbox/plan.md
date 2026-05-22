# sandbox — Capability-Controlled Code Execution

**Status: proposed.** Tracker task: T0452. No tracker prerequisites — independent
of the rest of the AI platform.

`modules/sandbox` runs Promise code in a restricted subprocess where every
filesystem / network / process / environment / time access is mediated against an
explicit `Capability[]` grant set. Designed for executing AI-generated code where
escape from the sandbox should be an explicit, visible action, not the default.

## Quick start

```promise
use sandbox;

main!() {
    sb := sandbox.Sandbox.standard();              // stdout, stderr, clock
    sb.allow(sandbox.Capability.FileRead(["/data/*"]));
    sb.config.timeout = Duration.from_secs(10);

    sandbox.ExecutionResult result = sb.run_code("...");
    print_line("exit={result.exit_code}: {result.stdout}");
}
```

## API surface (summary)

- `SandboxError is error` with `int? exit_code`, `bool timed_out`,
  `bool memory_exceeded`.
- `Capability` enum: `FileRead`, `FileWrite`, `FileReadAll`, `FileWriteAll`,
  `NetConnect`, `NetListen`, `NetAll`, `Exec`, `ExecAll`, `EnvRead`,
  `EnvReadAll`, `Stdin`, `Stdout`, `Stderr`, `Clock`, `Sleep`.
- `Sandbox` — `allow`, `allow_all`, factories `minimal()` / `standard()` /
  `unrestricted()`, `run_file!`, `run_code!`, `run_binary!`.
- `SandboxConfig` — `timeout`, `max_memory_mb`, `max_output_bytes`, `working_dir`.
- `ExecutionResult` — `exit_code`, `stdout`, `stderr`, `elapsed`, `timed_out`,
  `memory_exceeded`.
- `--sandbox` build flag for compile-time capability verification.

## Enforcement model

Per `docs/ai-platform.md` §12 Q2 the **PAL is the authoritative enforcement
layer** — `compiler/internal/codegen/pal/` mediates every gated call against the
active `Capability[]`. Platform syscall sandboxes (seccomp/landlock on Linux,
sandbox-exec on macOS) may be engaged as defense-in-depth on platforms that
support them, but the portable contract is the `Capability` enum and the PAL
check.

## Full design

See [`docs/ai-platform.md`](../../docs/ai-platform.md) §7 for the full module
specification and integration with `ai.Agent` for AI-generated code execution.

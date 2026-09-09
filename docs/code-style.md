# Promise Code Style

> **Tag:** `code-style` — remaining work to complete this document: `mcp__tracker__list --tag code-style`

Conventions for Promise source code (`.pr` files). These rules apply to standard library, catalog modules, examples, and tests.

## Field and getter naming

Promise types frequently expose internal state through getters. Use a consistent naming convention so callers don't have to type the type name repeatedly and can predict the API from the field name.

### Rules

1. **Prefix private fields with `_`.**
   The underscore marks the field as an implementation detail not intended for direct external access.

2. **Name the public getter with the field name *without* the underscore.**
   The getter is the public-facing name; callers should never see the underscore. The underscore on the field clearly signals that the field is internal and lookups should go through the getter.

3. **Tag construction-only fields with `` `final ``.**
   If a field is only meant to be set in the constructor or factory, mark it `` `final ``. This:
   - Prevents accidental later mutation.
   - Documents intent at the declaration site.
   - Enables future optimizations (the compiler can assume the value never changes).

4. **Do not prefix getters with the type name.**
   `response.status_code`, `response.response_headers`, `request.request_method` all force callers to repeat context they already have at the call site (`response.status`, `response.headers`, `request.method` are unambiguous and shorter). Keep getter names short and field-aligned.

### Example

Bad — getter prefixed with type name, field name not aligned with getter, no `` `final ``:

```promise
type Response `public {
  int status;
  map[string, string] headers;
  string _body;

  get status_code int `public => this.status;
  get response_headers map[string, string] `public => this.headers;
  get body string `public => this._body;
}
```

Good — private fields underscored, getters match field name, immutable fields marked `` `final ``:

```promise
type Response `public {
  int _status `final;
  map[string, string] _headers `final;
  string _body `final;

  get status int `public => this._status;
  get headers map[string, string] `public => this._headers;
  get body string `public => this._body;
}
```

Callers read naturally:

```promise
Response r = http_get(url)?^;
print_line(r.status.to_string());
for k, v in r.headers { ... }
print_line(r.body);
```

### When the field is already public

If the field itself is intended to be part of the public API and there's no derived/transformed accessor, expose it directly without a getter — don't introduce a `_field` + getter pair purely for symmetry. Adding a getter is what justifies the underscore on the field.

## Comments

- **No decorative banner/separator comments.** Lines like `// ── Section ─────` provide no semantic value, consume tokens, and frequently contain non-ASCII characters that corrupt over time. If a section needs documentation, attach a `` `doc `` annotation to the relevant declaration.
- **Default to no comments.** Names should carry the meaning. Only add a comment when the *why* is non-obvious — a hidden constraint, a workaround, a subtle invariant. Don't restate what the code says.

## Documentation annotations

- Always add `` `doc("...") `` on every `` `public `` declaration (types, methods, functions, getters). The `` `doc `` text is the API surface that AI agents and tooling rely on; it should describe behavior, not restate the signature.

## Naming

- Use full English words in public APIs. Approved abbreviations are listed in `docs/language-design.md` §9.3a — when an approved abbreviation exists (e.g. `dir`, `env`, `id`, `len`, `min`, `max`), prefer the abbreviation; otherwise use the full word (`print_line`, not `println`).
- A getter (`get name T`) is for access that is **both** side-effect-free **and** cheap — O(1), field-like (e.g. `len`, `is_empty`, `is_literal`). Use a method (`name() T`) when the operation takes parameters, has side effects, **or has material call cost** (allocation or non-trivial computation). The parentheses are a *cost signal*: they tell the caller "this does work." So `len` is a getter, but `to_string()` (allocates), `clone()` (allocates + deep-copies), `bytes()` (allocates), and `format(w)` (takes a `Writer`) are methods even when parameterless and side-effect-free. When in doubt, ask "is this a field-cheap read?" — yes ⇒ getter, no ⇒ method.
- **Interface conformance overrides the cost signal.** When a `` `structural `` interface declares an accessor as a getter (e.g. `Hashable` declares `get hash int`), every implementor matches that form — even where a particular type's implementation is O(n) (e.g. `string.hash` scans all bytes). A uniform shape across the hierarchy is worth more than the per-type cost signal, and such an accessor still reads as a property.

## Construction

- Use factory methods on the type (e.g. `Response.ok(...)`, `Server.bind(...)`) rather than free functions for constructing instances. Factories can set `` `final `` fields and live alongside the type's other methods. (See [feedback memory](../README.md) — saved separately.)

## Test synchronization

The rule itself lives in the engineering guide — [Time is not a coordinate](org/engineering-guide.md#time-is-not-a-coordinate): nothing synchronizes on time, in tests or anywhere else. What follows is only what is specific to Promise.

- **Join a `go` block over a completion channel.** Declare `channel[bool] done = channel[bool](capacity: 1);` in the test body, make `done.send(true);` the last statement of the block, and `_ := <-done;` where the wait belongs. Every early exit inside the block — an error handler that `return`s — must signal too, or the join hangs on exactly the path the test is about. `tests/concurrency/t1636_go_join_signal_test.pr` is the executable template and pins the exactly-once property; `modules/tls/tls_test.pr` and `modules/http/http_test.pr` are the worked examples.

- **Joining is also what makes a goroutine's assertions count.** An `assert()` that fails inside a `go` block is recovered and discarded, so an unjoined block's assertions are silently vacuous (T2014). A join turns that into a bounded, named `TIMEOUT`, because a panicking goroutine never reaches its `done.send`. Size the `timeout:` annotation to the work the join waits on: the in-binary per-test watchdog already names the test at the 60s default, so the annotation decides how long a hung join *costs*, not whether it is attributed.

- **Never wait for a fire-and-forget goroutine before a leak snapshot.** The batch harness drains every outstanding goroutine — spinning until `gs_created - gs_completed` falls back to the test's own pre-test baseline — *before* it reads `alloc_count` (T1639, `compiler/internal/codegen/compiler.go`). That drain is the barrier, so a test that deliberately discards a goroutine needs no barrier of its own. A `sleep(30ms) // let the goroutine finish tearing down` is not merely unsound, it is dead weight.

- **`// sleep-ok: <reason>` annotates a legitimate call.** A pre-commit guard (`CheckTestSleeps` in `tools/build/common/precommit.go`) rejects any `sleep()` in a `tests/**.pr` or `*_test.pr` file unless that line carries the marker with a non-empty reason. The marker is per-line on purpose: a per-file exemption re-permits every future sleep in a file that earned it for one call. Use it only where the duration is the subject under test — `tests/std/time_test.pr` measuring `sleep()` itself, or `modules/net/net_test.pr` sweeping a delay across a race window and asserting the same outcome at every value, including zero.

## Test scratch paths

The rule itself lives in the engineering guide — [Testing](org/engineering-guide.md#testing): *"Tests never rely on the environment they happen to run in… A test states its whole world or builds it."* `os.temp_dir` is machine-wide, so a fixed name appended to it is a global shared by every process that runs the file. Two concurrent runs on one host — two worktrees, a `-stress` loop beside a verify, two agents, or the same command started twice — then write the same files, and what comes out is `permission denied`, `no such file or directory`, and leak reports: indistinguishable from a genuine io defect, and misleading in the expensive direction, because it makes a clean tree look broken.

- **A test's scratch path includes `os.process_id`.** That is the whole rule, and it is uniqueness by construction rather than by convention. Build the path in one named helper per file and call it at every site; forty hand-written concatenations are forty chances for the next test to copy the literal form.

- **A file that owns many paths hangs them off one per-process directory and removes it last.** `modules/io/io_test.pr` is the worked example: `_scratch_dir()` derives `os.temp_dir + "/pr_iot_" + os.process_id.to_string()` and creates it — idempotently, so no ordered setup step is needed — and `_scratch(name)` hangs each file off it. A final `scratch_dir_teardown()` test removes the directory; tests run in declaration order, so it must stay last — a position dependence that only a real teardown hook (T2032) removes. Its removals are best-effort: a clean run empties the directory itself, and turning a failed run's debris into a second failure would bury the first.

- **A file that owns a handful of names and no directory uses a flat per-process name.** `tests/concurrency/io_syscall_*.pr` are the worked examples: `os.temp_dir + "/pr_sc_" + os.process_id.to_string() + name`, with nothing to create and nothing to remove. Keeping that helper a pure expression is what lets them call it from inside `go` blocks and task bodies with no error path at the call site.

- **`// temp-dir-ok: <reason>` annotates a legitimate use.** A pre-commit guard (`CheckTestTempPaths` in `tools/build/common/precommit.go`) rejects any line naming `temp_dir` in a `tests/**.pr`, `*_test.pr`, or `examples/**.pr` file unless that line also names `process_id` or carries the marker with a non-empty reason. Like `sleep-ok` it is per-line, so a file that earned one exemption does not silently license the next. Use it where `temp_dir` itself is the subject — `modules/os/os_test.pr` tests the getter and never touches the filesystem — or where the directory, not a file in it, is what the operation takes.

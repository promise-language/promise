# Time Module Plan (`modules/time/`)

## Relationship to `std/time.pr`

The two foundational time primitives are defined in the **standard library** (`modules/std/time.pr`), not here:

- **`Duration`** — a pure value type measuring a span of time in nanoseconds. Factory constructors (`from_nanos`, `from_micros`, `from_millis`, `from_secs`, `zero`), accessors (`as_nanos`, `as_millis`, etc.), arithmetic (`+`, `-`, `*`), comparison, `to_string()`, `format!`.
- **`Instant`** — a pure value type representing a point on the monotonic clock. `now()` factory, `elapsed()`, `duration_since()`, comparison.
- **`sleep(Duration)`** — free function that pauses the current goroutine.

These are auto-imported into every file via `use std as _`. They are always available without `use time;`. The `time` catalog module builds **on top of** these primitives — it does not redefine or wrap them.

---

## Scope of the `time` Catalog Module

The `time` module provides wall-clock time, date/calendar types, formatting, and parsing — capabilities that go beyond monotonic measurement. Import with `use time;`.

### Types

#### `DateTime`
Wall-clock date and time (UTC or offset-aware). Backed by Unix epoch nanoseconds internally.

```
type DateTime `public `doc("A calendar date and time, with optional UTC offset.") {
    int _epoch_nanos `value;   // nanoseconds since Unix epoch (1970-01-01T00:00:00Z)
    int _offset_secs `value;   // UTC offset in seconds (0 for UTC)

    // Factory constructors
    now() DateTime `factory;                           // current wall-clock time (UTC)
    from_unix_secs(int secs) DateTime `factory;        // from Unix timestamp (seconds)
    from_unix_millis(int ms) DateTime `factory;        // from Unix timestamp (milliseconds)
    from_unix_nanos(int ns) DateTime `factory;         // from Unix timestamp (nanoseconds)
    create(int year, int month, int day,
           int hour, int minute, int second) DateTime `factory;  // from components (UTC)
    parse!(string s) DateTime `factory;                          // ISO 8601 / RFC 3339

    // Component accessors
    get year int;
    get month int;        // 1-12
    get day int;          // 1-31
    get hour int;         // 0-23
    get minute int;       // 0-59
    get second int;       // 0-59
    get nanosecond int;   // 0-999_999_999
    get weekday int;      // 0=Sunday, 6=Saturday
    get day_of_year int;  // 1-366

    // Unix epoch accessors
    get unix_secs int;
    get unix_millis int;
    get unix_nanos int;

    // Arithmetic with Duration (from std)
    +(Duration d) DateTime;
    -(Duration d) DateTime;

    // Difference between two DateTimes
    duration_since(DateTime earlier) Duration;

    // Comparison
    == != < > <= >=

    // Offset
    with_offset(int offset_secs) DateTime;
    to_utc() DateTime;
    get offset_secs int;
    get is_utc bool;

    // Formatting
    to_string() string;        // ISO 8601: "2026-04-11T14:30:00Z"
    format!(Writer ~w);
    format_rfc3339() string;   // "2026-04-11T14:30:00+00:00"
}
```

#### `Date`
Calendar date without time-of-day. Pure value type.

```
type Date `public `doc("A calendar date (year, month, day) without time-of-day.") {
    int _days `value;  // days since epoch

    // Factory constructors
    today() Date `factory;
    create(int year, int month, int day) Date `factory;
    parse!(string s) Date `factory;    // "2026-04-11"

    // Component accessors
    get year int;
    get month int;
    get day int;
    get weekday int;
    get day_of_year int;

    // Arithmetic
    add_days(int n) Date;
    duration_since(Date earlier) Duration;

    // Comparison
    == != < > <= >=

    // Conversion
    at(int hour, int minute, int second) DateTime;

    to_string() string;    // "2026-04-11"
    format!(Writer ~w);
}
```

#### `Time`
Time-of-day without a date. Pure value type.

```
type Time `public `doc("A time of day (hour, minute, second, nanosecond) without a date.") {
    int _nanos `value;  // nanoseconds since midnight

    // Factory constructors
    create(int hour, int minute, int second) Time `factory;
    parse!(string s) Time `factory;    // "14:30:00"
    midnight() Time `factory;
    noon() Time `factory;

    // Component accessors
    get hour int;
    get minute int;
    get second int;
    get nanosecond int;

    // Arithmetic
    +(Duration d) Time;
    -(Duration d) Time;
    duration_since(Time earlier) Duration;

    // Comparison
    == != < > <= >=

    to_string() string;    // "14:30:00"
    format!(Writer ~w);
}
```

### No Free Functions

All construction and parsing goes through factory methods on the target type. No free-floating convenience wrappers — there is one obvious way to do each thing:

- `DateTime.now()` — current wall-clock time
- `DateTime.parse!(s)` — parse ISO 8601 / RFC 3339
- `Date.today()` — current date
- `Date.parse!(s)` — parse "2026-04-11"
- `Time.parse!(s)` — parse "14:30:00"

Factories can set `` `final `` fields during construction, ensuring hermetic, immutable instances.

### Helpers (internal)

Calendar math utilities (leap year, days-in-month, day-of-week) will be internal functions — not exported. These are standard algorithms (civil_from_days / days_from_civil, Howard Hinnant's approach or equivalent).

---

## PAL Requirements

The module needs one new PAL extern:

- **`promise_wallclock`** — returns nanoseconds since Unix epoch (1970-01-01T00:00:00Z) as `i64`. Implementation: `clock_gettime(CLOCK_REALTIME)` on POSIX, `GetSystemTimeAsFileTime` on Windows, `__wasi_clock_time_get(CLOCK_REALTIME)` on WASM.

All other time operations (component extraction, formatting, parsing, arithmetic) are pure calendar math implemented in Promise — no additional native calls needed. The existing `_nanotime` (monotonic) and `_sleep_nanos` remain in std.

---

## Implementation Phases

### Phase 1: PAL + DateTime Core
- Add `promise_wallclock` PAL function (codegen/io.go or codegen/pal/)
- Implement `DateTime` with `now()`, Unix epoch constructors/accessors, `to_string()` (ISO 8601)
- Calendar math helpers (internal): epoch-to-components, components-to-epoch, leap year
- Arithmetic with `Duration`, comparison operators
- Tests

### Phase 2: Date and Time Types
- Implement `Date` (calendar date, `today()`, component accessors, arithmetic)
- Implement `Time` (time-of-day, component accessors)
- Conversion between types (`Date.at()`, `DateTime` to `Date`/`Time`)
- Tests

### Phase 3: Parsing
- `DateTime.parse!` — ISO 8601 / RFC 3339 string parsing
- `Date.parse!`, `Time.parse!` — component parsing
- Error types for parse failures
- Tests

### Phase 4: Formatting and Offset Support
- `DateTime.with_offset()`, `to_utc()`, offset-aware formatting
- `format_rfc3339()`
- Extended `format!` support
- Tests

---

## Design Decisions

- **No timezone database.** Timezone handling (IANA tz, DST rules) is complex and requires either bundled data or OS integration. The `time` module supports fixed UTC offsets only. Full timezone support is a potential future module (`modules/tz/`).
- **Pure value types.** `DateTime`, `Date`, and `Time` are all pure value types (all fields `` `value ``). No heap allocation, automatic copy semantics.
- **Duration comes from std.** All duration arithmetic reuses `std.Duration` — no wrapper, no re-export.
- **Calendar math in Promise.** Component extraction and date arithmetic are pure functions — no reason to drop to native code. Only the wall-clock read is a PAL call.
- **Failable parsing.** All parse functions are failable (`!`) — invalid input raises an error, consistent with Promise's explicit error handling philosophy.

# WASM Binary Size: Regression Prevention & Optimization

## Motivation

Promise targets browser WebAssembly where download size directly impacts load time. Binary size has not been actively tracked, risking silent regressions. This document defines a phased plan: prevent regressions first, then optimize.

## Current Baseline (2026-03-23)

| Program | Features used | WASM size |
|---------|--------------|-----------|
| `print_line("hello")` | minimal runtime | 6 KB |
| channels example | concurrency stubs | 12 KB |
| vectors example | generics, collections | 13 KB |
| variables example | strings, formatting | 24 KB |
| basics.pr (large e2e) | broad stdlib | 38 KB |

Native (macOS arm64) hello is ~53 KB for comparison. Sizes are already reasonable thanks to `--lto-O2` but there is no mechanism to detect regressions.

## Phase 1: Regression Prevention

### Step 1 — Size canary programs (`tests/size/`)

Create a fixed set of `.pr` files under `tests/size/` that serve as size benchmarks. Each exercises a specific slice of the stdlib/runtime:

| Canary file | What it measures |
|-------------|-----------------|
| `canary_minimal.pr` | Base runtime: just `print_line("hello")` |
| `canary_strings.pr` | String ops: split, to_upper, contains, replace, formatting |
| `canary_collections.pr` | Vectors, maps, iteration, generics monomorphization cost |
| `canary_concurrency.pr` | Channels, goroutines — scheduler stub inclusion |
| `canary_full.pr` | Uses most stdlib features — worst-case size |

These files are checked in and must not change without updating the baseline. They are **not** test files (no `main` with `test` annotation) — they are plain programs compiled with `promise build`.

### Step 2 — Size report script (`bin/size-report.sh`)

A script that compiles each canary to `wasm32-wasi` and reports sizes:

```bash
bin/size-report.sh              # print TSV: name, bytes, KB
bin/size-report.sh --check      # compare against baseline, exit 1 on regression
bin/size-report.sh --update     # overwrite baseline with current sizes
```

**Baseline file:** `tests/size/baseline.tsv` — checked in, machine-readable:

```
canary_minimal	6009
canary_strings	15234
canary_collections	13500
canary_concurrency	11800
canary_full	38000
```

**Regression threshold:** fail if any canary exceeds baseline by more than **max(5%, 512 bytes)**. This allows minor fluctuations from LLVM version changes while catching real regressions.

### Step 3 — Integrate into `bin/verify.sh --wasm`

After WASM tests pass, add:

```bash
# --- WASM size regression check ---
echo "Checking WASM binary sizes..."
if ! bin/size-report.sh --check; then
  echo "WASM size regression detected!"
  echo "If intentional, run: bin/size-report.sh --update"
  exit 1
fi
```

This makes size regression a **commit blocker** — same as test failures.

### Step 4 — Informational size stats in `promise test`

When running `promise test -target wasm32-wasi`, after all tests pass, print aggregate size stats in the summary:

```
568 passed, 0 failed (117 files, 30.810s)
WASM size: total 1.2MB, largest: basics.pr (38KB), median: 11KB
```

This is informational only (not a gate). Implementation: `stat` each compiled binary before cleanup. Only shown for WASM targets.

---

## Phase 2: Understand What's In The Binary

### Step 5 — `promise size <file.wasm>` command

A built-in analysis command that parses the WASM binary format and reports:

```
$ promise size hello.wasm
Total: 6,009 bytes

Section breakdown:
  type       142 bytes   2.4%   (12 type entries)
  import      87 bytes   1.4%   (3 imports)
  function    45 bytes   0.7%   (38 functions)
  memory       7 bytes   0.1%
  export      23 bytes   0.4%   (1 export)
  code      4,891 bytes  81.4%  (38 function bodies)
  data        512 bytes   8.5%  (2 data segments)
  name        302 bytes   5.0%  (debug names — strippable)

Code size by origin (heuristic, from name section):
  user code          890 bytes  18.2%
  std runtime      2,100 bytes  42.9%
  allocator          800 bytes  16.3%
  string ops         600 bytes  12.3%
  other              501 bytes  10.2%
```

Implementation: WASM binary format is simple (magic + version + typed sections with varuint length). Parse in Go, no external tools needed. The "origin" breakdown uses function name prefixes (`promise_`, `pal_`, user-defined names) as heuristics.

### Step 6 — `promise size --compare <a.wasm> <b.wasm>`

Section-by-section diff for A/B testing optimizations:

```
$ promise size --compare before.wasm after.wasm
Total: 6,009 → 5,201 (-808 bytes, -13.4%)

  code:   4,891 → 4,583 (-308 bytes)
  data:     512 →   512 (unchanged)
  name:     302 →     0 (-302 bytes, stripped)
  other:    304 →   106 (-198 bytes)
```

---

## Phase 3: Low-Hanging Optimizations

### Step 7 — Strip WASM name section

The WASM name section contains function/local names for debugging. It's typically 5-15% of binary size and serves no purpose in production.

Add a `-strip` flag (or include stripping in `-release` mode):

```bash
promise build -target wasm32-wasi -strip -o hello.wasm hello.pr
```

Implementation options (in priority order):
1. `llc` flag to suppress name section emission
2. Post-link binary rewrite: truncate the last section if it's type 0 with name "name"
3. Integrate `wasm-strip` from WABT if available

### Step 8 — DCE audit: what survives LTO?

Using `promise size`, audit what dead code survives LTO in the minimal canary:

**Questions to answer:**
- Does scheduler code (M:N runtime) survive in WASM binaries despite being stubbed? If yes, mark scheduler globals/functions as `private` or `internal` linkage so LTO can remove them.
- Does `use std as _` cause all 29 std files to be linked? If LTO doesn't strip unused std functions, consider: (a) marking std functions as `linkonce_odr`, (b) splitting std into finer-grained compilation units, or (c) making auto-import smarter (only import referenced declarations).
- Are RTTI/vtable globals emitted for types never `is`-checked? If so, consider lazy emission.

### Step 9 — Evaluate `wasm-opt -Oz` post-processing

[Binaryen](https://github.com/WebAssembly/binaryen)'s `wasm-opt -Oz` is the industry standard for WASM size optimization. It performs optimizations LLVM doesn't:
- Stack IR compression
- Duplicate function merging
- Unreachable code elimination
- Local/global optimization beyond LLVM's scope

Typical savings: 10-30% beyond LLVM LTO.

Add as an optional post-processing step:

```bash
promise build -target wasm32-wasi -Os -o hello.wasm hello.pr
# internally: ... → wasm-ld → wasm-opt -Oz → hello.wasm
```

Guard behind flag since it requires Binaryen installation. Consider embedding `wasm-opt` in release builds (like LLVM tools are embedded).

### Step 10 — Evaluate `opt -Oz` for WASM target

Currently WASM uses `opt -O1` → `wasm-ld --lto-O2`. For WASM, `-Oz` (optimize for size) may be better than `-O1`:
- Trades inlining aggressiveness for smaller code
- Acceptable for WASM where download latency > CPU execution time
- Can be WASM-target-only (keep `-O1` for native)

Measure with canaries before/after. If size wins are >5% with <10% perf regression, make `-Oz` the WASM default.

---

## Phase 4: Structural Optimizations

### Step 11 — Lazy std import

Currently `use std as _` is injected into every file, pulling in all 29 std files. If LTO doesn't fully eliminate unused code (determined in Step 8), make the auto-import smarter:

- During sema, track which std declarations are actually referenced
- Only include std compilation units that contain referenced declarations
- This could dramatically reduce binary size for programs that use few std features

### Step 12 — WASM-specific codegen tuning

- **Scheduler elimination**: For single-threaded WASM, don't emit scheduler infrastructure at all (not just stub it). Gate on target triple during codegen.
- **String literal strategy**: Evaluate whether `.rodata` string literals are optimal for WASM data segments or whether a different encoding is better.
- **Vector COW**: WASM is single-threaded, so COW machinery for vector literals may be simplifiable.
- **`bulk-memory` usage**: Ensure `memcpy`/`memset` use WASM bulk memory operations (already enabled via `-mattr=+bulk-memory`).

### Step 13 — Compression-aware optimization

For web deployment, WASM files are typically served with gzip/brotli compression. Optimize for **compressed** size:
- Measure compressed sizes in canaries alongside raw sizes
- Some optimizations that reduce raw size may not help compressed size (and vice versa)
- Consider `wasm-opt --converge -Oz` which iterates until stable

---

## Implementation Order

| Priority | Steps | Effort | Value |
|----------|-------|--------|-------|
| **P0** | 1-3 (canaries + size gate in verify) | 1-2 days | Prevents all future regressions |
| **P1** | 5 (size command) | 1 day | Enables informed optimization |
| **P1** | 7 (strip names) | Half day | ~5-15% size reduction, trivial |
| **P2** | 8 (DCE audit) | 1 day | Identifies biggest optimization opportunities |
| **P2** | 9-10 (wasm-opt, opt -Oz) | 1 day | ~10-30% size reduction |
| **P3** | 4, 6 (test stats, compare) | 1 day | Developer experience |
| **P3** | 11-13 (structural) | Multi-day | Only if earlier steps show need |

**Rule: Steps 1-3 must be done before any optimization work.** Without regression prevention, optimizations can be silently undone by subsequent changes.

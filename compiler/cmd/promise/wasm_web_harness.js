// Node.js harness for running Promise wasm32-web binaries (T0315).
// Invoked by `promise test/exec/run -target wasm32-web` via the embedded
// runner in wasm_web_runner.go. Requires Node 20+.
//
// Usage: node wasm_web_harness.js <path-to-wasm>
//
// The harness:
//   - reads the .wasm file and instantiates it,
//   - provides `promise_env.write(fd, ptr, len) -> i64` (BigInt) routed to
//     process.stdout / process.stderr, and `promise_env.exit(code) -> never`
//     routed to process.exit,
//   - calls instance.exports._initialize() (the wasm32-web entry point),
//   - runs every continuation the program schedules through
//     `promise_env.schedule_pump(kind, delay_ms)` — a later
//     instance.exports.promise_web_pump(), after delay_ms for kind 1 (a timer)
//     and as soon as possible otherwise,
//   - exits 3 if the instance is still alive with no continuation pending,
//     after _initialize or after a continuation's pump.
//
// A program with no live host registration ends _initialize in exit, and so
// does every batch-test binary. A program whose goroutines sleep is a reactor
// until they wake (docs/web-apps.md "The liveness rule"); its drains schedule
// the pumps that wake them, which this harness runs. A program still alive with
// nothing scheduled waits on host events this harness never delivers, and its
// leak accounting never runs, so it fails rather than passing on whatever it
// printed during setup.
//
// It deliberately does NOT load the auto-generated bootstrap .js next to the
// .wasm — that file is browser-only (uses fetch) and would also keep the
// importObject private to its own module.

"use strict";

const fs = require("fs");

if (process.argv.length < 3) {
  console.error("usage: node wasm_web_harness.js <path-to-wasm>");
  process.exit(2);
}

const wasmPath = process.argv[2];
let buf;
try {
  buf = fs.readFileSync(wasmPath);
} catch (e) {
  console.error(`harness: cannot read ${wasmPath}: ${e.message}`);
  process.exit(2);
}

// Buffered stdout/stderr writer.
//
// process.stdout.write may be async-buffered when stdout is not a TTY
// (e.g. piped to the Go test runner via cmd.CombinedOutput). If we call
// process.exit synchronously after a write, Node truncates the pipe and the
// last chunk is lost. The harness avoids this by buffering all writes in
// memory and flushing them before exit using fs.writeSync, which always
// writes synchronously.
const stdoutBuf = [];
const stderrBuf = [];

function bufferWrite(fd, data) {
  if (fd === 2) {
    stderrBuf.push(data);
  } else {
    // Treat any non-2 fd as stdout. Promise's pal_write is invoked with
    // fd=1 for print_line, fd=2 for panic; we don't expect arbitrary fds.
    stdoutBuf.push(data);
  }
}

function flushBuffers() {
  for (const chunk of stdoutBuf) {
    fs.writeSync(1, chunk);
  }
  stdoutBuf.length = 0;
  for (const chunk of stderrBuf) {
    fs.writeSync(2, chunk);
  }
  stderrBuf.length = 0;
}

let instance = null;

function memoryView(ptr, len) {
  // memory.buffer can be replaced after grow(); always re-read.
  return new Uint8Array(instance.exports.memory.buffer, ptr, len);
}

function exitWith(code) {
  flushBuffers();
  // process.exit terminates the event loop synchronously after fs.writeSync
  // has drained both streams.
  process.exit(code | 0);
}

// Core promise_env functions (write + exit). Wrapped in a Proxy so that
// unknown imports (e.g., web module bindings like Console.log, Node.appendChild)
// get no-op stubs that return 0 rather than failing instantiation.
const _promiseEnvImpl = {
  write: (fd, ptr, len) => {
    // i64 args/returns are BigInts in default WebAssembly mode.
    const lenN = Number(len);
    if (lenN > 0) {
      const view = memoryView(Number(ptr), lenN);
      // Buffer.from copies; necessary because fs.writeSync may not handle
      // a memory-backed Uint8Array if the buffer detaches before the write.
      bufferWrite(Number(fd), Buffer.from(view));
    }
    return BigInt(lenN);
  },
  exit: (code) => {
    exitWith(Number(code));
    // process.exit never returns, but tell the WASM runtime we're done.
    throw new Error("promise_env.exit");
  },
  // T0680: monotonic clock source for wasm32-web. Returns nanoseconds as a
  // BigInt (i64) — process.hrtime.bigint() is a monotonic high-resolution
  // counter. Without this the IR import would fall through the Proxy stub to
  // () => 0, freezing all WASM time at 0.
  monotonic_nanos: () => process.hrtime.bigint(),
  // The reactor's continuation (docs/web-apps.md "Budget and continuation"):
  // pump again after delayMs for kind 1 (a timer), as soon as possible for
  // kind 0 (ASAP) and kind 2 (an animation frame — Node has none). One timer
  // is kept: a request no earlier than the pending one is already covered by
  // it, and an earlier one replaces it. Defined here so it can never fall
  // through to the no-op stub below.
  schedule_pump: (kind, delayMs) => {
    const pump = () => enter("promise_web_pump", () => instance.exports.promise_web_pump());
    if (kind !== 1) {
      pendingAsap++;
      setImmediate(() => {
        pendingAsap--;
        pump();
      });
      return;
    }
    const at = performance.now() + delayMs;
    if (timerPump !== null) {
      if (timerPump.at <= at) return;
      clearTimeout(timerPump.handle);
    }
    timerPump = {
      at,
      handle: setTimeout(() => {
        timerPump = null;
        pump();
      }, delayMs),
    };
  },
  // T1571: CSPRNG random bytes for the crypto module. Fills buf with len bytes
  // of cryptographically-secure randomness using Node's crypto.getRandomValues.
  // getRandomValues throws QuotaExceededError above 65536 bytes, so loop in
  // chunks (same pattern as the POSIX PAL's 256-byte getentropy loop).
  // Returns 0 on success.
  random_bytes: (buf, len) => {
    const crypto = require("crypto");
    let offset = 0;
    while (offset < len) {
      const chunk = Math.min(len - offset, 65536);
      const view = new Uint8Array(instance.exports.memory.buffer, buf + offset, chunk);
      crypto.getRandomValues(view);
      offset += chunk;
    }
    return 0;
  },
};

// Continuations scheduled through schedule_pump that have not run yet: the
// as-soon-as-possible ones, and the one timer ({at, handle}, or null).
let pendingAsap = 0;
let timerPump = null;

// enter makes one host entry into the instance (_initialize, or a scheduled
// pump). An entry that ends the program exits through promise_env.exit; one
// that traps fails the run. Once an entry returns with no continuation
// pending, the instance is a reactor idle on host events (see the header).
function enter(name, entry) {
  try {
    entry();
  } catch (e) {
    // The WASM "exit" import throws to unwind; ignore that specific case.
    if (e && e.message === "promise_env.exit") return;
    flushBuffers();
    console.error(`harness: ${name} trapped: ${e && e.stack ? e.stack : e}`);
    process.exit(1);
  }
  if (pendingAsap === 0 && timerPump === null) {
    bufferWrite(2, Buffer.from(
      "harness: reactor idle — live host registrations remain and the headless harness delivers no host events\n"));
    exitWith(3);
  }
}

const importObject = {
  promise_env: new Proxy(_promiseEnvImpl, {
    get: (target, prop) => {
      if (prop in target) return target[prop];
      // No-op stub for unknown imports (web module bindings, etc.).
      return (..._args) => 0;
    },
  }),
};

(async () => {
  let module;
  try {
    module = await WebAssembly.compile(buf);
  } catch (e) {
    console.error(`harness: compile failed: ${e.message}`);
    process.exit(2);
  }

  // Defensive: if the module ever imports wasi_snapshot_preview1 (e.g. a
  // mis-targeted build), provide stubs so instantiate doesn't fail outright.
  // This isn't expected for wasm32-web but makes diagnosis easier.
  for (const imp of WebAssembly.Module.imports(module)) {
    if (imp.module === "wasi_snapshot_preview1") {
      if (!importObject.wasi_snapshot_preview1) {
        importObject.wasi_snapshot_preview1 = new Proxy({}, {
          get: (_t, name) => () => {
            console.error(`harness: unexpected WASI import wasi_snapshot_preview1.${String(name)}`);
            return 0;
          },
        });
      }
    }
  }

  try {
    instance = await WebAssembly.instantiate(module, importObject);
  } catch (e) {
    flushBuffers();
    console.error(`harness: instantiate failed: ${e.message}`);
    process.exit(2);
  }

  if (typeof instance.exports._initialize !== "function") {
    flushBuffers();
    console.error("harness: wasm module has no _initialize export");
    process.exit(2);
  }

  enter("_initialize", () => instance.exports._initialize());
})();

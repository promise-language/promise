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
//   - exits 0 if _initialize returns without invoking promise_env.exit.
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

const importObject = {
  promise_env: {
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
  },
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

  try {
    instance.exports._initialize();
  } catch (e) {
    // The WASM "exit" import throws to unwind; ignore that specific case.
    if (e && e.message === "promise_env.exit") return;
    flushBuffers();
    console.error(`harness: _initialize trapped: ${e && e.stack ? e.stack : e}`);
    process.exit(1);
  }

  // _initialize returned without calling promise_env.exit — treat as success.
  exitWith(0);
})();

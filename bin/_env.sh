# Deterministic PATH for build scripts. Sourced by build, verify, test, coverage.
# Constructed from known install locations (see bin/install-prereqs.sh).
# No user profile sourcing — works identically interactive and non-interactive.
#
# Note: LLVM tools (opt, llc, lld) are NOT included here — the compiled
# bin/promise binary finds them via findLLVMTool() with its own search logic.

# System
PATH="/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin"

# Homebrew (macOS): Apple Silicon (/opt/homebrew)
PATH="/opt/homebrew/bin:$PATH"

# Snap packages (Ubuntu/Linux)
PATH="/snap/bin:$PATH"

# Go: official tarball (/usr/local/go/bin) + go install'd tools ($HOME/go/bin)
PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"

# Java for ANTLR parser generation (Homebrew keg-only on macOS; apt on Linux puts it in /usr/bin)
PATH="/opt/homebrew/opt/openjdk/bin:$PATH"

# wasmtime (installed by install-prereqs.sh --wasm)
PATH="$HOME/.wasmtime/bin:$PATH"

# Disable epoch shim dispatch in development. Build scripts always use the
# locally built bin/promise, not the installed epoch binary. Without this,
# shimDispatch() may redirect to an outdated installed binary, silently
# bypassing recent codegen fixes (B0239).
export PROMISE_NO_SHIM=1

export PATH

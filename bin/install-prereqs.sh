#!/usr/bin/env bash
set -euo pipefail

# Install all prerequisites for building the Promise compiler.
# Run with: sudo bin/install-prereqs.sh
#
# Prerequisites:
#   Linux: LLVM 22+ (opt, llc, lld), musl-dev, Go 1.25+, Java 11+ (for ANTLR)
#   macOS: Homebrew LLVM 22+ (opt, llc) + LLD (ld64.lld), Xcode CommandLineTools, Go 1.25+, Java 11+
#
# Optional (for --target wasm32-wasi):
#   wasmtime: WASI runtime for running/testing WASM binaries

MIN_LLVM=22
MAX_LLVM=25
INSTALL_WASM=0

for arg in "$@"; do
    case "$arg" in
        --wasm) INSTALL_WASM=1 ;;
    esac
done

OS=$(uname -s)

# --- Helper functions ---

has_cmd() { command -v "$1" &>/dev/null; }

check_go() {
    if has_cmd go; then
        echo "  go: $(go version | awk '{print $3}')"
    else
        echo "  go: NOT FOUND"
        echo "    Install Go 1.25+: https://go.dev/dl/"
        return 1
    fi
}

check_java() {
    if has_cmd java; then
        echo "  java: $(java -version 2>&1 | head -1)"
    else
        echo "  java: NOT FOUND"
        echo "    Install Java 11+ (for ANTLR parser generation)"
        return 1
    fi
}

check_wasmtime() {
    if has_cmd wasmtime; then
        echo "  wasmtime: $(wasmtime --version 2>&1)"
    else
        echo "  wasmtime: NOT FOUND (optional, for --target wasm32-wasi)"
        return 1
    fi
}

install_wasmtime() {
    if has_cmd wasmtime; then
        return 0
    fi
    echo "Installing wasmtime (WASI runtime)..."
    curl https://wasmtime.dev/install.sh -sSf | bash
    echo "  NOTE: restart your shell or run 'source ~/.bashrc' to add wasmtime to PATH"
}

# Find the best available LLVM version (newest >= MIN_LLVM)
find_llvm_version() {
    for v in $(seq $MAX_LLVM -1 $MIN_LLVM); do
        if has_cmd "opt-$v"; then
            echo "$v"
            return 0
        fi
    done
    if has_cmd opt; then
        local ver
        ver=$(opt --version 2>&1 | grep -oE 'LLVM version [0-9]+' | grep -oE '[0-9]+' || echo 0)
        if [ "$ver" -ge "$MIN_LLVM" ] 2>/dev/null; then
            echo "$ver"
            return 0
        fi
    fi
    echo "0"
}

# --- Linux ---

install_linux() {
    echo "Platform: Linux ($(lsb_release -ds 2>/dev/null || cat /etc/os-release | grep PRETTY_NAME | cut -d= -f2 | tr -d '"'))"
    echo ""

    # Detect distro codename (for apt.llvm.org repo URL)
    local codename=""
    if [ -f /etc/os-release ]; then
        codename=$(. /etc/os-release && echo "${VERSION_CODENAME:-}")
    fi

    echo "Checking existing tools..."
    local llvm_ver
    llvm_ver=$(find_llvm_version)
    if [ "$llvm_ver" -ge "$MIN_LLVM" ] 2>/dev/null; then
        echo "  LLVM: $llvm_ver (opt-$llvm_ver, llc-$llvm_ver, ld.lld-$llvm_ver)"
    else
        echo "  LLVM: not found (need $MIN_LLVM+)"
    fi

    if [ -f /usr/lib/x86_64-linux-musl/libc.a ]; then
        echo "  musl-dev: installed"
    else
        echo "  musl-dev: NOT FOUND"
    fi
    check_go || true
    check_java || true
    check_wasmtime || true
    echo ""

    if [ "$llvm_ver" -ge "$MIN_LLVM" ] 2>/dev/null && [ -f /usr/lib/x86_64-linux-musl/libc.a ]; then
        if [ "$INSTALL_WASM" = "1" ]; then
            install_wasmtime
        else
            echo "All prerequisites already installed."
        fi
        return 0
    fi

    echo "Installing missing prerequisites..."
    echo ""

    # Add LLVM apt repo if needed
    if [ "$llvm_ver" -lt "$MIN_LLVM" ] 2>/dev/null || [ "$llvm_ver" = "0" ]; then
        # Check if LLVM apt repo is already configured
        if ! grep -q "apt.llvm.org" /etc/apt/sources.list.d/*.list 2>/dev/null; then
            echo "Adding LLVM apt repository (apt.llvm.org)..."
            local llvm_keyring="/usr/share/keyrings/llvm-archive-keyring.gpg"
            wget -qO- https://apt.llvm.org/llvm-snapshot.gpg.key | gpg --dearmor -o "$llvm_keyring" 2>/dev/null
            echo "deb [signed-by=$llvm_keyring] http://apt.llvm.org/${codename}/ llvm-toolchain-${codename} main" \
                > /etc/apt/sources.list.d/llvm-toolchain.list
            apt-get update -qq
        fi

        # Find the newest LLVM version available in apt >= MIN_LLVM
        local install_ver=0
        for v in $(seq $MAX_LLVM -1 $MIN_LLVM); do
            if apt-cache show "llvm-$v" &>/dev/null; then
                install_ver=$v
                break
            fi
        done

        if [ "$install_ver" -ge "$MIN_LLVM" ] 2>/dev/null; then
            echo "Installing LLVM $install_ver (opt, llc, lld)..."
            apt-get install -y "llvm-$install_ver" "lld-$install_ver"
        else
            echo "ERROR: No LLVM >= $MIN_LLVM found in apt repositories."
            echo "  Try: wget https://apt.llvm.org/llvm.sh && sudo bash llvm.sh $MIN_LLVM"
            return 1
        fi
    fi

    # Install musl-dev
    if [ ! -f /usr/lib/x86_64-linux-musl/libc.a ]; then
        echo "Installing musl-dev..."
        apt-get install -y musl-dev
    fi

    echo ""
    echo "Verifying installation..."
    llvm_ver=$(find_llvm_version)
    echo "  LLVM: $llvm_ver (opt-$llvm_ver, llc-$llvm_ver)"
    if has_cmd "ld.lld-$llvm_ver"; then
        echo "  lld:  ld.lld-$llvm_ver"
    elif has_cmd "ld.lld"; then
        echo "  lld:  ld.lld"
    fi
    if [ -f /usr/lib/x86_64-linux-musl/libc.a ]; then
        echo "  musl: /usr/lib/x86_64-linux-musl/libc.a"
    fi
    # Install wasmtime if --wasm flag
    if [ "$INSTALL_WASM" = "1" ]; then
        install_wasmtime
    fi

    echo ""
    echo "Done. Run './build' to build."
}

# --- macOS ---

install_macos() {
    echo "Platform: macOS ($(sw_vers -productVersion 2>/dev/null || echo 'unknown'))"
    echo ""

    echo "Checking existing tools..."

    # Check Homebrew LLVM
    local brew_llvm=""
    for prefix in /opt/homebrew/opt/llvm/bin /usr/local/opt/llvm/bin; do
        if [ -x "$prefix/opt" ]; then
            brew_llvm="$prefix"
            break
        fi
    done

    if [ -n "$brew_llvm" ]; then
        local ver
        ver=$("$brew_llvm/opt" --version 2>&1 | grep -oE 'LLVM version [0-9]+' | grep -oE '[0-9]+' || echo 0)
        echo "  LLVM (Homebrew): $ver ($brew_llvm/)"
    else
        echo "  LLVM (Homebrew): NOT FOUND"
    fi

    # Check Homebrew LLD (ld64.lld — required for linking LLVM bitcode on macOS)
    local brew_lld=""
    for prefix in /opt/homebrew/opt/lld/bin /usr/local/opt/lld/bin /opt/homebrew/opt/llvm/bin /usr/local/opt/llvm/bin; do
        if [ -x "$prefix/ld64.lld" ]; then
            brew_lld="$prefix"
            break
        fi
    done

    if [ -n "$brew_lld" ]; then
        echo "  LLD (Homebrew): $brew_lld/ld64.lld"
    else
        echo "  LLD (Homebrew): NOT FOUND"
    fi

    # Check Xcode CLT
    if xcode-select -p &>/dev/null; then
        echo "  Xcode CLT: $(xcode-select -p)"
    else
        echo "  Xcode CLT: NOT FOUND"
    fi

    check_go || true
    check_java || true
    check_wasmtime || true
    echo ""

    # Install or upgrade Homebrew LLVM if needed
    if [ -z "$brew_llvm" ]; then
        if ! has_cmd brew; then
            echo "ERROR: Homebrew not found. Install from https://brew.sh"
            return 1
        fi
        echo "Installing LLVM via Homebrew..."
        brew install llvm
    elif [ "$ver" -lt "$MIN_LLVM" ] 2>/dev/null; then
        echo "LLVM $ver is too old (need $MIN_LLVM+), upgrading..."
        brew upgrade llvm
    fi

    # Install Homebrew LLD if needed (system ld can't process LLVM 22+ bitcode)
    if [ -z "$brew_lld" ]; then
        if ! has_cmd brew; then
            echo "ERROR: Homebrew not found. Install from https://brew.sh"
            return 1
        fi
        echo "Installing LLD via Homebrew..."
        brew install lld
    fi

    # Install Xcode CLT if needed
    if ! xcode-select -p &>/dev/null; then
        echo "Installing Xcode CommandLineTools..."
        xcode-select --install
        echo "  (follow the GUI prompt, then re-run this script)"
        return 1
    fi

    # Install wasmtime if --wasm flag
    if [ "$INSTALL_WASM" = "1" ]; then
        install_wasmtime
    fi

    echo ""
    echo "Done. Run './build' to build."
}

# --- Main ---

echo "=== Promise Compiler Prerequisites ==="
echo ""

# On Linux, non-root with --wasm: just install wasmtime (no apt-get needed)
if [ "$OS" = "Linux" ] && [ "$(id -u)" -ne 0 ] && [ "$INSTALL_WASM" = "1" ]; then
    install_wasmtime
    exit 0
fi

# Linux package installation requires root
if [ "$OS" = "Linux" ] && [ "$(id -u)" -ne 0 ]; then
    echo "This script requires root privileges on Linux (for apt-get)."
    echo ""
    echo "  sudo bin/install-prereqs.sh"
    echo ""
    echo "To install only wasmtime (no root needed):"
    echo ""
    echo "  bin/install-prereqs.sh --wasm"
    echo ""
    exit 1
fi

case "$OS" in
    Linux)  install_linux ;;
    Darwin) install_macos ;;
    *)
        echo "Unsupported platform: $OS"
        echo "Promise currently supports Linux and macOS."
        exit 1
        ;;
esac

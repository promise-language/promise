#!/bin/sh
# Promise language installer — for end users downloading a release binary.
#
# Remote install (latest stable):
#   curl -sSf https://promise-lang.org/install.sh | sh
#
# Remote install (pinned epoch):
#   curl -sSf https://promise-lang.org/install.sh | sh -s -- --epoch 2026.0
#
# Local install (from this repo, after building locally):
#   bin/install.sh
#
# This script downloads the pre-built Promise binary for your platform,
# verifies its checksum, and runs `promise install` which sets up ~/.promise/.
# The binary is fully self-contained: compiler + stdlib + LLVM tools embedded.

set -eu

GITHUB_REPO="promise-language/promise"
PROMISE_HOME="${PROMISE_HOME:-$HOME/.promise}"

# ── argument parsing ────────────────────────────────────────────────────────

EPOCH="latest"
# VARIANT selects the asset suffix: "" = thin (default), "-full" = host workflow
# pre-staged (offline), "-all" = every target's blobs (deferred, T0774).
VARIANT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --epoch)  EPOCH="$2"; shift 2 ;;
    --epoch=*) EPOCH="${1#--epoch=}"; shift ;;
    --full)   VARIANT="-full"; shift ;;
    --all)    VARIANT="-all"; shift ;;
    -h|--help)
      echo "Usage: install.sh [--epoch EPOCH] [--full | --all]"
      echo "  --epoch EPOCH   Install a specific epoch (default: latest stable)"
      echo "  --full          Install the full variant (host toolchain pre-staged; offline)"
      echo "  --all           Install the all variant (every target pre-staged; deferred)"
      exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [ "$VARIANT" = "-all" ]; then
  echo "note: the 'all' variant is deferred — no cross-target blobs exist yet (T0774);" >&2
  echo "      requesting it anyway in case this release provides it." >&2
fi

# ── platform detection ──────────────────────────────────────────────────────

OS=$(uname -s)
ARCH=$(uname -m)

case "$OS" in
  Linux)  PLATFORM="linux" ;;
  Darwin) PLATFORM="darwin" ;;
  *)
    echo "error: unsupported OS: $OS" >&2
    echo "  Windows support is planned but not yet available." >&2
    echo "  Windows users can use WSL2 to run the Linux binary." >&2
    exit 1 ;;
esac

case "$ARCH" in
  x86_64|amd64)    ARCH="amd64" ;;
  arm64|aarch64)   ARCH="arm64" ;;
  *)
    echo "error: unsupported architecture: $ARCH" >&2
    exit 1 ;;
esac

# Asset naming: promise-<os>-<arch>[-<variant>]; bare = thin.
BINARY_NAME="promise-${PLATFORM}-${ARCH}${VARIANT}"

# ── resolve release tag ─────────────────────────────────────────────────────

resolve_latest() {
  API_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
  if command -v curl >/dev/null 2>&1; then
    curl -sSf "$API_URL"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$API_URL"
  else
    echo "error: curl or wget is required" >&2
    exit 1
  fi | grep '"tag_name"' | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/'
}

if [ "$EPOCH" = "latest" ]; then
  echo "Fetching latest release..."
  TAG=$(resolve_latest)
  if [ -z "$TAG" ]; then
    echo "error: could not determine latest release from GitHub API" >&2
    exit 1
  fi
else
  TAG="epoch-${EPOCH}"
fi

echo "Installing Promise ${TAG} (${PLATFORM}-${ARCH})..."

BASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${TAG}"
DOWNLOAD_URL="${BASE_URL}/${BINARY_NAME}"
SUMS_URL="${BASE_URL}/SHA256SUMS"

# ── download ────────────────────────────────────────────────────────────────

TMP_BIN=$(mktemp)
TMP_SUMS=$(mktemp)
# Ensure cleanup on exit (including error exit)
# shellcheck disable=SC2064
trap "rm -f '$TMP_BIN' '$TMP_SUMS'" EXIT

download() {
  url="$1"; dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -sSfL "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
  else
    echo "error: curl or wget is required" >&2
    exit 1
  fi
}

echo "Downloading ${BINARY_NAME}..."
download "$DOWNLOAD_URL" "$TMP_BIN"

echo "Downloading SHA256SUMS..."
download "$SUMS_URL" "$TMP_SUMS"

# ── checksum verification ───────────────────────────────────────────────────

EXPECTED=$(grep "${BINARY_NAME}" "$TMP_SUMS" | awk '{print $1}')
if [ -z "$EXPECTED" ]; then
  echo "error: ${BINARY_NAME} not found in SHA256SUMS" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMP_BIN" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL=$(shasum -a 256 "$TMP_BIN" | awk '{print $1}')
else
  echo "warning: no sha256 tool found, skipping checksum verification" >&2
  ACTUAL="$EXPECTED"
fi

if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "error: checksum mismatch" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  actual:   $ACTUAL" >&2
  exit 1
fi

echo "Checksum verified."

# ── install ─────────────────────────────────────────────────────────────────

chmod +x "$TMP_BIN"

# promise install copies itself to ~/.promise/bin/promise, extracts stdlib,
# musl CRT (Linux), and LLVM tools. All embedded in the binary.
"$TMP_BIN" install

# ── PATH reminder ────────────────────────────────────────────────────────────

PROMISE_BIN="${PROMISE_HOME}/bin"

# Check if already on PATH
case ":${PATH}:" in
  *":${PROMISE_BIN}:"*) ALREADY_ON_PATH=1 ;;
  *) ALREADY_ON_PATH=0 ;;
esac

if [ "$ALREADY_ON_PATH" = "0" ]; then
  echo ""
  echo "Add Promise to your PATH. For bash:"
  echo "  echo 'export PATH=\"\$HOME/.promise/bin:\$PATH\"' >> ~/.bashrc && source ~/.bashrc"
  echo "For zsh:"
  echo "  echo 'export PATH=\"\$HOME/.promise/bin:\$PATH\"' >> ~/.zshrc && source ~/.zshrc"
fi

echo ""
echo "Run 'promise version' to verify."

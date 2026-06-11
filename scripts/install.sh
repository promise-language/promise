#!/bin/sh
# Promise language installer - for end users downloading a release binary.
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

# -- argument parsing --------------------------------------------------------

EPOCH="latest"
# VARIANT selects the asset suffix: "" = thin (default), "-full" = host workflow
# pre-staged (offline), "-all" = every target's blobs (deferred, T0774).
VARIANT=""
# THIN_INSTALL=1 installs only the compiler and skips the install-time toolchain
# pre-fetch (passed through to `promise install --no-fetch-toolchain`). The
# toolchain then downloads lazily on the first compile. No effect with --full
# (that binary already carries the toolchain).
THIN_INSTALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --epoch)  EPOCH="$2"; shift 2 ;;
    --epoch=*) EPOCH="${1#--epoch=}"; shift ;;
    --full)   VARIANT="-full"; shift ;;
    --all)    VARIANT="-all"; shift ;;
    --thin)   THIN_INSTALL=1; shift ;;
    -h|--help)
      echo "Usage: install.sh [--epoch EPOCH] [--full | --all] [--thin]"
      echo "  --epoch EPOCH   Install a specific epoch (default: latest stable)"
      echo "  --full          Install the full variant (host toolchain pre-staged; offline)"
      echo "  --all           Install the all variant (every target pre-staged; deferred)"
      echo "  --thin          Install only the compiler; download the toolchain on first build"
      exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [ "$VARIANT" = "-all" ]; then
  echo "note: the 'all' variant is deferred - no cross-target blobs exist yet (T0774);" >&2
  echo "      requesting it anyway in case this release provides it." >&2
fi

# -- platform detection ------------------------------------------------------

OS=$(uname -s)
ARCH=$(uname -m)

case "$OS" in
  Linux)  PLATFORM="linux" ;;
  Darwin) PLATFORM="darwin" ;;
  *)
    echo "error: unsupported OS: $OS" >&2
    echo "  On Windows, use the PowerShell installer instead:" >&2
    echo "    powershell -ExecutionPolicy Bypass -File install.ps1   (or install.cmd)" >&2
    echo "  Or use WSL2 to run the Linux binary." >&2
    exit 1 ;;
esac

case "$ARCH" in
  x86_64|amd64)    ARCH="amd64" ;;
  arm64|aarch64)   ARCH="arm64" ;;
  *)
    echo "error: unsupported architecture: $ARCH" >&2
    exit 1 ;;
esac

# Asset naming: promise-<os>-<arch>[-<variant>].gz; bare prefix = thin. Published
# assets are gzip-compressed (T0796) - no raw binary is uploaded. RUNTIME_NAME is
# the decompressed binary; ASSET_NAME is what we download and verify.
RUNTIME_NAME="promise-${PLATFORM}-${ARCH}${VARIANT}"
ASSET_NAME="${RUNTIME_NAME}.gz"

# -- resolve release tag -----------------------------------------------------

resolve_latest() {
  API_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
  # -f fails (empty body, nonzero exit) on any HTTP error; we deliberately drop
  # curl's -S so it does NOT print "curl: (22) ... 404" - an empty TAG is handled
  # by the generic "not launched yet" message below, which covers a 404, a network
  # error, or an empty body uniformly (T0804). Same intent for wget's -q.
  if command -v curl >/dev/null 2>&1; then
    curl -sf "$API_URL"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$API_URL"
  else
    echo "error: curl or wget is required" >&2
    exit 1
  fi | grep '"tag_name"' | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/'
}

# T0804: remove this PROMISE_BASE_URL override when the repo goes public.
# When PROMISE_BASE_URL is set, download the assets directly from that base URL
# (skipping GitHub "latest" release resolution). Used by the install gate (T0803)
# to point at the prebuilts dist bucket while the repo is still private.
if [ -n "${PROMISE_BASE_URL:-}" ]; then
  BASE_URL="${PROMISE_BASE_URL%/}"
  if [ "$EPOCH" != "latest" ]; then
    echo "note: --epoch is ignored under PROMISE_BASE_URL (the dist bucket is unversioned)" >&2
  fi
  echo "note: using PROMISE_BASE_URL override ($BASE_URL) - skipping GitHub release resolution (T0803/T0804)" >&2
  echo "Installing Promise (${PLATFORM}-${ARCH}) from ${BASE_URL}..."
else
  if [ "$EPOCH" = "latest" ]; then
    echo "Fetching latest release..."
    TAG=$(resolve_latest)
    if [ -z "$TAG" ]; then
      # T0804: remove this "not launched yet" messaging once the repo is public.
      # While https://github.com/promise-language/promise is private, the GitHub
      # "latest release" lookup fails (404, network error, or empty body), so a
      # missing TAG almost always means Promise has not launched publicly yet
      # rather than a problem on the user's machine. Keep the guidance generic -
      # any resolution failure (not just a GitHub 404) lands here.
      echo "error: could not find a published Promise release." >&2
      echo "" >&2
      echo "  You don't have access to" >&2
      echo "  https://github.com/promise-language/promise. The project is not" >&2
      echo "  live - nothing is wrong on your end. Please try again once the" >&2
      echo "  launch is announced." >&2
      exit 1
    fi
  else
    TAG="epoch-${EPOCH}"
  fi
  echo "Installing Promise ${TAG} (${PLATFORM}-${ARCH})..."
  BASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${TAG}"
fi

DOWNLOAD_URL="${BASE_URL}/${ASSET_NAME}"
SUMS_URL="${BASE_URL}/SHA256SUMS"

# -- download ----------------------------------------------------------------

TMP_GZ=$(mktemp)
TMP_BIN=$(mktemp)
TMP_SUMS=$(mktemp)
# Ensure cleanup on exit (including error exit)
# shellcheck disable=SC2064
trap "rm -f '$TMP_GZ' '$TMP_BIN' '$TMP_SUMS'" EXIT

download() {
  url="$1"; dest="$2"
  if command -v curl >/dev/null 2>&1; then
    # Quiet on HTTP errors (no -S): the caller inspects the status and prints a
    # tailored message, so curl's own "curl: (22) ... 404" would just be noise.
    curl -sfL "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
  else
    echo "error: curl or wget is required" >&2
    exit 1
  fi
}

# Like download() but renders a progress bar to stderr — the binary is ~60-70 MB
# and the wait is long enough that silence reads as a hang. Still quiet about the
# HTTP status itself (no curl -S) so a 404 is handled by the caller's messaging.
download_progress() {
  url="$1"; dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fL --progress-bar "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -q --show-progress -O "$dest" "$url"
  else
    echo "error: curl or wget is required" >&2
    exit 1
  fi
}

# Best-effort HTTP status for $1 on stdout (empty on a connection/DNS failure).
# Only called on a download failure, so the extra request is cheap (a 404 body
# is tiny). Follows redirects (-L) - GitHub asset URLs redirect to a CDN.
http_status() {
  url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl -sL -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true
  elif command -v wget >/dev/null 2>&1; then
    wget -S --spider -q "$url" 2>&1 | awk 'tolower($1) ~ /http\// {c=$2} END {print c}' || true
  fi
}

# Emitted when the platform asset is absent (HTTP 404). Promise ships prebuilt
# binaries for a fixed set of targets; any other platform - notably Intel macOS,
# which reports x86_64 - has no asset and 404s. Give a precise, non-scary reason.
no_prebuilt_for_platform() {
  echo "error: no prebuilt Promise binary is available for your platform (${PLATFORM}-${ARCH})." >&2
  echo "" >&2
  if [ "$PLATFORM" = "darwin" ] && [ "$ARCH" = "amd64" ]; then
    if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = "1" ]; then
      # Apple Silicon Mac, but this shell runs under Rosetta (x86_64 emulation),
      # so uname reported x86_64 and we asked for the Intel asset. The arm64
      # build exists - they just need to run from a native shell.
      echo "  You're on an Apple Silicon Mac, but this terminal is running under" >&2
      echo "  Rosetta (x86_64 emulation), so the installer asked for the Intel build" >&2
      echo "  - which doesn't exist. The Apple Silicon build is available; just run" >&2
      echo "  the installer from a native arm64 shell:" >&2
      echo "" >&2
      echo "    arch -arm64 /bin/zsh        # then re-run the install command" >&2
    else
      echo "  Promise provides macOS binaries for Apple Silicon (arm64) only." >&2
      echo "  Intel Macs (x86_64) are not supported." >&2
    fi
  fi
  echo "" >&2
  echo "  Supported platforms: macOS (Apple Silicon / arm64), Linux (x86_64), Windows (x86_64)." >&2
  exit 1
}

# Size hint depends on the variant: the default (thin) binary is ~13-20 MB; the
# -full binary embeds the LLVM toolchain (~60-70 MB).
case "$VARIANT" in
  "")      echo "Downloading ${ASSET_NAME} (~20 MB)..." ;;
  "-full") echo "Downloading ${ASSET_NAME} (~60-70 MB; this can take a minute)..." ;;
  *)       echo "Downloading ${ASSET_NAME}..." ;;
esac
if ! download_progress "$DOWNLOAD_URL" "$TMP_GZ"; then
  STATUS=$(http_status "$DOWNLOAD_URL")
  if [ "$STATUS" = "404" ]; then
    no_prebuilt_for_platform
  fi
  echo "error: failed to download ${ASSET_NAME} from ${DOWNLOAD_URL}" >&2
  echo "  HTTP status: ${STATUS:-unknown (network error?)}" >&2
  echo "  Check your network connection and try again." >&2
  exit 1
fi

echo "Downloading SHA256SUMS..."
download "$SUMS_URL" "$TMP_SUMS"

# -- checksum verification ---------------------------------------------------

# Match the filename field EXACTLY ($2): SHA256SUMS lists the thin
# (promise-linux-amd64.gz) and full (promise-linux-amd64-full.gz) assets, so
# a substring/prefix grep on the thin name would also match the full line
# and yield two hashes (-> a guaranteed checksum "mismatch"). SHA256SUMS is
# computed over the .gz asset (what's downloaded) - verify before decompressing.
EXPECTED=$(awk -v name="$ASSET_NAME" '$2 == name { print $1 }' "$TMP_SUMS")
if [ -z "$EXPECTED" ]; then
  echo "error: ${ASSET_NAME} not found in SHA256SUMS" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMP_GZ" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL=$(shasum -a 256 "$TMP_GZ" | awk '{print $1}')
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

echo "Checksum verified. Decompressing..."

# -- decompress --------------------------------------------------------------

# gunzip ships on every POSIX system (Linux/macOS/BSD), so no fallback path.
gunzip -c "$TMP_GZ" > "$TMP_BIN"

# -- install -----------------------------------------------------------------

chmod +x "$TMP_BIN"

# promise install copies itself to ~/.promise/bin/promise, extracts stdlib and
# musl CRT (Linux), and — unless --thin — pre-fetches the host LLVM toolchain so
# the first build is instant instead of blocking for minutes. A full binary
# stages its embedded toolchain regardless.
if [ "$THIN_INSTALL" = "1" ]; then
  "$TMP_BIN" install --no-fetch-toolchain
else
  "$TMP_BIN" install
fi

# -- PATH reminder ------------------------------------------------------------

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

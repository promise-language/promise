# Promise language installer (Windows / PowerShell) - for end users downloading a
# release binary. This is the real implementation; install.cmd is a thin shim that
# re-invokes it. Mirrors scripts/install.sh.
#
# Remote install (latest stable):
#   irm https://promise-lang.org/install.ps1 | iex
#
# Remote install (pinned epoch / full variant) - download then run with parameters:
#   $s = irm https://promise-lang.org/install.ps1
#   & ([scriptblock]::Create($s)) -Epoch 2026.0
#   & ([scriptblock]::Create($s)) -Full
#
# Downloads the pre-built Promise binary for your platform, verifies its checksum,
# and runs `promise install` which sets up %USERPROFILE%\.promise\. The binary is
# self-contained: compiler + stdlib + LLVM tools embedded.

[CmdletBinding()]
param(
    # Install a specific epoch (default: latest stable).
    [string]$Epoch = "latest",
    # Install the full variant (host toolchain pre-staged; offline).
    [switch]$Full,
    # Install the all variant (every target pre-staged; deferred).
    [switch]$All,
    # Install only the compiler; download the toolchain on first build (skips the
    # install-time pre-fetch). No effect with -Full (it carries the toolchain).
    [switch]$Thin,
    # Do not add %USERPROFILE%\.promise\bin to the User PATH. Equivalent to setting
    # $env:PROMISE_NO_MODIFY_PATH=1 before running. `promise install` owns PATH
    # setup (T0863), so this is forwarded to it as --no-modify-path.
    [switch]$NoModifyPath
)

$ErrorActionPreference = "Stop"

$GitHubRepo  = "promise-language/promise"

# VARIANT selects the asset suffix: "" = thin (default), "-full" = host workflow
# pre-staged (offline), "-all" = every target's blobs (deferred, T0774).
$Variant = ""
if ($Full) { $Variant = "-full" }
if ($All)  { $Variant = "-all" }

if ($Variant -eq "-all") {
    Write-Warning "the 'all' variant is deferred - no cross-target blobs exist yet (T0774);"
    Write-Warning "      requesting it anyway in case this release provides it."
}

# -- platform detection ------------------------------------------------------

switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { $Arch = "amd64" }
    "ARM64" { $Arch = "arm64" }
    default {
        Write-Error "unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
        exit 1
    }
}

# Asset naming: promise-windows-<arch>[-<variant>].exe.gz; bare prefix = thin.
# Published assets are gzip-compressed (T0796) - no raw binary is uploaded.
# $RuntimeName is the decompressed .exe; $AssetName is what we download/verify.
$RuntimeName = "promise-windows-${Arch}${Variant}.exe"
$AssetName   = "${RuntimeName}.gz"

# Shown when the platform asset is absent (HTTP 404). Promise publishes a Windows
# binary for x64 (amd64) only; ARM64 has no native build yet (Windows 11 on ARM
# transparently runs the x64 build under emulation). Give a precise reason rather
# than letting the raw 404 surface.
function Show-NoPrebuiltForPlatform {
    Write-Host "error: no prebuilt Promise binary is available for your platform (windows-${Arch})." -ForegroundColor Red
    Write-Host ""
    if ($Arch -eq "arm64") {
        Write-Host "  Promise provides a Windows binary for x64 (amd64) only - there is no"
        Write-Host "  native ARM64 build yet. Windows 11 on ARM transparently runs x64"
        Write-Host "  binaries under emulation; native ARM64 support is planned."
    }
    Write-Host ""
    Write-Host "  Supported platforms: Windows (x64), macOS (Apple Silicon), Linux (x64)."
    exit 1
}

# -- checksum helper (Constrained Language Mode safe) ------------------------

# Compute a file's SHA256 surviving Constrained Language Mode / stripped
# PowerShell where Get-FileHash is unavailable (T0501). Get-FileHash lives in
# Microsoft.PowerShell.Utility and is missing/blocked on locked-down corporate
# machines (WDAC/AppLocker CLM). certutil.exe is a native Win32 binary present
# on every Windows since Vista and runs regardless of language mode, so it is
# the fallback. Returns the hex digest, or $null when no method is available.
function Get-Sha256Hex {
    param([string]$Path)

    if (Get-Command Get-FileHash -ErrorAction SilentlyContinue) {
        return (Get-FileHash -Algorithm SHA256 -Path $Path).Hash
    }

    if (Get-Command certutil.exe -ErrorAction SilentlyContinue) {
        Write-Host "Get-FileHash unavailable (Constrained Language Mode?); verifying checksum with certutil."
        $out = & certutil.exe -hashfile $Path SHA256 2>$null
        if ($LASTEXITCODE -eq 0) {
            # certutil prints a header line, the hash, then a footer line; some
            # versions/locales space the hex bytes apart. Strip whitespace and
            # take the line that is a 64-char hex digest.
            foreach ($line in $out) {
                $h = ($line -replace '\s', '')
                if ($h -match '^[0-9a-fA-F]{64}$') { return $h }
            }
        }
    }

    return $null
}

# -- resolve release tag -----------------------------------------------------

# T0804: remove this PROMISE_BASE_URL override when the repo goes public.
# When PROMISE_BASE_URL is set, download the assets directly from that base URL
# (skipping GitHub "latest" release resolution). Used by the install gate (T0803)
# to point at the prebuilts dist bucket while the repo is still private.
if ($env:PROMISE_BASE_URL) {
    $BaseUrl = $env:PROMISE_BASE_URL.TrimEnd('/')
    if ($Epoch -ne "latest") {
        Write-Warning "-Epoch is ignored under PROMISE_BASE_URL (the dist bucket is unversioned)"
    }
    Write-Host "note: using PROMISE_BASE_URL override ($BaseUrl) - skipping GitHub release resolution (T0803/T0804)"
    Write-Host "Installing Promise (windows-${Arch}) from ${BaseUrl}..."
} else {
    if ($Epoch -eq "latest") {
        Write-Host "Fetching latest release..."
        $api = "https://api.github.com/repos/${GitHubRepo}/releases/latest"
        try {
            $release = Invoke-RestMethod -Uri $api -Headers @{ "User-Agent" = "promise-install" }
            $Tag = $release.tag_name
        } catch {
            $Tag = $null
        }
        if (-not $Tag) {
            # T0804: remove this "not launched yet" messaging once the repo is public.
            # While https://github.com/promise-language/promise is private, the GitHub
            # "latest release" lookup fails (404, network error, or empty body), so a
            # missing tag almost always means Promise has not launched publicly yet
            # rather than a problem on the user's machine. Keep the guidance generic -
            # any resolution failure (not just a GitHub 404) lands here.
            Write-Host "error: could not find a published Promise release." -ForegroundColor Red
            Write-Host ""
            Write-Host "  You don't have access to"
            Write-Host "  https://github.com/promise-language/promise. The project is not"
            Write-Host "  live - nothing is wrong on your end. Please try again once the"
            Write-Host "  launch is announced."
            exit 1
        }
    } else {
        $Tag = "epoch-${Epoch}"
    }
    Write-Host "Installing Promise ${Tag} (windows-${Arch})..."
    $BaseUrl = "https://github.com/${GitHubRepo}/releases/download/${Tag}"
}

$DownloadUrl = "${BaseUrl}/${AssetName}"
$SumsUrl     = "${BaseUrl}/SHA256SUMS"

# -- download ----------------------------------------------------------------

$TmpGz   = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".gz")
$TmpBin  = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".exe")
$TmpSums = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".sums")

try {
    # Size hint depends on the variant: the default (thin) binary is ~13-20 MB;
    # the -full binary embeds the LLVM toolchain (~60-70 MB).
    switch ($Variant) {
        ""      { Write-Host "Downloading ${AssetName} (~20 MB)..." }
        "-full" { Write-Host "Downloading ${AssetName} (~60-70 MB; this can take a minute)..." }
        default { Write-Host "Downloading ${AssetName}..." }
    }
    try {
        Invoke-WebRequest -Uri $DownloadUrl -OutFile $TmpGz
    } catch {
        # A 404 means the asset for this platform isn't published (e.g. ARM64
        # Windows) - report it meaningfully instead of a raw web exception.
        $code = $null
        try { if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode } } catch { }
        if ($code -eq 404) { Show-NoPrebuiltForPlatform }
        throw
    }

    Write-Host "Downloading SHA256SUMS..."
    Invoke-WebRequest -Uri $SumsUrl -OutFile $TmpSums

    # -- checksum verification -----------------------------------------------

    # Match the filename field EXACTLY (last whitespace-delimited token): SHA256SUMS
    # lists both the thin (promise-windows-amd64.exe.gz) and full
    # (promise-windows-amd64-full.exe.gz) assets - an exact compare avoids any
    # substring overlap between the two names (mirrors install.sh's awk $2 match).
    # SHA256SUMS is computed over the .gz asset (what's downloaded) - verify
    # before decompressing.
    $sumLine = Get-Content $TmpSums | Where-Object { ($_ -split '\s+')[-1] -eq $AssetName } | Select-Object -First 1
    if (-not $sumLine) {
        Write-Error "${AssetName} not found in SHA256SUMS"
        exit 1
    }
    $Expected = ($sumLine -split '\s+')[0]
    $Actual   = Get-Sha256Hex $TmpGz

    if (-not $Actual) {
        Write-Error @"
cannot verify the download checksum: neither Get-FileHash nor certutil is
available in this PowerShell session (often Constrained Language Mode on
locked-down/corporate machines). Refusing to install an unverified binary.
Re-run from a normal (Full Language Mode) PowerShell session, or download and
verify manually:
  binary: $DownloadUrl
  sums:   $SumsUrl
"@
        exit 1
    }

    if ($Expected -ne $Actual) {
        Write-Error "checksum mismatch`n  expected: $Expected`n  actual:   $Actual"
        exit 1
    }
    Write-Host "Checksum verified. Decompressing..."

    # -- decompress ----------------------------------------------------------

    # GzipStream is built into .NET - no external gzip CLI required.
    $in  = [System.IO.File]::OpenRead($TmpGz)
    $gz  = New-Object System.IO.Compression.GzipStream($in, [System.IO.Compression.CompressionMode]::Decompress)
    $out = [System.IO.File]::Create($TmpBin)
    try { $gz.CopyTo($out) } finally { $out.Dispose(); $gz.Dispose(); $in.Dispose() }

    # -- install -------------------------------------------------------------

    # promise install copies itself to %USERPROFILE%\.promise\bin\promise.exe,
    # extracts stdlib, and — unless -Thin — pre-fetches the host LLVM toolchain so
    # the first build is instant instead of blocking for minutes.
    $installArgs = @("install")
    if ($Thin)         { $installArgs += "--no-fetch-toolchain" }
    if ($NoModifyPath) { $installArgs += "--no-modify-path" }
    & $TmpBin @installArgs
    if ($LASTEXITCODE -ne 0) {
        Write-Error "promise install failed (exit code $LASTEXITCODE)"
        exit $LASTEXITCODE
    }
} finally {
    Remove-Item -Force -ErrorAction SilentlyContinue $TmpGz, $TmpBin, $TmpSums
}

# -- PATH setup ---------------------------------------------------------------
# `promise install` adds %USERPROFILE%\.promise\bin to the User PATH itself
# (idempotently, via the registry) and prints what it did. We do NOT set PATH here
# too: doing it in two places duplicated entries and defeated the -NoModifyPath /
# PROMISE_NO_MODIFY_PATH opt-out (T0863).

Write-Host ""
Write-Host "Run 'promise version' to verify."

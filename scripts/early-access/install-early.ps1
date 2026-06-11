# Promise language installer (EARLY ACCESS, Windows / PowerShell) - for end users
# while the GitHub repo is still private (T0804). This is the TEMPORARY companion
# to install.ps1: the install flow is identical, but it skips GitHub "latest"
# release resolution and fetches the pre-built assets straight from the public
# prebuilts dist bucket (https://prebuilts.promise-lang.org/dist, which is
# public-read). No PROMISE_BASE_URL needed - the bucket is baked in.
#
# Delete this script once the repo goes public and install.ps1 resolves "latest"
# anonymously from GitHub releases (T0804). Mirrors scripts/early-access/install-early.sh.
#
# Remote install (early access):
#   irm https://promise-lang.org/install-early.ps1 | iex
#
# Remote install (full variant) - download then run with parameters:
#   $s = irm https://promise-lang.org/install-early.ps1
#   & ([scriptblock]::Create($s)) -Full
#
# Downloads the pre-built Promise binary for your platform, verifies its checksum,
# and runs `promise install` which sets up %USERPROFILE%\.promise\. The binary is
# self-contained: compiler + stdlib + LLVM tools embedded.

[CmdletBinding()]
param(
    # Install the full variant (host toolchain pre-staged; offline).
    [switch]$Full,
    # Install the all variant (every target pre-staged; deferred).
    [switch]$All
)

$ErrorActionPreference = "Stop"

$PromiseHome = if ($env:PROMISE_HOME) { $env:PROMISE_HOME } else { Join-Path $env:USERPROFILE ".promise" }

# Early access always pulls from the public prebuilts dist bucket - no GitHub
# release resolution, no PROMISE_BASE_URL override (T0803/T0804). The dist bucket
# is unversioned, so there is no -Epoch parameter (unlike install.ps1).
$BaseUrl = "https://prebuilts.promise-lang.org/dist"

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

# Shown when the platform asset is absent (HTTP 404). Early access publishes a
# Windows binary for x64 (amd64) only; ARM64 has no native build yet (Windows 11
# on ARM transparently runs the x64 build under emulation). Give a precise reason
# rather than letting the raw 404 surface.
function Show-NoPrebuiltForPlatform {
    Write-Host "error: no prebuilt Promise binary is available for your platform (windows-${Arch})." -ForegroundColor Red
    Write-Host ""
    if ($Arch -eq "arm64") {
        Write-Host "  Promise early access provides a Windows binary for x64 (amd64) only -"
        Write-Host "  there is no native ARM64 build yet. Windows 11 on ARM transparently"
        Write-Host "  runs x64 binaries under emulation; native ARM64 support is planned."
    }
    Write-Host ""
    Write-Host "  Supported platforms: Windows (x64), macOS (Apple Silicon), Linux (x64)."
    Write-Host "  Questions? email early@promise-lang.org"
    exit 1
}

Write-Host "Installing Promise (early access, windows-${Arch}) from ${BaseUrl}..."

$DownloadUrl = "${BaseUrl}/${AssetName}"
$SumsUrl     = "${BaseUrl}/SHA256SUMS"

# -- download ----------------------------------------------------------------

$TmpGz   = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".gz")
$TmpBin  = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".exe")
$TmpSums = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".sums")

try {
    Write-Host "Downloading ${AssetName}..."
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
    # substring overlap between the two names (mirrors install-early.sh's awk $2 match).
    # SHA256SUMS is computed over the .gz asset (what's downloaded) - verify
    # before decompressing.
    $sumLine = Get-Content $TmpSums | Where-Object { ($_ -split '\s+')[-1] -eq $AssetName } | Select-Object -First 1
    if (-not $sumLine) {
        Write-Error "${AssetName} not found in SHA256SUMS"
        exit 1
    }
    $Expected = ($sumLine -split '\s+')[0]
    $Actual   = (Get-FileHash -Algorithm SHA256 -Path $TmpGz).Hash

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
    # extracts stdlib and LLVM tools. All embedded in the binary.
    & $TmpBin install
    if ($LASTEXITCODE -ne 0) {
        Write-Error "promise install failed (exit code $LASTEXITCODE)"
        exit $LASTEXITCODE
    }
} finally {
    Remove-Item -Force -ErrorAction SilentlyContinue $TmpGz, $TmpBin, $TmpSums
}

# -- PATH setup (User scope) --------------------------------------------------

$PromiseBin = Join-Path $PromiseHome "bin"
$UserPath   = [Environment]::GetEnvironmentVariable("Path", "User")
$onPath     = $false
if ($UserPath) {
    foreach ($p in $UserPath -split ';') {
        if ($p.TrimEnd('\') -ieq $PromiseBin.TrimEnd('\')) { $onPath = $true; break }
    }
}

if (-not $onPath) {
    $newPath = if ([string]::IsNullOrEmpty($UserPath)) { $PromiseBin } else { "$UserPath;$PromiseBin" }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host ""
    Write-Host "Added $PromiseBin to your User PATH. Open a new terminal for it to take effect."
}

Write-Host ""
Write-Host "Run 'promise version' to verify."

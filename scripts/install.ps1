# Promise language installer (Windows / PowerShell) — for end users downloading a
# release binary. This is the real implementation; install.cmd is a thin shim that
# re-invokes it. Mirrors scripts/install.sh.
#
# Remote install (latest stable):
#   irm https://github.com/promise-language/promise/releases/latest/download/install.ps1 | iex
#
# Remote install (pinned epoch / full variant) — download then run with parameters:
#   $s = irm https://github.com/promise-language/promise/releases/latest/download/install.ps1
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
    [switch]$All
)

$ErrorActionPreference = "Stop"

$GitHubRepo  = "promise-language/promise"
$PromiseHome = if ($env:PROMISE_HOME) { $env:PROMISE_HOME } else { Join-Path $env:USERPROFILE ".promise" }

# VARIANT selects the asset suffix: "" = thin (default), "-full" = host workflow
# pre-staged (offline), "-all" = every target's blobs (deferred, T0774).
$Variant = ""
if ($Full) { $Variant = "-full" }
if ($All)  { $Variant = "-all" }

if ($Variant -eq "-all") {
    Write-Warning "the 'all' variant is deferred — no cross-target blobs exist yet (T0774);"
    Write-Warning "      requesting it anyway in case this release provides it."
}

# ── platform detection ──────────────────────────────────────────────────────

switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { $Arch = "amd64" }
    "ARM64" { $Arch = "arm64" }
    default {
        Write-Error "unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
        exit 1
    }
}

# Asset naming: promise-windows-<arch>[-<variant>].exe; bare = thin.
$BinaryName = "promise-windows-${Arch}${Variant}.exe"

# ── resolve release tag ─────────────────────────────────────────────────────

if ($Epoch -eq "latest") {
    Write-Host "Fetching latest release..."
    $api = "https://api.github.com/repos/${GitHubRepo}/releases/latest"
    try {
        $release = Invoke-RestMethod -Uri $api -Headers @{ "User-Agent" = "promise-install" }
        $Tag = $release.tag_name
    } catch {
        Write-Error "could not determine latest release from GitHub API: $_"
        exit 1
    }
    if (-not $Tag) {
        Write-Error "could not determine latest release from GitHub API"
        exit 1
    }
} else {
    $Tag = "epoch-${Epoch}"
}

Write-Host "Installing Promise ${Tag} (windows-${Arch})..."

$BaseUrl     = "https://github.com/${GitHubRepo}/releases/download/${Tag}"
$DownloadUrl = "${BaseUrl}/${BinaryName}"
$SumsUrl     = "${BaseUrl}/SHA256SUMS"

# ── download ────────────────────────────────────────────────────────────────

$TmpBin  = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".exe")
$TmpSums = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + ".sums")

try {
    Write-Host "Downloading ${BinaryName}..."
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $TmpBin

    Write-Host "Downloading SHA256SUMS..."
    Invoke-WebRequest -Uri $SumsUrl -OutFile $TmpSums

    # ── checksum verification ───────────────────────────────────────────────

    # Match the filename field EXACTLY (last whitespace-delimited token): SHA256SUMS
    # lists both the thin (promise-windows-amd64.exe) and full
    # (promise-windows-amd64-full.exe) binaries — an exact compare avoids any
    # substring overlap between the two names (mirrors install.sh's awk $2 match).
    $sumLine = Get-Content $TmpSums | Where-Object { ($_ -split '\s+')[-1] -eq $BinaryName } | Select-Object -First 1
    if (-not $sumLine) {
        Write-Error "${BinaryName} not found in SHA256SUMS"
        exit 1
    }
    $Expected = ($sumLine -split '\s+')[0]
    $Actual   = (Get-FileHash -Algorithm SHA256 -Path $TmpBin).Hash

    if ($Expected -ne $Actual) {
        Write-Error "checksum mismatch`n  expected: $Expected`n  actual:   $Actual"
        exit 1
    }
    Write-Host "Checksum verified."

    # ── install ─────────────────────────────────────────────────────────────

    # promise install copies itself to %USERPROFILE%\.promise\bin\promise.exe,
    # extracts stdlib and LLVM tools. All embedded in the binary.
    & $TmpBin install
    if ($LASTEXITCODE -ne 0) {
        Write-Error "promise install failed (exit code $LASTEXITCODE)"
        exit $LASTEXITCODE
    }
} finally {
    Remove-Item -Force -ErrorAction SilentlyContinue $TmpBin, $TmpSums
}

# ── PATH setup (User scope) ──────────────────────────────────────────────────

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

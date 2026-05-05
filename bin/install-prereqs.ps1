# Install prerequisites for building the Promise compiler on Windows.
# Run from an elevated (Administrator) PowerShell prompt:
#
#   powershell -ExecutionPolicy Bypass -File bin\install-prereqs.ps1
#
# Prerequisites:
#   - LLVM 22+ (opt, llc, lld-link) -- full clang+llvm from GitHub releases
#   - Visual Studio Build Tools or VS Community (MSVC libs + Windows SDK)
#   - Go 1.25+
#   - Java 11+ (for ANTLR parser generation -- optional if parser already generated)
#
# The standard LLVM Windows installer (Chocolatey, LLVM-*-win64.exe) only ships
# clang/lld/lldb. Promise requires opt and llc (LLVM backend tools) which are only
# available in the full clang+llvm tarball from GitHub releases.

param(
    [switch]$Force  # Re-install even if already present
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$MinLLVM = 22
$MaxLLVM = 25
$LLVMVersion = "22.1.0"
$LLVMInstallDir = "$env:ProgramFiles\LLVM"

# Architecture detection
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
    $LLVMArch = "aarch64"
} else {
    $LLVMArch = "x86_64"
}

$LLVMTarball = "clang+llvm-$LLVMVersion-$LLVMArch-pc-windows-msvc.tar.xz"
$LLVMUrl = "https://github.com/llvm/llvm-project/releases/download/llvmorg-$LLVMVersion/$LLVMTarball"

# ---- Helpers ----------------------------------------------------------------

function Write-Status($icon, $msg) { Write-Host "  $icon $msg" }
function Write-OK($msg)     { Write-Status "[OK]" $msg }
function Write-Missing($msg) { Write-Status "[--]" $msg }
function Write-Info($msg)    { Write-Status "    " $msg }

function Test-Admin {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Find-OnPath($name) {
    $result = Get-Command $name -ErrorAction SilentlyContinue
    if ($result) { return $result.Source }
    return $null
}

# ---- LLVM Detection ---------------------------------------------------------

function Find-LLVMVersion {
    # Check versioned names on PATH (newest first)
    for ($v = $MaxLLVM; $v -ge $MinLLVM; $v--) {
        if (Find-OnPath "opt-$v") { return $v }
    }
    # Check unversioned on PATH
    $opt = Find-OnPath "opt"
    if ($opt) {
        $verOutput = & $opt --version 2>&1 | Out-String
        if ($verOutput -match 'LLVM version (\d+)') {
            $ver = [int]$Matches[1]
            if ($ver -ge $MinLLVM) { return $ver }
        }
    }
    # Check the standard install location
    $llvmOpt = Join-Path $LLVMInstallDir "bin\opt.exe"
    if (Test-Path $llvmOpt) {
        $verOutput = & $llvmOpt --version 2>&1 | Out-String
        if ($verOutput -match 'LLVM version (\d+)') {
            $ver = [int]$Matches[1]
            if ($ver -ge $MinLLVM) { return $ver }
        }
    }
    return 0
}

function Find-LLVMTools {
    $tools = @{}
    foreach ($tool in @("opt", "llc", "lld-link")) {
        $found = $null
        # Versioned on PATH
        for ($v = $MaxLLVM; $v -ge $MinLLVM; $v--) {
            $found = Find-OnPath "$tool-$v"
            if ($found) { break }
        }
        # Unversioned on PATH
        if (-not $found) { $found = Find-OnPath $tool }
        # Standard install location
        if (-not $found) {
            $candidate = Join-Path $LLVMInstallDir "bin\$tool.exe"
            if (Test-Path $candidate) { $found = $candidate }
        }
        $tools[$tool] = $found
    }
    return $tools
}

# ---- Windows SDK / MSVC Detection -------------------------------------------

function Find-WindowsSDK {
    $arch = if ([Environment]::Is64BitOperatingSystem) { "x64" } else { "x86" }
    $result = @{ ucrt = $null; um = $null; msvc = $null }

    # Search Windows Kits
    $sdkRoot = "${env:ProgramFiles(x86)}\Windows Kits\10\Lib"
    if (-not $sdkRoot -or -not (Test-Path $sdkRoot)) {
        $sdkRoot = "C:\Program Files (x86)\Windows Kits\10\Lib"
    }
    if (Test-Path $sdkRoot) {
        $versions = Get-ChildItem $sdkRoot -Directory |
            Where-Object { $_.Name -match '^10\.' } |
            Sort-Object Name -Descending
        foreach ($ver in $versions) {
            $ucrt = Join-Path $ver.FullName "ucrt\$arch"
            $um   = Join-Path $ver.FullName "um\$arch"
            if (-not $result.ucrt -and (Test-Path (Join-Path $ucrt "libucrt.lib"))) {
                $result.ucrt = $ucrt
            }
            if (-not $result.um -and (Test-Path (Join-Path $um "kernel32.lib"))) {
                $result.um = $um
            }
            if ($result.ucrt -and $result.um) { break }
        }
    }

    # Search MSVC via vswhere
    $vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
    if (-not (Test-Path $vswhere)) {
        $vswhere = "C:\Program Files (x86)\Microsoft Visual Studio\Installer\vswhere.exe"
    }
    if (Test-Path $vswhere) {
        $vsPath = & $vswhere -latest -property installationPath 2>$null
        if ($vsPath) {
            $msvcRoot = Join-Path $vsPath "VC\Tools\MSVC"
            if (Test-Path $msvcRoot) {
                $versions = Get-ChildItem $msvcRoot -Directory | Sort-Object Name -Descending
                foreach ($ver in $versions) {
                    $dir = Join-Path $ver.FullName "lib\$arch"
                    if (Test-Path (Join-Path $dir "libcmt.lib")) {
                        $result.msvc = $dir
                        break
                    }
                }
            }
        }
    }

    # Fallback: probe common VS paths
    if (-not $result.msvc) {
        $programFiles = if ($env:ProgramFiles) { $env:ProgramFiles } else { "C:\Program Files" }
        foreach ($edition in @("BuildTools", "Community", "Professional", "Enterprise")) {
            $msvcRoot = Join-Path $programFiles "Microsoft Visual Studio\2022\$edition\VC\Tools\MSVC"
            if (Test-Path $msvcRoot) {
                $versions = Get-ChildItem $msvcRoot -Directory | Sort-Object Name -Descending
                foreach ($ver in $versions) {
                    $dir = Join-Path $ver.FullName "lib\$arch"
                    if (Test-Path (Join-Path $dir "libcmt.lib")) {
                        $result.msvc = $dir
                        break
                    }
                }
            }
            if ($result.msvc) { break }
        }
    }

    return $result
}

# ---- Go Detection -----------------------------------------------------------

function Find-Go {
    $go = Find-OnPath "go"
    if (-not $go) { return $null }
    $verOutput = & $go version 2>&1 | Out-String
    if ($verOutput -match 'go(\d+\.\d+)') {
        return @{ Path = $go; Version = $Matches[1] }
    }
    return @{ Path = $go; Version = "unknown" }
}

# ---- Java Detection ---------------------------------------------------------

function Find-Java {
    $java = Find-OnPath "java"
    if (-not $java) { return $null }
    $verOutput = & $java -version 2>&1 | Out-String
    return @{ Path = $java; VersionInfo = ($verOutput -split "`n")[0].Trim() }
}

# ---- LLVM Install -----------------------------------------------------------

function Install-LLVM {
    # Download the full clang+llvm tarball from GitHub releases.
    # The standard LLVM Windows installer (LLVM-*-win64.exe / Chocolatey) does NOT
    # include opt or llc. The full tarball does.

    $tempDir = Join-Path $env:TEMP "promise-llvm-install"
    if (Test-Path $tempDir) { Remove-Item $tempDir -Recurse -Force }
    New-Item -ItemType Directory -Path $tempDir | Out-Null

    $tarballPath = Join-Path $tempDir $LLVMTarball

    Write-Host "  Downloading $LLVMTarball..."
    Write-Host "  From: $LLVMUrl"
    Write-Host "  (this is ~700MB and may take a few minutes)"
    Write-Host ""

    # Use BITS for background download with progress, fall back to WebClient
    try {
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -Uri $LLVMUrl -OutFile $tarballPath -UseBasicParsing
    } catch {
        Write-Host "  Download failed: $_"
        Write-Host ""
        Write-Host "  You can manually download from:"
        Write-Host "    $LLVMUrl"
        Write-Host "  Extract to: $LLVMInstallDir"
        Write-Host "  Then add $LLVMInstallDir\bin to your PATH."
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        return $false
    }

    if (-not (Test-Path $tarballPath)) {
        Write-Host "  ERROR: Download failed -- file not found."
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        return $false
    }

    $fileSize = (Get-Item $tarballPath).Length / 1MB
    Write-Host "  Downloaded: $([math]::Round($fileSize, 1)) MB"

    # Extract .tar.xz -- requires tar (built into Windows 10+)
    Write-Host "  Extracting to $LLVMInstallDir..."
    Write-Host "  (extraction takes a few minutes -- the archive contains ~3GB)"

    $extractDir = Join-Path $tempDir "extracted"
    New-Item -ItemType Directory -Path $extractDir | Out-Null

    # Use Windows system tar.exe explicitly -- Git Bash tar misinterprets
    # drive letters (C:) as remote host prefixes.
    $winTar = Join-Path $env:SystemRoot "System32\tar.exe"
    if (-not (Test-Path $winTar)) {
        $winTar = "tar"  # fallback to PATH
    }

    try {
        & $winTar -xf $tarballPath -C $extractDir 2>&1
        if ($LASTEXITCODE -ne 0) {
            throw "tar extraction failed (exit code $LASTEXITCODE)"
        }
    } catch {
        Write-Host "  ERROR: Extraction failed: $_"
        Write-Host "  Ensure Windows tar.exe is available (built into Windows 10 1803+)."
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        return $false
    }

    # Find the extracted directory (named like clang+llvm-22.1.0-x86_64-pc-windows-msvc)
    $extractedDirs = @(Get-ChildItem $extractDir -Directory)
    if ($extractedDirs.Count -eq 0) {
        Write-Host "  ERROR: No directory found after extraction."
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        return $false
    }
    $srcDir = $extractedDirs[0].FullName

    # Verify opt.exe exists in the extracted tree
    if (-not (Test-Path (Join-Path $srcDir "bin\opt.exe"))) {
        Write-Host "  ERROR: opt.exe not found in extracted archive."
        Write-Host "  Contents of bin/:"
        Get-ChildItem (Join-Path $srcDir "bin") -Name | Select-Object -First 20 | ForEach-Object { Write-Host "    $_" }
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        return $false
    }

    # Remove existing LLVM install if present
    if (Test-Path $LLVMInstallDir) {
        Write-Host "  Removing existing $LLVMInstallDir..."
        Remove-Item $LLVMInstallDir -Recurse -Force
    }

    # Move extracted directory to install location
    Move-Item $srcDir $LLVMInstallDir

    # Add to system PATH if not already there
    $machinePath = [System.Environment]::GetEnvironmentVariable("Path", "Machine")
    $llvmBin = Join-Path $LLVMInstallDir "bin"
    if ($machinePath -notlike "*$llvmBin*") {
        Write-Host "  Adding $llvmBin to system PATH..."
        [System.Environment]::SetEnvironmentVariable("Path", "$machinePath;$llvmBin", "Machine")
    }

    # Refresh current process PATH
    $env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" +
                [System.Environment]::GetEnvironmentVariable("Path", "User")

    # Clean up
    Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue

    Write-Host "  LLVM $LLVMVersion installed to $LLVMInstallDir"
    Write-Host ""
    return $true
}

# ---- Main -------------------------------------------------------------------

Write-Host ""
Write-Host "=== Promise Compiler Prerequisites (Windows) ==="
Write-Host ""

# ---- 1. Check existing tools ------------------------------------------------

Write-Host "Checking existing tools..."
Write-Host ""

$needsInstall = $false

# Go
$goInfo = Find-Go
if ($goInfo) {
    Write-OK "Go: $($goInfo.Version) ($($goInfo.Path))"
} else {
    Write-Missing "Go: NOT FOUND -- install Go 1.25+ from https://go.dev/dl/"
}

# Java
$javaInfo = Find-Java
if ($javaInfo) {
    Write-OK "Java: $($javaInfo.VersionInfo)"
} else {
    Write-Missing "Java: NOT FOUND (optional -- only needed to regenerate ANTLR parser)"
}

# LLVM -- check for all three required tools (opt, llc, lld-link)
$llvmVer = Find-LLVMVersion
$llvmTools = Find-LLVMTools
$hasAllTools = $llvmTools["opt"] -and $llvmTools["llc"] -and $llvmTools["lld-link"]

if ($llvmVer -ge $MinLLVM -and $hasAllTools) {
    Write-OK "LLVM: $llvmVer"
    foreach ($tool in @("opt", "llc", "lld-link")) {
        Write-Info "$tool : $($llvmTools[$tool])"
    }
} else {
    if ($llvmVer -gt 0 -and -not $hasAllTools) {
        Write-Missing "LLVM: $llvmVer (incomplete -- missing opt/llc)"
        Write-Info "The standard LLVM installer does not include opt/llc."
        Write-Info "This script installs the full clang+llvm from GitHub releases."
    } elseif ($llvmVer -gt 0) {
        Write-Missing "LLVM: $llvmVer (need $MinLLVM+)"
    } else {
        Write-Missing "LLVM: NOT FOUND (need $MinLLVM+)"
    }
    $needsInstall = $true
}

# Windows SDK
$sdk = Find-WindowsSDK
if ($sdk.ucrt -and $sdk.um) {
    $sdkVer = if ($sdk.ucrt -match '\\(10\.\d+\.\d+\.\d+)\\') { $Matches[1] } else { "found" }
    Write-OK "Windows SDK: $sdkVer"
    Write-Info "ucrt: $($sdk.ucrt)"
    Write-Info "um  : $($sdk.um)"
} else {
    Write-Missing "Windows SDK: NOT FOUND"
    Write-Info "Install Visual Studio Build Tools with 'Desktop development with C++' workload"
    Write-Info "https://visualstudio.microsoft.com/downloads/"
}

# MSVC
if ($sdk.msvc) {
    $msvcVer = if ($sdk.msvc -match '\\(\d+\.\d+\.\d+)\\') { $Matches[1] } else { "found" }
    Write-OK "MSVC libs: $msvcVer"
    Write-Info "path: $($sdk.msvc)"
} else {
    Write-Missing "MSVC libs: NOT FOUND"
    Write-Info "Install Visual Studio Build Tools with 'Desktop development with C++' workload"
    Write-Info "https://visualstudio.microsoft.com/downloads/"
}

Write-Host ""

# ---- 2. Install missing prerequisites ---------------------------------------

if (-not $needsInstall -and -not $Force) {
    if (-not $sdk.ucrt -or -not $sdk.um -or -not $sdk.msvc) {
        Write-Host "LLVM is installed, but Windows SDK/MSVC libs are missing."
        Write-Host "Install Visual Studio Build Tools with 'Desktop development with C++' workload:"
        Write-Host "  https://visualstudio.microsoft.com/downloads/"
        Write-Host ""
        exit 1
    }
    Write-Host "All prerequisites are installed."
    Write-Host "Run .\build.bat or ./build (from Git Bash) to build."
    Write-Host ""
    exit 0
}

# Check admin for LLVM install (writes to Program Files and system PATH)
if ($needsInstall) {
    if (-not (Test-Admin)) {
        Write-Host "ERROR: Administrator privileges required to install LLVM."
        Write-Host ""
        Write-Host "  Right-click PowerShell -> 'Run as Administrator', then:"
        Write-Host "  powershell -ExecutionPolicy Bypass -File bin\install-prereqs.ps1"
        Write-Host ""
        exit 1
    }

    Write-Host "Installing LLVM $LLVMVersion (full clang+llvm with opt/llc/lld-link)..."
    Write-Host ""
    $ok = Install-LLVM
    if (-not $ok) {
        Write-Host "ERROR: LLVM installation failed."
        exit 1
    }
}

# ---- 3. Verify --------------------------------------------------------------

Write-Host "Verifying installation..."
Write-Host ""

$allOK = $true

$llvmVer = Find-LLVMVersion
if ($llvmVer -ge $MinLLVM) {
    Write-OK "LLVM: $llvmVer"
} else {
    Write-Missing "LLVM: not found or too old"
    Write-Info "Ensure $LLVMInstallDir\bin is on your PATH."
    $allOK = $false
}

$llvmTools = Find-LLVMTools
foreach ($tool in @("opt", "llc", "lld-link")) {
    if ($llvmTools[$tool]) {
        Write-OK "$tool : $($llvmTools[$tool])"
    } else {
        Write-Missing "$tool : NOT FOUND"
        $allOK = $false
    }
}

$sdk = Find-WindowsSDK
if ($sdk.ucrt -and $sdk.um -and $sdk.msvc) {
    Write-OK "Windows SDK + MSVC: found"
} else {
    Write-Missing "Windows SDK/MSVC: incomplete"
    Write-Info "Install Visual Studio Build Tools with 'Desktop development with C++'"
    $allOK = $false
}

Write-Host ""
if ($allOK) {
    Write-Host "Done. Run .\build.bat or ./build (from Git Bash) to build."
} else {
    Write-Host "Some prerequisites are still missing. See above for details."
    exit 1
}
Write-Host ""

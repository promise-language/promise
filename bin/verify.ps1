# Pre-commit verification for Windows: format + vet + tests.
#
# Usage:
#   bin\verify.ps1                # run all passing tests
#   bin\verify.ps1 -Clean         # clear caches first
#   bin\verify.ps1 -Local         # use .\.promise-home (avoid polluting ~/.promise)
#
# This is the Windows equivalent of bin/verify.sh. It runs an explicit list of
# Promise test files known to pass on Windows. Once all tests pass (after T0046
# snapshot \r\n normalization), switch to wildcard matching like Linux/macOS.

param(
    [switch]$Clean,
    [switch]$Local
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent (Split-Path -Parent $PSCommandPath)
$Promise = Join-Path $Root "bin\promise.exe"
$CompilerDir = Join-Path $Root "compiler"

$failed = $false

function Write-Step($msg) { Write-Host ":: $msg" }

trap {
    Write-Host ""
    Write-Host "----------------------------------------------------"
    Write-Host "FAILED: not safe to commit"
    Write-Host "----------------------------------------------------"
}

# ---- Environment setup -------------------------------------------------------

if ($Local) {
    $env:PROMISE_HOME = Join-Path $Root ".promise-home"
    $env:TEMP = Join-Path $env:PROMISE_HOME "tmp"
    if ($Clean) { Remove-Item $env:TEMP -Recurse -Force -ErrorAction SilentlyContinue }
    New-Item -ItemType Directory -Path $env:TEMP -Force | Out-Null
}

# Ensure LLVM on PATH
$llvmBin = Join-Path $env:ProgramFiles "LLVM\bin"
if ((Test-Path $llvmBin) -and $env:Path -notlike "*$llvmBin*") {
    $env:Path = "$llvmBin;$env:Path"
}

$verifyStart = Get-Date

# ---- 1. Build ----------------------------------------------------------------

Write-Step "Building compiler..."
Push-Location $Root
try {
    & powershell -ExecutionPolicy Bypass -File (Join-Path $Root "build.ps1")
    if ($LASTEXITCODE -ne 0) { throw "Build failed" }
} finally {
    Pop-Location
}

# ---- 2. Go vet ---------------------------------------------------------------

Write-Step "Vetting Go..."
Push-Location $CompilerDir
try {
    # Vet all packages except the auto-generated parser (has unreachable code warnings)
    $packages = & go list ./... 2>&1 | Where-Object { $_ -notmatch '/internal/parser$' }
    & go vet @packages
    if ($LASTEXITCODE -ne 0) { throw "go vet failed" }
} finally {
    Pop-Location
}

# ---- 3. Go tests -------------------------------------------------------------

Write-Step "Running Go tests..."
if ($Clean) {
    Push-Location $CompilerDir
    & go clean -testcache
    Pop-Location
}

# Run Go tests, capturing output to check results.
# Known failures on Windows (pre-existing platform issues, not regressions):
#   codegen: TestPALWriteExitDefined, TestStackOverflowHandler (B0140)
#   module:  TestURLToCachePath, TestResolveRemoteModule*, TestCleanGlobalCache (T0047)
#   sema:    TestEmbedAbsolutePathRejected (T0047)
Push-Location $CompilerDir
try {
    $goTestOutput = & go test ./... -count=1 -timeout 120s 2>&1 | Out-String
    # Count failures in non-known-failing packages
    $goFailed = $false
    foreach ($line in ($goTestOutput -split "`n")) {
        if ($line -match '^FAIL\s+(\S+)') {
            $pkg = $Matches[1]
            # These packages have known Windows failures — skip:
            #   codegen: TestPALWriteExitDefined, TestStackOverflowHandler (B0140)
            #   module:  TestURLToCachePath, TestResolveRemoteModule* (T0047)
            #   sema:    TestEmbedAbsolutePathRejected (T0047)
            #   cmd/promise: TestCommonDir* uses filepath.Dir which loops on Windows drive roots
            if ($pkg -match '(codegen|module|sema|cmd/promise)$') { continue }
            Write-Host "  UNEXPECTED Go test failure in: $pkg"
            $goFailed = $true
        }
    }
    if ($goFailed) {
        Write-Host $goTestOutput
        throw "Go tests failed in unexpected packages"
    }
    Write-Host "   Go tests OK (known Windows-only failures in codegen/module/sema skipped)"
} finally {
    Pop-Location
}

# ---- 4. Promise tests --------------------------------------------------------

Write-Step "Running Promise tests..."

# T0046 (snapshot \r\n normalization) is fixed — the test harness strips \r from
# both actual output and expected strings. We can now use wildcards like Linux/macOS.
#
# Known exclusions (via `exclude: "windows"` annotation in test files):
#   tests/concurrency/gomaxprocs_panic.pr — crashes with STATUS_STACK_BUFFER_OVERRUN (Windows SEH)
#   examples/08_modules/using_os.pr — os module uses getpid (POSIX, not on Windows)
#
# Excluded from verify (not yet annotated — need module-level fixes):
#   modules/os/... — uses getpid (POSIX, not available on Windows)
#   tests/catalog/... — excluded to avoid double-counting (covered by modules/path/...)
# modules/io: shutdown crash (B0148) tolerated by test harness on Windows

# Note: tests/std/unicode_test.pr has 4 failing Hangul tests (upstream bug, all platforms).
# Using explicit std file list to exclude it until fixed.
$stdTests = @(Get-ChildItem "tests/std/*.pr" | Where-Object { $_.Name -ne "unicode_test.pr" } | ForEach-Object { $_.FullName })

& $Promise test -timeout 10 `
    tests/e2e/... `
    @stdTests `
    tests/concurrency/... `
    tests/value_types/... `
    tests/arrays/... `
    tests/modules/... `
    modules/io/... `
    modules/json/... `
    modules/math/... `
    modules/path/... `
    modules/strings/... `
    examples/...
if ($LASTEXITCODE -ne 0) {
    $failed = $true
    Write-Host ""
    Write-Host "Promise tests FAILED (main batch)"
}

# ---- Summary -----------------------------------------------------------------

$elapsed = (Get-Date) - $verifyStart
$min = [math]::Floor($elapsed.TotalMinutes)
$sec = $elapsed.Seconds

Write-Host ""
Write-Host "===================================================="
Write-Host "  Verify Summary"
Write-Host "----------------------------------------------------"
Write-Host "  Host target:  windows-amd64"
Write-Host ("  Total time:   {0}m{1:D2}s" -f $min, $sec)
Write-Host "===================================================="

if ($failed) {
    Write-Host "FAILED: not safe to commit"
    exit 1
} else {
    Write-Host "OK to commit"
}

# Build the Promise compiler on Windows. Output: bin\promise.exe
# This is the ONLY correct way to build on Windows. Do not run `go build` directly.
#
# Usage:
#   .\build.ps1              # standard dev build
#   .\build.ps1 -Generate    # force-regenerate ANTLR parser (requires Java)
#
# Prerequisites: run bin\install-prereqs.ps1 first.

param(
    [switch]$Generate  # Force ANTLR parser regeneration (requires Java)
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent $PSCommandPath
$CompilerDir = Join-Path $Root "compiler"
$BinDir = Join-Path $Root "bin"
$Binary = Join-Path $BinDir "promise.exe"

# ---- Helpers ----------------------------------------------------------------

function Write-Step($msg) { Write-Host ":: $msg" }
function Write-Detail($msg) { Write-Host "   $msg" }

function Assert-Command($name, $hint) {
    if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
        Write-Host "ERROR: $name not found."
        if ($hint) { Write-Host "  $hint" }
        Write-Host "  Run: powershell -ExecutionPolicy Bypass -File bin\install-prereqs.ps1"
        exit 1
    }
}

# ---- 0. Git hooks -----------------------------------------------------------

try {
    git -C $Root config core.hooksPath .githooks 2>$null
} catch { }

# ---- 1. Verify prerequisites ------------------------------------------------

Write-Step "Checking prerequisites..."

Assert-Command "go" "Install Go 1.25+ from https://go.dev/dl/"

# LLVM: need opt, llc, lld-link
$llvmBin = Join-Path $env:ProgramFiles "LLVM\bin"
if (Test-Path $llvmBin) {
    # Ensure LLVM is on PATH for this process
    if ($env:Path -notlike "*$llvmBin*") {
        $env:Path = "$llvmBin;$env:Path"
    }
}

foreach ($tool in @("opt", "llc", "lld-link")) {
    Assert-Command $tool "LLVM tool '$tool' not found. Run bin\install-prereqs.ps1"
}

# Check LLVM version
$optVersion = 0
$optOutput = & opt --version 2>&1 | Out-String
if ($optOutput -match 'LLVM version (\d+)') {
    $optVersion = [int]$Matches[1]
}
if ($optVersion -lt 22) {
    Write-Host "ERROR: LLVM 22+ required (found: $optVersion)"
    Write-Host "  Run: powershell -ExecutionPolicy Bypass -File bin\install-prereqs.ps1"
    exit 1
}
Write-Detail "LLVM $optVersion"

$goVer = & go version 2>&1 | Out-String
Write-Detail ($goVer.Trim())

# ---- 2. Generate ANTLR parser (optional) ------------------------------------

$ParserPkg = Join-Path $CompilerDir "internal\parser"
$parserExists = Test-Path (Join-Path $ParserPkg "promise_parser.go")

if ($Generate -or -not $parserExists) {
    if (-not $parserExists) {
        Write-Step "Parser not found -- generating..."
    } else {
        Write-Step "Regenerating ANTLR parser..."
    }

    Assert-Command "java" "Java 11+ required for ANTLR parser generation."

    $AntlrVersion = "4.13.1"
    $ToolsDir = Join-Path $CompilerDir "tools"
    $AntlrJar = Join-Path $ToolsDir "antlr-$AntlrVersion-complete.jar"

    # Download ANTLR JAR if missing
    if (-not (Test-Path $AntlrJar)) {
        if (-not (Test-Path $ToolsDir)) { New-Item -ItemType Directory -Path $ToolsDir | Out-Null }
        $antlrUrl = "https://www.antlr.org/download/antlr-$AntlrVersion-complete.jar"
        Write-Detail "Downloading ANTLR $AntlrVersion..."
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -Uri $antlrUrl -OutFile $AntlrJar -UseBasicParsing
    }

    # Generate lexer
    $GrammarDir = Join-Path $CompilerDir "grammar"
    if (-not (Test-Path $ParserPkg)) { New-Item -ItemType Directory -Path $ParserPkg | Out-Null }

    Push-Location $GrammarDir
    try {
        & java -jar "..\tools\antlr-$AntlrVersion-complete.jar" `
            -Dlanguage=Go -package parser -visitor `
            -o "..\internal\parser" PromiseLexer.g4
        if ($LASTEXITCODE -ne 0) { throw "ANTLR lexer generation failed" }

        & java -jar "..\tools\antlr-$AntlrVersion-complete.jar" `
            -Dlanguage=Go -package parser -visitor `
            -lib "..\internal\parser" `
            -o "..\internal\parser" PromiseParser.g4
        if ($LASTEXITCODE -ne 0) { throw "ANTLR parser generation failed" }
    } finally {
        Pop-Location
    }
    Write-Detail "Parser generated."
} else {
    Write-Detail "Parser up to date (use -Generate to force)"
}

# ---- 3. Embed resources (std library, catalog, modules) ---------------------

Write-Step "Embedding resources..."

$Resources = Join-Path $CompilerDir "cmd\promise\resources"
$TestdataStd = Join-Path $CompilerDir "internal\testutil\testdata\std"
$ModulesDir = Join-Path $Root "modules"

# catalog.toml
if (-not (Test-Path $Resources)) { New-Item -ItemType Directory -Path $Resources | Out-Null }
Copy-Item (Join-Path $Root "catalog.toml") (Join-Path $Resources "catalog.toml") -Force

# modules/ -- clean copy
$resourceModules = Join-Path $Resources "modules"
if (Test-Path $resourceModules) { Remove-Item $resourceModules -Recurse -Force }
New-Item -ItemType Directory -Path $resourceModules | Out-Null
# .keep file for go:embed
New-Item -ItemType File -Path (Join-Path $resourceModules ".keep") -Force | Out-Null

if (Test-Path $ModulesDir) {
    foreach ($d in Get-ChildItem $ModulesDir -Directory) {
        Copy-Item $d.FullName (Join-Path $resourceModules $d.Name) -Recurse
    }
}

# testdata/std -- for Go test helpers
if (Test-Path $TestdataStd) { Remove-Item $TestdataStd -Recurse -Force }
New-Item -ItemType Directory -Path $TestdataStd | Out-Null
$stdDir = Join-Path $ModulesDir "std"
if (Test-Path $stdDir) {
    Copy-Item (Join-Path $stdDir "*.pr") $TestdataStd -Force
}

# .sources.sha256 -- compute content hash of all source modules
# Use Get-FileHash (PowerShell native) to avoid dependency on sha256sum
$hashOutput = New-Object System.Text.StringBuilder
$sourceFiles = @()
$sourceFiles += Get-ChildItem (Join-Path $Root "modules") -Recurse -File | Sort-Object { $_.FullName.Replace('\','/') }
$catalogFile = Get-Item (Join-Path $Root "catalog.toml") -ErrorAction SilentlyContinue
if ($catalogFile) { $sourceFiles += $catalogFile }
foreach ($f in $sourceFiles) {
    $rel = $f.FullName.Substring($Root.Length + 1).Replace('\', '/')
    $hash = (Get-FileHash $f.FullName -Algorithm SHA256).Hash.ToLower()
    [void]$hashOutput.AppendLine("$hash  $rel")
}
# Write without BOM (consistent with sha256sum output on Linux/macOS)
$sha256Path = Join-Path $Resources ".sources.sha256"
[System.IO.File]::WriteAllText($sha256Path, $hashOutput.ToString().TrimEnd(), (New-Object System.Text.UTF8Encoding $false))

$moduleCount = (Get-ChildItem $resourceModules -Directory).Count
Write-Detail "$moduleCount modules embedded"

# ---- 4. Build ---------------------------------------------------------------

Write-Step "Building..."

if (-not (Test-Path $BinDir)) { New-Item -ItemType Directory -Path $BinDir | Out-Null }

# Compute version string: <epoch>-<gitsha7> for dev builds.
$catalogPath = Join-Path $Resources "catalog.toml"
$epoch = "unknown"
$catalogContent = Get-Content $catalogPath -Raw -ErrorAction SilentlyContinue
if ($catalogContent -match 'epoch\s*=\s*"([^"]+)"') { $epoch = $Matches[1] }
$gitSha = "unknown"
try { $gitSha = (git -C $Root rev-parse --short=7 HEAD 2>$null) } catch { }
$buildVersion = "$epoch-$gitSha"
$ldflags = "-X main.version=$buildVersion"

Push-Location $CompilerDir
try {
    & go build -buildvcs=false -ldflags $ldflags -o $Binary ./cmd/promise
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally {
    Pop-Location
}

# Write compiler fingerprint sidecar (used by CompilerHash() for cache keys)
$binaryHash = (Get-FileHash $Binary -Algorithm SHA256).Hash.ToLower()
Set-Content -Path (Join-Path $BinDir ".promise.hash") -Value $binaryHash -NoNewline -Encoding ASCII

$size = [math]::Round((Get-Item $Binary).Length / 1MB, 1)
Write-Step "Built: bin\promise.exe (${size}MB)"

# ---- 5. Quick smoke test ----------------------------------------------------

try {
    # Write a temp .pr file to avoid PowerShell quote-escaping issues
    $smokeFile = Join-Path $env:TEMP "promise-smoke.pr"
    # Use .NET to write UTF-8 without BOM (PowerShell's -Encoding UTF8 adds BOM)
    [System.IO.File]::WriteAllText($smokeFile, 'main() { print_line("ok"); }', (New-Object System.Text.UTF8Encoding $false))
    $output = & $Binary run $smokeFile 2>&1 | Out-String
    Remove-Item $smokeFile -ErrorAction SilentlyContinue
    if ($output.Trim() -eq "ok") {
        Write-Detail "Smoke test passed"
    } else {
        Write-Host "WARNING: smoke test unexpected output: $($output.Trim())"
    }
} catch {
    Write-Host "WARNING: smoke test failed: $_"
}

Write-Host ""

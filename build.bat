@echo off
REM Build the Promise compiler. Output: bin\promise.exe
REM This is the ONLY correct way to build on Windows. Do not run `go build` directly.
setlocal

set "ROOT=%~dp0"
REM Remove trailing backslash
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"

REM Ensure git hooks are configured (idempotent)
git -C "%ROOT%" config core.hooksPath .githooks 2>nul

cd /d "%ROOT%\compiler"

REM Generate parser from grammar (no-op if up to date)
make generate --no-print-directory
if errorlevel 1 (
    echo ERROR: make generate failed
    exit /b 1
)

REM Embed resources (std library, catalog, modules)
make resources --no-print-directory
if errorlevel 1 (
    echo ERROR: make resources failed
    exit /b 1
)

REM Build
if not exist "%ROOT%\bin" mkdir "%ROOT%\bin"
go build -buildvcs=false -o "%ROOT%\bin\promise.exe" .\cmd\promise
if errorlevel 1 (
    echo ERROR: go build failed
    exit /b 1
)

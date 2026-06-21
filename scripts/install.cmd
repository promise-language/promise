@echo off
:: Promise language installer (Windows / cmd.exe) - thin shim.
::
:: The PowerShell idiom (irm ... | iex) does not work in cmd.exe, so this shim just
:: re-invokes install.ps1 (the single real implementation; see scripts/install.ps1).
::
::   curl -fsSL https://github.com/promise-language/promise/releases/latest/download/install.cmd -o install.cmd && install.cmd && del install.cmd
::
:: Forwards no arguments - for a pinned epoch or the full variant, run install.ps1
:: directly with -Epoch / -Full (see scripts/install.ps1).
powershell -ExecutionPolicy Bypass -Command "irm https://github.com/promise-language/promise/releases/latest/download/install.ps1 | iex"

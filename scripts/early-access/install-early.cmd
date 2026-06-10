@echo off
:: Promise language installer (EARLY ACCESS, Windows / cmd.exe) - thin shim.
::
:: The PowerShell idiom (irm ... | iex) does not work in cmd.exe, so this shim just
:: re-invokes install-early.ps1 (the single real implementation; see
:: scripts/early-access/install-early.ps1).
::
::   curl -fsSL https://promise-lang.org/install-early.cmd -o install-early.cmd && install-early.cmd && del install-early.cmd
::
:: Forwards no arguments - for the full variant, run install-early.ps1 directly
:: with -Full (see scripts/early-access/install-early.ps1).
::
:: Early-access companion to install.cmd while the GitHub repo is private (T0804):
:: install-early.ps1 fetches assets from the public prebuilts dist bucket, not
:: GitHub releases. Delete this shim once the repo goes public (T0804).
powershell -ExecutionPolicy Bypass -Command "irm https://promise-lang.org/install-early.ps1 | iex"

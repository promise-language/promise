# Installing Promise

Promise installs as a single self-contained binary (compiler + standard library +
catalog modules + runtime). The install script detects your platform, downloads
the matching release binary, verifies its checksum, and sets up `~/.promise/`.

## macOS (Apple Silicon) or Linux (x86_64)

```sh
curl -sSfL https://github.com/promise-language/promise/releases/latest/download/install.sh | sh
```

## Windows (PowerShell)

```powershell
irm https://github.com/promise-language/promise/releases/latest/download/install.ps1 | iex
```

That's it — `promise` is on your `PATH`. Verify with `promise version`, and keep it
current with `promise update`.

## Notes

- **Supported platforms:** macOS Apple Silicon (arm64), Linux x86_64, Windows x86_64.
  Intel Macs (x86_64) are not supported.
- **macOS** also needs the Xcode Command Line Tools (`xcode-select --install`) for
  now; a bundled SDK stub is on the way.
- **Pin an epoch** instead of the latest stable:
  `curl -sSfL https://github.com/promise-language/promise/releases/latest/download/install.sh | sh -s -- --epoch 2026.0`
- **No-script install** (direct binary download + checksum verify) is
  documented in [distribution.md](distribution.md) §2.3.

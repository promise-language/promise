# Security Policy

Thank you for helping keep Promise and its users safe.

## Reporting a vulnerability

**Please report security issues privately. Do not open a public issue, pull
request, or discussion for a suspected vulnerability.**

Email **security@promise-lang.org** with the details. If you would like to
encrypt your report, say so in an initial message and we will arrange a secure
channel.

We will acknowledge your report, investigate, and keep you updated on progress
toward a fix. We ask that you give us a reasonable opportunity to release a fix
before any public disclosure — **no details should be made public until a fixed
release has shipped.** We are happy to credit reporters who wish to be named
once the fix is out.

## What to include

A good report lets us reproduce the issue quickly. Please include:

- A description of the vulnerability and its impact.
- A **minimal reproduction**: the smallest `.pr` source that triggers it, plus
  the exact `promise build` / `promise run` command you used.
- The output of `promise version`.
- Your platform / OS (Linux, macOS, Windows, or WASM target).
- Expected behavior vs. actual behavior, and the verbatim error or crash output
  if any.

## Supported versions

Promise is **pre-1.0 and under active development.** Only the **current epoch**
(the latest released catalog, e.g. `2026.0`) receives security fixes. There are
no long-term-support or back-ported releases at this stage; fixes ship in the
next epoch release. Please make sure you can reproduce the issue on the current
epoch before reporting.

## Scope

This policy covers the Promise compiler, runtime, standard library, and catalog
modules in the `promise-language` organization. Modules in the
`promise-community` organization are independently maintained and are **not**
covered by this policy or by Promise Lang LLC — report issues with a community
module to that module's maintainers.

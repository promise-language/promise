---
name: Bug report
about: Report a compiler, runtime, or standard-library bug
title: ""
labels: bug
assignees: ""
---

<!--
Before filing: please confirm you can reproduce on the current epoch, and that
this is NOT a security issue. Security issues must be reported privately per
SECURITY.md (security@promise-lang.org), not here.
-->

### What happened

<!-- A clear description of the bug and its impact. -->

### Minimal reproduction

<!--
The SMALLEST `.pr` source that triggers it, plus the EXACT command you ran.
If you found a variant that compiles fine, include it too — it pins down the trigger.
-->

```promise
// smallest .pr that reproduces the problem
```

```sh
# exact command, e.g.:
promise build repro.pr
```

### `promise version` output

```
# paste the output of `promise version` here
```

### Platform / OS

<!-- e.g. macOS 15 (arm64), Ubuntu 24.04 (x86_64), Windows 11, or WASM target -->

### Expected vs. actual

<!-- What you expected to happen, and what actually happened (verbatim error/crash output). -->

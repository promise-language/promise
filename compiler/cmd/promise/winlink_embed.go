package main

import "embed"

// embeddedWinLink carries the self-generated Windows import libraries (T0772):
// MSVC-ABI import libs for kernel32.dll, advapi32.dll, ws2_32.dll, and
// ucrtbase.dll, produced from license-clean symbol-list .def files via
// llvm-dlltool (see tools/build/winlink/). They are the entire external link
// surface a Promise .exe needs — combined with the codegen-emitted crt0
// (@__promise_start), TLS directory (_tls_used), __chkstk, and _fltused — so
// linking requires NO Visual Studio Build Tools or Windows SDK, and re-hosts no
// Microsoft toolchain file.
//
// Embedded unconditionally (not host-gated) so that cross-compiling a Windows
// target from any host has the surface available; ~21 KiB total.
//
//go:embed resources/winlink/windows-amd64/*.lib
var embeddedWinLink embed.FS

const hasEmbeddedWinLink = true

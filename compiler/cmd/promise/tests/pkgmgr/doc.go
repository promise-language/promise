// Package pkgmgr holds cmd/promise's black-box package-manager tests: adding,
// pinning, updating and removing module dependencies, and building against
// modules that live in subdirectories of a remote repo — all driven through the
// built compiler rather than by calling runAdd and friends in process.
//
// These are the most expensive tests in the tree. Each one clones fixture git
// repos and runs a real verification build, and thirteen of them accounted for
// 760 of the 1994 seconds of work in the cmd/promise package. Living out here
// gives them their own process, their own build-cache entry, and concurrency
// with the rest of the suite, the same trade codegen's per-area packages make
// (T1776).
//
// Only black-box tests can live here: Go will not let anything import package
// main, so a test that calls runAdd or reads embeddedCatalog directly stays
// behind. The unit tests over these same commands — flag parsing, usage errors,
// catalog lookups — are fast, need those internals, and remain in package main.
package pkgmgr

// Package buildrun holds cmd/promise's black-box tests over the build and run
// commands: what `promise build`, `promise run`, `promise emit-ir` and
// `promise exec` do to a real project directory, what they name the binary,
// which files they include, what they forward to the compiled program, and when
// they hit the exec cache.
//
// Every one of these compiles a Promise program, so the group cost 164 of the
// 1994 seconds of work in the cmd/promise package. Out here it runs as its own
// process and its own build-cache entry, beside the rest of the suite (T1776).
//
// The cache-key unit tests over the same commands stay in package main: they
// call computeExecBinaryCacheKey and friends directly, and nothing can import
// package main.
package buildrun

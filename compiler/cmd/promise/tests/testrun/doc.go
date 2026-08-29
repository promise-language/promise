// Package testrun holds cmd/promise's black-box tests over the `promise test`
// runner itself: what it prints and what it exits with when a test process
// times out, wedges, blows its memory limit, or dies mid-batch without
// reporting — plus the build-cache correctness gates that can only be observed
// across two whole compiler runs.
//
// Each of these compiles a Promise program and runs it to completion (or to its
// deadline), so they are slow by construction: seventeen of them accounted for
// 396 of the 1994 seconds of work in the cmd/promise package. Out here they get
// their own process and build-cache entry and run beside the rest of the suite
// (T1776).
//
// Their in-process counterparts stay in package main: parsing a child's output,
// building a roster, formatting a truncated-batch summary and the rest all call
// unexported functions, and Go will not let anything import package main. The
// division is not by subject but by whether the assertion is about the built
// binary's observable behavior or about a function inside it.
package testrun

package main

// Test parallelism policy for this package (T1776).
//
// Most tests here call t.Parallel(): the package is dominated by tests that
// drive the built `promise` binary as a subprocess, and running them one at a
// time made this the slowest package in the repo. A test may be parallel only
// when it touches nothing process-global. Concretely, do NOT add t.Parallel()
// to a test that:
//
//   - sets the environment or the working directory — t.Setenv/t.Chdir panic
//     under t.Parallel, and os.Setenv/os.Chdir silently corrupt their neighbors.
//     A subprocess test wanting its own PROMISE_HOME should pass it through
//     cmd.Env instead, and its own directory through cmd.Dir;
//   - captures or reassigns os.Stdout/os.Stderr/os.Stdin, or parses the global
//     flag set — the streams are shared by every concurrently running test;
//   - assigns a package-level variable (production or test-only), directly or
//     through a helper;
//   - runs the compiler frontend in-process — anything reaching sema, codegen,
//     types, ast, parser or module. Those packages keep mutable state that two
//     concurrent compiles corrupt: the symptom is a nil dereference deep in
//     sema, or "concurrent map read and map write", not a clean failure.
//
// The last rule is why the package-manager and doc tests are still serial: they
// call runAdd/runPkgUpdate/emitDoc in-process. Converting one of those to drive
// the binary as a subprocess makes it parallelizable — that is the way to claw
// back the remaining serial time, not relaxing the rules above.

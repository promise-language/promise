#include <unistd.h>
#include <sys/wait.h>
#include <stdio.h>

// Run a test function in a forked child process.
// Returns 0 on success, 1 on failure (panic/crash/non-zero exit).
//
// This is the only remaining C runtime function. fork/waitpid cannot be
// expressed in pure LLVM IR without platform-specific inline assembly.
// Deferred to Phase 5 (thread-based test isolation).
int promise_test_run(void (*fn)()) {
    fflush(stdout);
    fflush(stderr);
    pid_t pid = fork();
    if (pid == 0) {
        // Child: run the test, exit 0 on success
        fn();
        _exit(0);
    }
    // Parent: wait for child
    int status;
    waitpid(pid, &status, 0);
    if (WIFEXITED(status) && WEXITSTATUS(status) == 0) {
        return 0; // pass
    }
    return 1; // fail
}

#include <unistd.h>
#include <sys/wait.h>
#include <stdio.h>
#include <string.h>

// Run a test function in a forked child process.
// Returns 0 on success, 1 on failure (panic/crash/non-zero exit).
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

// Print test result: name and PASS/FAIL
void promise_test_print_result(const char* name, int failed) {
    if (failed) {
        printf("FAIL %s\n", name);
    } else {
        printf("PASS %s\n", name);
    }
}

// Print test summary line
void promise_test_summary(int passed, int failed) {
    printf("\n%d passed, %d failed\n", passed, failed);
}

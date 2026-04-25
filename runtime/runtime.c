#include <stdio.h>
#include <stdlib.h>
// promise_bindings.h is auto-included via clang -include flag

void promise_print_int(promise_int_v x)   { printf("%lld\n", (long long)x.raw); }
void promise_print_f64(promise_f64_v x)   { printf("%g\n", x.raw); }
void promise_print_bool(promise_bool_v x) { printf(x.raw ? "true\n" : "false\n"); }
void promise_panic(const char* msg)       { fprintf(stderr, "panic: %s\n", msg); exit(1); }

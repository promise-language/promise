#include <stdio.h>
#include <stdlib.h>

void promise_print_int(long long x)   { printf("%lld\n", x); }
void promise_print_f64(double x)      { printf("%g\n", x); }
void promise_print_bool(char x)       { printf(x ? "true\n" : "false\n"); }
void promise_panic(const char* msg)   { fprintf(stderr, "panic: %s\n", msg); exit(1); }

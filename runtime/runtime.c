#include <stdio.h>
#include <stdlib.h>
#include "promise_bindings.h"

void promise_print_int(promise_int_v *x)   { printf("%lld\n", (long long)x->raw); }
void promise_print_f64(promise_f64_v *x)   { printf("%g\n", x->raw); }
void promise_print_bool(promise_bool_v *x) { printf(x->raw ? "true\n" : "false\n"); }
void promise_panic(const char* msg)       { fprintf(stderr, "panic: %s\n", msg); exit(1); }
void promise_panic_msg(promise_string_v *s) {
    fprintf(stderr, "panic: %.*s\n", (int)s->_instance->len, s->_instance->data);
    exit(1);
}

// promise_type_is is now codegen-emitted LLVM IR
// (see compiler/internal/codegen/rtti.go: defineTypeIsFunc)

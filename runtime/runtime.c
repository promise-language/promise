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

/* RTTI: type info struct stored in the _variant pointer of each instance. */
#include <stdint.h>
typedef struct {
    void*   vtable_ptr;
    int32_t type_id;
    int32_t num_parents;
    int32_t parent_ids[];
} promise_typeinfo;

/* Check if the type identified by variant_ptr is or inherits from expected_id. */
int32_t promise_type_is(void* variant_ptr, int32_t expected_id) {
    if (!variant_ptr) return 0;
    promise_typeinfo* info = (promise_typeinfo*)variant_ptr;
    if (info->type_id == expected_id) return 1;
    for (int32_t i = 0; i < info->num_parents; i++) {
        if (info->parent_ids[i] == expected_id) return 1;
    }
    return 0;
}

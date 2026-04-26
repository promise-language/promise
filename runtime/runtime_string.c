#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include "promise_bindings.h"

promise_string_i* promise_string_new(const char* data, int64_t len) {
    promise_string_i* s = (promise_string_i*)malloc(sizeof(promise_string_i) + len);
    s->_variant = NULL;
    s->len = len;
    memcpy(s->data, data, len);
    return s;
}

promise_string_i* promise_string_concat(promise_string_i* a, promise_string_i* b) {
    int64_t total = a->len + b->len;
    promise_string_i* s = (promise_string_i*)malloc(sizeof(promise_string_i) + total);
    s->_variant = NULL;
    s->len = total;
    memcpy(s->data, a->data, a->len);
    memcpy(s->data + a->len, b->data, b->len);
    return s;
}

_Bool promise_string_eq(promise_string_i* a, promise_string_i* b) {
    if (a->len != b->len) return 0;
    return memcmp(a->data, b->data, a->len) == 0;
}

void promise_print_string(promise_string_v s) {
    fwrite(s._instance->data, 1, s._instance->len, stdout);
    putchar('\n');
}

// --- Value-to-string conversion for string interpolation ---

promise_string_i* promise_int_to_string(int64_t x) {
    char buf[32];
    int len = snprintf(buf, sizeof(buf), "%lld", (long long)x);
    return promise_string_new(buf, len);
}

promise_string_i* promise_f64_to_string(double x) {
    char buf[64];
    int len = snprintf(buf, sizeof(buf), "%g", x);
    return promise_string_new(buf, len);
}

promise_string_i* promise_bool_to_string(int8_t x) {
    if (x) {
        return promise_string_new("true", 4);
    }
    return promise_string_new("false", 5);
}

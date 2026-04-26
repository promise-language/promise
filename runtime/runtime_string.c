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

// --- Char-to-string conversion (UTF-8 encode a Unicode codepoint) ---

promise_string_i* promise_char_to_string(int32_t cp) {
    char buf[4];
    int len = 0;
    if (cp < 0x80) {
        buf[0] = (char)cp; len = 1;
    } else if (cp < 0x800) {
        buf[0] = 0xC0 | (cp >> 6);
        buf[1] = 0x80 | (cp & 0x3F);
        len = 2;
    } else if (cp < 0x10000) {
        buf[0] = 0xE0 | (cp >> 12);
        buf[1] = 0x80 | ((cp >> 6) & 0x3F);
        buf[2] = 0x80 | (cp & 0x3F);
        len = 3;
    } else {
        buf[0] = 0xF0 | (cp >> 18);
        buf[1] = 0x80 | ((cp >> 12) & 0x3F);
        buf[2] = 0x80 | ((cp >> 6) & 0x3F);
        buf[3] = 0x80 | (cp & 0x3F);
        len = 4;
    }
    return promise_string_new(buf, len);
}

// --- String character iteration (UTF-8 decode) ---

int32_t promise_string_next_char(promise_string_i* s, int64_t* pos) {
    if (*pos >= s->len) return -1;
    unsigned char b = (unsigned char)s->data[*pos];
    int32_t cp;
    int n;
    if (b < 0x80)      { cp = b;          n = 1; }
    else if (b < 0xE0) { cp = b & 0x1F;   n = 2; }
    else if (b < 0xF0) { cp = b & 0x0F;   n = 3; }
    else               { cp = b & 0x07;   n = 4; }
    for (int i = 1; i < n && (*pos + i) < s->len; i++)
        cp = (cp << 6) | (s->data[*pos + i] & 0x3F);
    *pos += n;
    return cp;
}

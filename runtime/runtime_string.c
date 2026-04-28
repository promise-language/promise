#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include "promise_bindings.h"

extern void promise_panic(const char* msg);

promise_string_i* promise_string_new(const char* data, int64_t len) {
    promise_string_i* s = (promise_string_i*)malloc(sizeof(promise_string_i) + len);
    if (!s) promise_panic("out of memory");
    s->_variant = NULL;
    s->len = len;
    memcpy(s->data, data, len);
    return s;
}

promise_string_i* promise_string_concat(promise_string_i* a, promise_string_i* b) {
    int64_t total = a->len + b->len;
    promise_string_i* s = (promise_string_i*)malloc(sizeof(promise_string_i) + total);
    if (!s) promise_panic("out of memory");
    s->_variant = NULL;
    s->len = total;
    memcpy(s->data, a->data, a->len);
    memcpy(s->data + a->len, b->data, b->len);
    return s;
}

void promise_print_string(promise_string_v *s) {
    fwrite(s->_instance->data, 1, s->_instance->len, stdout);
    putchar('\n');
}

// promise_int_to_string, promise_f64_to_string, promise_bool_to_string,
// promise_char_to_string are now codegen-emitted LLVM IR
// (see compiler/internal/codegen/compiler.go: defineIntToStringFunc, etc.)

// promise_string_trim returns a new string with leading/trailing whitespace removed.
promise_string_i* promise_string_trim(promise_string_i* s) {
    int64_t start = 0;
    int64_t end = s->len;
    while (start < end && (s->data[start] == ' ' || s->data[start] == '\t' ||
                           s->data[start] == '\n' || s->data[start] == '\r'))
        start++;
    while (end > start && (s->data[end-1] == ' ' || s->data[end-1] == '\t' ||
                           s->data[end-1] == '\n' || s->data[end-1] == '\r'))
        end--;
    return promise_string_new(s->data + start, end - start);
}

// promise_string_split splits s by sep and returns a slice of strings.
// Slice layout: [len:i64, cap:i64, data...] where each element is an i8* (promise_string_i*)
void* promise_string_split(promise_string_i* s, promise_string_i* sep) {
    // Count splits first
    int64_t count = 1;
    if (sep->len > 0) {
        for (int64_t i = 0; i <= s->len - sep->len; i++) {
            if (memcmp(s->data + i, sep->data, sep->len) == 0) {
                count++;
                i += sep->len - 1;
            }
        }
    }

    // Allocate slice: header (16 bytes) + count * sizeof(pointer)
    int64_t elem_size = sizeof(void*);
    int64_t header = 16;
    void* slice = malloc(header + count * elem_size);
    if (!slice) promise_panic("out of memory");
    int64_t* hdr = (int64_t*)slice;
    hdr[0] = count; // len
    hdr[1] = count; // cap
    void** elems = (void**)((uint8_t*)slice + header);

    // Split
    int64_t pos = 0;
    int64_t idx = 0;
    if (sep->len == 0) {
        // Empty separator: return the whole string as a single element
        elems[0] = promise_string_new(s->data, s->len);
    } else {
        for (int64_t i = 0; i <= s->len - sep->len; i++) {
            if (memcmp(s->data + i, sep->data, sep->len) == 0) {
                elems[idx++] = promise_string_new(s->data + pos, i - pos);
                pos = i + sep->len;
                i += sep->len - 1;
            }
        }
        elems[idx] = promise_string_new(s->data + pos, s->len - pos);
    }

    return slice;
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

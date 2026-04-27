#include <stdint.h>
#include <string.h>

// FNV-1a hash mixing for 8 bytes of integer data.
int64_t promise_hash_int(int64_t val) {
    const uint8_t* data = (const uint8_t*)&val;
    uint64_t hash = 14695981039346656037ULL;
    for (int i = 0; i < 8; i++) {
        hash ^= data[i];
        hash *= 1099511628211ULL;
    }
    return (int64_t)hash;
}

// Hash a string value by its content.
// ptr is a promise_string_i* (pointer to string header: {variant_ptr, len, data...}).
typedef struct {
    void*   _variant;
    int64_t len;
    char    data[];
} promise_string_header;

int64_t promise_hash_string_value(const void* ptr) {
    if (!ptr) return 0;
    const promise_string_header* s = (const promise_string_header*)ptr;
    uint64_t hash = 14695981039346656037ULL;
    for (int64_t i = 0; i < s->len; i++) {
        hash ^= (uint8_t)s->data[i];
        hash *= 1099511628211ULL;
    }
    return (int64_t)hash;
}

// Compare two string keys by content (used by Vector.contains for string elements).
int32_t promise_eq_string(const void* a, const void* b, int64_t key_size) {
    (void)key_size;
    const void* pa = *(const void**)a;
    const void* pb = *(const void**)b;
    if (pa == pb) return 1;
    if (!pa || !pb) return 0;
    const promise_string_header* sa = (const promise_string_header*)pa;
    const promise_string_header* sb = (const promise_string_header*)pb;
    if (sa->len != sb->len) return 0;
    return memcmp(sa->data, sb->data, sa->len) == 0 ? 1 : 0;
}

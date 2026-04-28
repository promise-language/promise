#include <stdint.h>
#include <string.h>

// String instance layout (matches codegen's string instance struct).
typedef struct {
    void*   _variant;
    int64_t len;
    char    data[];
} promise_string_header;

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

#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdio.h>

extern void promise_panic(const char* msg);

// Slice layout: [len:i64, cap:i64, data...]
// Header size = 16 bytes (matching codegen sliceHeaderSize)

typedef struct {
    int64_t len;
    int64_t cap;
} slice_header_t;

static inline slice_header_t* hdr(void* slice) {
    return (slice_header_t*)slice;
}

static inline uint8_t* data(void* slice) {
    return (uint8_t*)slice + sizeof(slice_header_t);
}

// promise_slice_push appends an element to the slice.
// Returns the (possibly reallocated) slice pointer.
void* promise_slice_push(void* slice, const void* elem, int64_t elem_size) {
    slice_header_t* h = hdr(slice);
    if (h->len >= h->cap) {
        int64_t new_cap = h->cap == 0 ? 4 : h->cap * 2;
        int64_t new_size = sizeof(slice_header_t) + new_cap * elem_size;
        void* new_slice = realloc(slice, new_size);
        if (!new_slice) promise_panic("out of memory");
        slice = new_slice;
        h = hdr(slice);
        h->cap = new_cap;
    }
    memcpy(data(slice) + h->len * elem_size, elem, elem_size);
    h->len++;
    return slice;
}

// promise_slice_pop removes and returns the last element.
// Copies element to out_elem. Returns 1 if successful, 0 if empty.
int32_t promise_slice_pop(void* slice, void* out_elem, int64_t elem_size) {
    slice_header_t* h = hdr(slice);
    if (h->len == 0) return 0;
    h->len--;
    memcpy(out_elem, data(slice) + h->len * elem_size, elem_size);
    return 1;
}

// promise_slice_contains checks if an element exists in the slice.
// eq_fn: int32_t (*)(const void* a, const void* b, int64_t size) or NULL for memcmp.
int8_t promise_slice_contains(void* slice, const void* elem, int64_t elem_size, void* eq_fn) {
    slice_header_t* h = hdr(slice);
    typedef int32_t (*eq_func_t)(const void*, const void*, int64_t);

    for (int64_t i = 0; i < h->len; i++) {
        const void* cur = data(slice) + i * elem_size;
        if (eq_fn) {
            if (((eq_func_t)eq_fn)(cur, elem, elem_size)) return 1;
        } else {
            if (memcmp(cur, elem, elem_size) == 0) return 1;
        }
    }
    return 0;
}

// promise_slice_remove removes an element at the given index by shifting.
void promise_slice_remove(void* slice, int64_t index, int64_t elem_size) {
    slice_header_t* h = hdr(slice);
    if (index < 0 || index >= h->len) {
        promise_panic("slice remove: index out of bounds");
    }
    uint8_t* d = data(slice);
    if (index < h->len - 1) {
        memmove(d + index * elem_size, d + (index + 1) * elem_size,
                (h->len - index - 1) * elem_size);
    }
    h->len--;
}

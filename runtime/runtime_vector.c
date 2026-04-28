#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdio.h>

extern void promise_panic(const char* msg);

// Vector layout: [len:i64, cap:i64, data...]
// Header size = 16 bytes (matching codegen vectorHeaderSize)

typedef struct {
    int64_t len;
    int64_t cap;
} vector_header_t;

static inline vector_header_t* hdr(void* vec) {
    return (vector_header_t*)vec;
}

static inline uint8_t* data(void* vec) {
    return (uint8_t*)vec + sizeof(vector_header_t);
}

// promise_vector_with_capacity allocates a vector with len=0 and given capacity.
void* promise_vector_with_capacity(int64_t capacity, int64_t elem_size) {
    if (capacity < 0) capacity = 0;
    int64_t total = sizeof(vector_header_t) + capacity * elem_size;
    void* vec = malloc(total);
    if (!vec) promise_panic("out of memory");
    hdr(vec)->len = 0;
    hdr(vec)->cap = capacity;
    return vec;
}

// promise_vector_push appends an element to the vector.
// Returns the (possibly reallocated) vector pointer.
void* promise_vector_push(void* vec, const void* elem, int64_t elem_size) {
    vector_header_t* h = hdr(vec);
    if (h->len >= h->cap) {
        int64_t new_cap = h->cap == 0 ? 4 : h->cap * 2;
        int64_t new_size = sizeof(vector_header_t) + new_cap * elem_size;
        void* new_vec = realloc(vec, new_size);
        if (!new_vec) promise_panic("out of memory");
        vec = new_vec;
        h = hdr(vec);
        h->cap = new_cap;
    }
    memcpy(data(vec) + h->len * elem_size, elem, elem_size);
    h->len++;
    return vec;
}

// promise_vector_pop removes and returns the last element.
// Copies element to out_elem. Returns 1 if successful, 0 if empty.
int32_t promise_vector_pop(void* vec, void* out_elem, int64_t elem_size) {
    vector_header_t* h = hdr(vec);
    if (h->len == 0) return 0;
    h->len--;
    memcpy(out_elem, data(vec) + h->len * elem_size, elem_size);
    return 1;
}

// promise_vector_contains and promise_vector_remove are now codegen-emitted LLVM IR
// (see compiler/internal/codegen/compiler.go: defineVectorContainsFunc, defineVectorRemoveFunc)

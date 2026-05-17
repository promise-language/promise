// wasm_alloc.c — Free-list allocator for WASM (no libc).
//
// Size-class segregated free-list with sbrk via memory.grow.
// Single-threaded (WASM has no threads). Self-initializing from __heap_base.
//
// Rebuild: clang --target=wasm32-unknown-wasi -O2 -nostdlib -c wasm_alloc.c -o wasm_alloc.o

typedef unsigned int uint32;
typedef unsigned long size_t;

// Linker-provided symbol marking the start of heap (after stack + data sections).
extern unsigned char __heap_base;

// --- sbrk implementation ---

static uint32 brk = 0;

static void *sbrk(uint32 increment) {
    if (brk == 0) {
        brk = (uint32)&__heap_base;
        brk = (brk + 15) & ~15; // align to 16
    }
    uint32 old_brk = brk;
    uint32 new_brk = old_brk + increment;

    uint32 cur_pages = __builtin_wasm_memory_size(0);
    uint32 cur_end = cur_pages * 65536;
    if (new_brk > cur_end) {
        uint32 needed = new_brk - cur_end;
        uint32 pages = (needed + 65535) / 65536;
        if (__builtin_wasm_memory_grow(0, pages) == -1)
            return (void *)-1; // OOM
    }
    brk = new_brk;
    return (void *)old_brk;
}

// --- Size class free-list ---

// 13 size classes: 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536
#define NUM_CLASSES 13
#define MIN_CLASS_SHIFT 4 // 1 << 4 = 16
#define MAX_CLASS_SIZE (1 << (MIN_CLASS_SHIFT + NUM_CLASSES - 1)) // 65536

// Each free block stores a next pointer in its first 4 bytes.
static void *free_lists[NUM_CLASSES];

// Header: 8 bytes before each user pointer.
// Bytes 0-3: class index (0..NUM_CLASSES-1) or 0xFFFFFFFF for oversized
// Bytes 4-7: allocation size (for oversized blocks and realloc)
typedef struct {
    uint32 class_idx;
    uint32 size;
} header_t;

#define HEADER_SIZE 8

// Compute size class for a given size (including header).
// Returns NUM_CLASSES if size exceeds max class.
static int size_class(uint32 size) {
    if (size <= 16) return 0;
    // Find the smallest power-of-2 >= size
    int cls = 0;
    uint32 s = size - 1;
    while (s >= 16) {
        s >>= 1;
        cls++;
    }
    return cls < NUM_CLASSES ? cls : NUM_CLASSES;
}

// Size of a given class bucket.
static uint32 class_size(int cls) {
    return (uint32)16 << cls;
}

void *malloc(size_t size) {
    if (size == 0) size = 1;

    uint32 total = size + HEADER_SIZE;

    int cls = size_class(total);

    if (cls < NUM_CLASSES) {
        uint32 bucket_sz = class_size(cls);

        // Try free list
        if (free_lists[cls]) {
            void *block = free_lists[cls];
            free_lists[cls] = *(void **)block;
            header_t *hdr = (header_t *)block;
            hdr->class_idx = cls;
            hdr->size = size;
            return (char *)block + HEADER_SIZE;
        }

        // Sbrk a new block
        void *block = sbrk(bucket_sz);
        if (block == (void *)-1) return (void *)0;
        header_t *hdr = (header_t *)block;
        hdr->class_idx = cls;
        hdr->size = size;
        return (char *)block + HEADER_SIZE;
    }

    // Oversized: allocate exact (aligned to 16)
    uint32 alloc_total = (total + 15) & ~15;
    void *block = sbrk(alloc_total);
    if (block == (void *)-1) return (void *)0;
    header_t *hdr = (header_t *)block;
    hdr->class_idx = 0xFFFFFFFF; // oversized marker
    hdr->size = size;
    return (char *)block + HEADER_SIZE;
}

void free(void *ptr) {
    if (!ptr) return;
    header_t *hdr = (header_t *)((char *)ptr - HEADER_SIZE);
    uint32 cls = hdr->class_idx;

    if (cls < NUM_CLASSES) {
        // Return to free list
        *(void **)hdr = free_lists[cls];
        free_lists[cls] = (void *)hdr;
    }
    // Oversized blocks are not reclaimed (acceptable for WASM lifetime)
}

size_t malloc_usable_size(void *ptr) {
    if (!ptr) return 0;
    header_t *hdr = (header_t *)((char *)ptr - HEADER_SIZE);
    if (hdr->class_idx < NUM_CLASSES)
        return class_size(hdr->class_idx) - HEADER_SIZE;
    return hdr->size;
}

void *realloc(void *ptr, size_t new_size) {
    if (!ptr) return malloc(new_size);
    if (new_size == 0) {
        free(ptr);
        return (void *)0;
    }

    header_t *hdr = (header_t *)((char *)ptr - HEADER_SIZE);
    uint32 old_size = hdr->size;

    // If the new size fits in the same bucket, just update the header
    if (hdr->class_idx < NUM_CLASSES) {
        uint32 bucket_sz = class_size(hdr->class_idx);
        if (new_size + HEADER_SIZE <= bucket_sz) {
            hdr->size = new_size;
            return ptr;
        }
    }

    // Allocate new, copy, free old
    void *new_ptr = malloc(new_size);
    if (!new_ptr) return (void *)0;

    uint32 copy_size = old_size < new_size ? old_size : new_size;
    char *src = (char *)ptr;
    char *dst = (char *)new_ptr;
    for (uint32 i = 0; i < copy_size; i++)
        dst[i] = src[i];

    free(ptr);
    return new_ptr;
}

// --- Canonical ABI support (Component Model) ---

// cabi_realloc — Canonical ABI memory allocation for the Component Model.
// Required export for every component; used by the host to allocate/reallocate
// memory in the component's linear memory for passing compound types.
// Alignment is naturally satisfied: all buckets are >= 16-byte aligned.
void *cabi_realloc(void *ptr, size_t old_size, size_t align, size_t new_size) {
    (void)old_size;
    (void)align;
    if (!ptr) return malloc(new_size);
    return realloc(ptr, new_size);
}

// __cabi_retarea — Fixed buffer for canonical ABI return value decoding.
// Exported as a global; the host writes multi-value returns here.
// Sized for canonical ABI max flat layout: 16 flat values × 8 bytes = 128 bytes.
__attribute__((aligned(8)))
unsigned char __cabi_retarea[128];

// Canonical ABI memory access helpers for reading/writing linear memory.
// Used by generated wrapper code to decode return values from retptr
// and encode compound parameters into linear memory buffers.
// With LTO (always enabled for WASM), these inline at call sites.

int cabi_load_i32(int ptr) {
    return *(int *)ptr;
}

void cabi_store_i32(int ptr, int val) {
    *(int *)ptr = val;
}

long long cabi_load_i64(int ptr) {
    return *(long long *)ptr;
}

void cabi_store_i64(int ptr, long long val) {
    *(long long *)ptr = val;
}

float cabi_load_f32(int ptr) {
    return *(float *)ptr;
}

void cabi_store_f32(int ptr, float val) {
    *(float *)ptr = val;
}

double cabi_load_f64(int ptr) {
    return *(double *)ptr;
}

void cabi_store_f64(int ptr, double val) {
    *(double *)ptr = val;
}

// --- Canonical ABI string helpers ---
//
// Promise string instance layout on wasm32:
//   offset 0:  i32 variant_ptr  (4 bytes)
//   offset 4:  [4 bytes padding to align i64]
//   offset 8:  i64 len          (8 bytes, bit 63 = literal flag)
//   offset 16: [N x i8] data
//
// The extern ABI passes string as i8* (instance pointer) on wasm32 = i32.

// Alloc counter declared in PAL-emitted LLVM IR.
extern long long __promise_alloc_count;

// Get data pointer from a string instance pointer.
int cabi_string_data(void *instance) {
    return (int)((char *)instance + 16);
}

// Get length from a string instance pointer (masking bit 63 literal flag).
int cabi_string_len(void *instance) {
    long long raw = *(long long *)((char *)instance + 8);
    return (int)(raw & 0x7FFFFFFFFFFFFFFFLL);
}

// Construct a new heap-allocated string instance from a (ptr, len) pair.
// Copies data from ptr into the new instance. Caller owns the result.
void *cabi_string_from(int ptr, int len) {
    int total = 16 + len;
    void *inst = malloc(total);
    if (!inst) return (void *)0;
    __promise_alloc_count++; // track for leak detection (matches pal_alloc behavior)
    *(int *)inst = 0;                              // variant_ptr = null
    *(int *)((char *)inst + 4) = 0;                // padding = 0
    *(long long *)((char *)inst + 8) = (long long)len; // len (no literal flag)
    // Copy data
    char *dst = (char *)inst + 16;
    char *src = (char *)ptr;
    for (int i = 0; i < len; i++) dst[i] = src[i];
    return inst;
}

// Return the address of the canonical ABI return area buffer.
int cabi_retarea_ptr(void) {
    return (int)__cabi_retarea;
}

// --- Canonical ABI vector helpers ---
//
// Promise vector layout on wasm32:
//   offset 0:  i64 len          (8 bytes, bit 63 = static flag)
//   offset 8:  i64 cap          (8 bytes)
//   offset 16: [N x T] data
//
// The extern ABI passes vector as i8* (header pointer) on wasm32 = i32.

// Get data pointer from a vector header pointer.
int cabi_vector_data(void *header) {
    return (int)((char *)header + 16);
}

// Get length from a vector header pointer (masking bit 63 static flag).
int cabi_vector_len(void *header) {
    long long raw = *(long long *)header;
    return (int)(raw & 0x7FFFFFFFFFFFFFFFLL);
}

// Construct a new heap-allocated vector from a (ptr, len, elem_size) triple.
// Copies data from ptr into the new vector. Caller owns the result.
void *cabi_vector_from(int data_ptr, int len, int elem_size) {
    int data_bytes = len * elem_size;
    void *header = malloc(16 + data_bytes);
    if (!header) return (void *)0;
    __promise_alloc_count++;
    *(long long *)header = (long long)len;                    // len (no static flag)
    *(long long *)((char *)header + 8) = (long long)len;     // cap = len
    char *dst = (char *)header + 16;
    char *src = (char *)data_ptr;
    for (int i = 0; i < data_bytes; i++) dst[i] = src[i];
    return header;
}

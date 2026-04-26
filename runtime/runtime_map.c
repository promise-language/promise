#include <stdlib.h>
#include <string.h>
#include <stdint.h>

// Type-erased open-addressing hash table for Promise maps.
// Keys and values are stored inline as raw bytes.

typedef struct {
    int64_t key_size;
    int64_t val_size;
    int64_t count;
    int64_t capacity;
    uint8_t* entries;       // array of { used_flag (1 byte), key_bytes, val_bytes }
    void*    hash_fn;       // int64_t (*)(const void* key, int64_t key_size) or NULL
    void*    eq_fn;         // int32_t (*)(const void* a, const void* b, int64_t key_size) or NULL
} promise_map_t;

typedef int64_t (*hash_func_t)(const void* key, int64_t key_size);
typedef int32_t (*eq_func_t)(const void* a, const void* b, int64_t key_size);

// Default FNV-1a hash over raw key bytes.
static int64_t default_hash(const void* key, int64_t key_size) {
    const uint8_t* data = (const uint8_t*)key;
    uint64_t hash = 14695981039346656037ULL;
    for (int64_t i = 0; i < key_size; i++) {
        hash ^= data[i];
        hash *= 1099511628211ULL;
    }
    return (int64_t)hash;
}

// Default equality: memcmp over raw key bytes.
static int32_t default_eq(const void* a, const void* b, int64_t key_size) {
    return memcmp(a, b, key_size) == 0 ? 1 : 0;
}

// Entry layout: [used:1][key:key_size][val:val_size]
static inline int64_t entry_size(promise_map_t* m) {
    return 1 + m->key_size + m->val_size;
}

static inline uint8_t* entry_at(promise_map_t* m, int64_t idx) {
    return m->entries + idx * entry_size(m);
}

static inline uint8_t* entry_key(promise_map_t* m, uint8_t* entry) {
    return entry + 1;
}

static inline uint8_t* entry_val(promise_map_t* m, uint8_t* entry) {
    return entry + 1 + m->key_size;
}

static int64_t map_hash(promise_map_t* m, const void* key) {
    if (m->hash_fn) return ((hash_func_t)m->hash_fn)(key, m->key_size);
    return default_hash(key, m->key_size);
}

static int32_t map_eq(promise_map_t* m, const void* a, const void* b) {
    if (m->eq_fn) return ((eq_func_t)m->eq_fn)(a, b, m->key_size);
    return default_eq(a, b, m->key_size);
}

static void map_rehash(promise_map_t* m);

// promise_map_new creates a new map with the given key/value sizes.
void* promise_map_new(int64_t key_size, int64_t val_size, void* hash_fn, void* eq_fn) {
    promise_map_t* m = (promise_map_t*)malloc(sizeof(promise_map_t));
    m->key_size = key_size;
    m->val_size = val_size;
    m->count = 0;
    m->capacity = 16;
    m->hash_fn = hash_fn;
    m->eq_fn = eq_fn;
    m->entries = (uint8_t*)calloc(m->capacity, entry_size(m));
    return m;
}

// promise_map_set inserts or updates a key-value pair.
void promise_map_set(void* mp, const void* key, const void* val) {
    promise_map_t* m = (promise_map_t*)mp;

    // Rehash at 75% load
    if (m->count * 4 >= m->capacity * 3) {
        map_rehash(m);
    }

    int64_t hash = map_hash(m, key);
    int64_t idx = (uint64_t)hash % (uint64_t)m->capacity;

    for (;;) {
        uint8_t* e = entry_at(m, idx);
        if (!e[0]) {
            // Empty slot: insert
            e[0] = 1;
            memcpy(entry_key(m, e), key, m->key_size);
            memcpy(entry_val(m, e), val, m->val_size);
            m->count++;
            return;
        }
        if (map_eq(m, entry_key(m, e), key)) {
            // Key exists: update value
            memcpy(entry_val(m, e), val, m->val_size);
            return;
        }
        idx = ((uint64_t)idx + 1) % (uint64_t)m->capacity;
    }
}

// promise_map_get looks up a key. Returns pointer to value or NULL.
void* promise_map_get(void* mp, const void* key) {
    promise_map_t* m = (promise_map_t*)mp;
    if (m->count == 0) return NULL;

    int64_t hash = map_hash(m, key);
    int64_t idx = (uint64_t)hash % (uint64_t)m->capacity;

    for (;;) {
        uint8_t* e = entry_at(m, idx);
        if (!e[0]) return NULL;  // empty slot = not found
        if (map_eq(m, entry_key(m, e), key)) {
            return entry_val(m, e);
        }
        idx = ((uint64_t)idx + 1) % (uint64_t)m->capacity;
    }
}

// promise_map_len returns the number of entries.
int64_t promise_map_len(void* mp) {
    return ((promise_map_t*)mp)->count;
}

// promise_map_iter_next advances the iteration state.
// Returns 1 if a next entry was found, 0 if done.
// state is an index into the entries array, updated on each call.
int32_t promise_map_iter_next(void* mp, int64_t* state, void* key_out, void* val_out) {
    promise_map_t* m = (promise_map_t*)mp;
    while (*state < m->capacity) {
        uint8_t* e = entry_at(m, *state);
        (*state)++;
        if (e[0]) {
            memcpy(key_out, entry_key(m, e), m->key_size);
            memcpy(val_out, entry_val(m, e), m->val_size);
            return 1;
        }
    }
    return 0;
}

static void map_rehash(promise_map_t* m) {
    int64_t old_cap = m->capacity;
    uint8_t* old_entries = m->entries;

    m->capacity *= 2;
    m->entries = (uint8_t*)calloc(m->capacity, entry_size(m));
    m->count = 0;

    for (int64_t i = 0; i < old_cap; i++) {
        uint8_t* e = old_entries + i * entry_size(m);
        if (e[0]) {
            promise_map_set(m, entry_key(m, e), entry_val(m, e));
        }
    }

    free(old_entries);
}

// --- String key hash/eq functions ---
// These dereference the i8* key to get the promise_string_i* header for content-based ops.

typedef struct {
    void*   _variant;
    int64_t len;
    char    data[];
} promise_string_header;

// promise_hash_string hashes a string key by its content.
// key points to an i8* (pointer to promise_string_i*), key_size is sizeof(i8*) = 8.
int64_t promise_hash_string(const void* key, int64_t key_size) {
    (void)key_size;
    const void* ptr = *(const void**)key;
    if (!ptr) return 0;
    const promise_string_header* s = (const promise_string_header*)ptr;
    // FNV-1a hash over string content
    uint64_t hash = 14695981039346656037ULL;
    for (int64_t i = 0; i < s->len; i++) {
        hash ^= (uint8_t)s->data[i];
        hash *= 1099511628211ULL;
    }
    return (int64_t)hash;
}

// promise_eq_string compares two string keys by content.
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

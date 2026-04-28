#include <stdio.h>
#include "promise_bindings.h"

// promise_string_trim, promise_string_split, promise_string_next_char
// are now codegen-emitted LLVM IR
// (see compiler/internal/codegen/compiler.go: defineStringTrimFunc, etc.)

void promise_print_string(promise_string_v *s) {
    fwrite(s->_instance->data, 1, s->_instance->len, stdout);
    putchar('\n');
}

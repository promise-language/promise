package bindgen

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/wit"
)

// TestT1733AnnotationOnAllTypeKinds verifies that `structural(protocol: false)
// is emitted on every generated type (record, flags, resource) but NOT on enum
// or variant declarations, which have no methods and cannot collide with protocol
// interface names.
func TestT1733AnnotationOnAllTypeKinds(t *testing.T) {
	src := `
interface types {
    record my-record { x: u32, }
    flags my-flags { read, write, }
    resource my-resource {}
    enum my-enum { a, b, }
    variant my-variant { case-a(u32), case-b, }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	out := GeneratePromise(modules, "wasi")

	// record → type with `structural(protocol: false)
	assertContains(t, out, "type MyRecord `structural(protocol: false) `public {")

	// flags → type with `structural(protocol: false)
	assertContains(t, out, "type MyFlags `structural(protocol: false) `public {")

	// resource → type with `structural(protocol: false)
	assertContains(t, out, "type MyResource `structural(protocol: false) `public `target(wasi) {")

	// enum → plain enum declaration, no `structural(protocol: false)
	assertContains(t, out, "enum MyEnum `public {")
	assertNotContains(t, out, "enum MyEnum `structural(protocol: false)")

	// variant → plain enum declaration, no `structural(protocol: false)
	assertContains(t, out, "enum MyVariant `public {")
	assertNotContains(t, out, "enum MyVariant `structural(protocol: false)")
}

// TestT1733CloseReadWriteMethodsPreservedNoRename verifies that method names
// reserved by standard protocol interfaces (close, read, write, next, clone) are
// preserved verbatim in the generated code when the enclosing type carries
// `structural(protocol: false) — no renaming, no compilation failure.
func TestT1733CloseReadWriteMethodsPreservedNoRename(t *testing.T) {
	src := `
interface stream {
    resource stream-handle {
        read: func(length: u64) -> list<u8>;
        write: func(bytes: list<u8>);
        next: func() -> option<u8>;
        clone: func() -> stream-handle;
        close: func();
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	out := GeneratePromise(modules, "wasi")

	// The type declaration carries the exemption annotation.
	assertContains(t, out, "type StreamHandle `structural(protocol: false) `public `target(wasi) {")

	// All protocol-reserved method names are present verbatim — no renaming.
	// Methods returning plain types (not result<>) are non-failable (no ! suffix).
	assertContains(t, out, "read(this, u64 length) u8[] `public {")
	assertContains(t, out, "write(this, u8[] bytes) `public {")
	assertContains(t, out, "next(this) u8? `public {")
	assertContains(t, out, "clone(this) StreamHandle `public {")
	assertContains(t, out, "close(this) `public {")

	// The extern call sites use the corresponding mangled extern names.
	assertContains(t, out, "_stream_handle_read(this._handle,")
	assertContains(t, out, "_stream_handle_write(this._handle,")
	assertContains(t, out, "_stream_handle_next(this._handle)")
	assertContains(t, out, "_stream_handle_clone(this._handle)")
	assertContains(t, out, "_stream_handle_close(this._handle)")
}

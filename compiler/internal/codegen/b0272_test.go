package codegen

import (
	"testing"
)

// B0272: Failable structural interface returns through vtable dispatch must use
// RTTI-based drop (emitStructuralInstanceDrop) to properly clean up instances
// with droppable fields (e.g., string fields). Without this, the instance leaks.

func TestB0272FailableUnwrapStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Readable `+"`"+`structural {
			read_data(int n) string `+"`"+`abstract;
		}
		type FailableDataSource `+"`"+`structural {
			open_source!(string name) Readable `+"`"+`abstract;
		}
		type MemReadable {
			string data;
			read_data(int n) string { return this.data; }
		}
		type FailableMemDataSource {
			open_source!(string name) MemReadable {
				return MemReadable(data: name);
			}
		}
		test!() {
			FailableDataSource s = FailableMemDataSource();
			Readable r = s.open_source("world")?!;
		}
	`)
	fn := extractFunction(ir, "__user.test")
	if fn == "" {
		t.Fatal("could not find @__user.test function")
	}
	// RTTI-based structural drop must be emitted for variable r
	assertContains(t, fn, "struct.drop")
}

func TestB0272FailableHandlerStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Readable `+"`"+`structural {
			read_data(int n) string `+"`"+`abstract;
		}
		type FailableDataSource `+"`"+`structural {
			open_source!(string name) Readable `+"`"+`abstract;
		}
		type MemReadable {
			string data;
			read_data(int n) string { return this.data; }
		}
		type FailableMemDataSource {
			open_source!(string name) MemReadable {
				return MemReadable(data: name);
			}
		}
		test!() {
			FailableDataSource s = FailableMemDataSource();
			Readable r = s.open_source("world") ? { MemReadable(data: "fallback") };
		}
	`)
	fn := extractFunction(ir, "__user.test")
	if fn == "" {
		t.Fatal("could not find @__user.test function")
	}
	// RTTI-based structural drop must be emitted even with ? {} handler wrapper
	assertContains(t, fn, "struct.drop")
}

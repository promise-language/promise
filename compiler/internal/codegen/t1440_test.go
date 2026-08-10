package codegen

import "testing"

// T1440: `for e in v.iter()` double-freed every heap element. Two independent
// defects, both guarded here.
//
// 1. analyzeEnvCaptureDrop excluded `this` captures only in its heap-user-type
//    and structural arms. Vector[T].iter()'s closure captures the BORROWED
//    `this` receiver, which fell into the Vector arm instead and mapped to
//    Vector.drop — so the closure's env_drop freed the caller's vector.
// 2. genForInCustomIter never dropped the owned element that next() hands back,
//    so every heap element leaked once defect 1 was fixed.
//
// `int[]` hid both: an all-constant int[] is a T0062 .rodata static vector
// (double-drop is a no-op) and `int` needs no element drop at all.

// --- Defect 1: the iter() closure must not drop its borrowed receiver ---

// Vector[string].iter() must store a null env-drop pointer: its only droppable
// capture candidate is the borrowed `this`, so no env_drop function should be
// generated at all. Pre-fix the body bitcast a `.lambda.Vector[string].N.env_drop`
// into env field 0, and that function called @Vector.drop on the caller's vector.
func TestT1440_VectorIterClosureHasNoEnvDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["ab"];
			for e in v.iter() {
			}
		}
	`)
	fn := extractDefine(ir, "Vector[string].iter")
	if fn == "" {
		t.Fatalf("Vector[string].iter not found in IR")
	}
	assertNotContains(t, fn, "env_drop")
	assertNotContains(t, fn, "@Vector.drop")
}

// Same guard for a Copy element type. The receiver is a Vector either way, so
// the `this` exclusion is about the RECEIVER, not the element — Vector[int].iter()
// was equally wrong pre-fix, it just happened to be freeing a .rodata vector.
func TestT1440_VectorIntIterClosureHasNoEnvDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2];
			for e in v.iter() {
			}
		}
	`)
	fn := extractDefine(ir, "Vector[int].iter")
	if fn == "" {
		t.Fatalf("Vector[int].iter not found in IR")
	}
	assertNotContains(t, fn, "env_drop")
	assertNotContains(t, fn, "@Vector.drop")
}

// Over-correction guard: the `this` exclusion must not disarm env_drop for
// ordinary move captures. A lambda that move-captures a local string still
// needs its env_drop to free that string.
func TestT1440_MovedStringCaptureStillDropped(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string s = "abc";
			f := move || -> int {
				return s.len;
			};
			int n = f();
		}
	`)
	envDrop := findEnvDropContaining(ir, "@promise_string_drop")
	if envDrop == "" {
		t.Fatal("expected an env_drop calling @promise_string_drop for a moved string capture")
	}
}

// --- Defect 2: the for-in element yielded by next() is owned and must drop ---

// The loop body must drop the string element on the fall-through path, or every
// element leaks. maybeRegisterDrop is registered at loopScopeDepth, so the drop
// lands between the body and the update block.
func TestT1440_ForInCustomIterDropsStringElement(t *testing.T) {
	ir := generateIR(t, `
		count(string[] v) int {
			int n = 0;
			for e in v.iter() {
				n = n + 1;
			}
			return n;
		}
		main() {
			string[] v = ["ab"];
			int n = count(v);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@promise_string_drop")
}

// A Vector element goes through the same binding and must call Vector.drop.
func TestT1440_ForInCustomIterDropsVectorElement(t *testing.T) {
	ir := generateIR(t, `
		count(int[][] v) int {
			int n = 0;
			for e in v.iter() {
				n = n + e.len;
			}
			return n;
		}
		main() {
			int[][] v = [[1, 2]];
			int n = count(v);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@Vector.drop")
}

// A heap user type with an explicit drop must have that drop called per element.
func TestT1440_ForInCustomIterDropsUserTypeElement(t *testing.T) {
	ir := generateIR(t, `
		type T1440R {
			int id;
			drop(~this) {}
		}
		count(T1440R[] v) int {
			int n = 0;
			for e in v.iter() {
				n = n + e.id;
			}
			return n;
		}
		main() {
			T1440R[] v = [T1440R(id: 1)];
			int n = count(v);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@T1440R.drop")
}

// break leaves the loop above the element's drop binding, so genBreakStmt's
// emitScopeCleanup(loopScopeDepth) must drop the in-flight element too. Without
// the binding sitting at loopScopeDepth this path leaks the current element.
func TestT1440_ForInCustomIterDropsElementOnBreak(t *testing.T) {
	ir := generateIR(t, `
		first(string[] v) int {
			for e in v.iter() {
				break;
			}
			return 0;
		}
		main() {
			string[] v = ["ab"];
			int n = first(v);
		}
	`)
	fn := extractDefine(ir, "__user.first")
	if fn == "" {
		t.Fatalf("first not found in IR")
	}
	assertContains(t, fn, "@promise_string_drop")
}

// Over-arming guard: a Copy element type needs no per-element drop, so the
// int[] loop must stay allocation-free. maybeRegisterDrop no-ops for `int`.
func TestT1440_ForInCustomIterIntElementNoDrop(t *testing.T) {
	ir := generateIR(t, `
		total(int[] v) int {
			int n = 0;
			for e in v.iter() {
				n = n + e;
			}
			return n;
		}
		main() {
			int[] v = [1, 2];
			int n = total(v);
		}
	`)
	fn := extractDefine(ir, "__user.total")
	if fn == "" {
		t.Fatalf("total not found in IR")
	}
	assertNotContains(t, fn, "@promise_string_drop")
	assertNotContains(t, fn, "@Vector.drop")
}

// The `this` exclusion is hoisted ahead of every branch, so the Vector arm is
// the one that has to keep working for ordinary captures: a move-captured local
// vector is owned by the closure and its env_drop must still call Vector.drop.
// This is the direct over-correction counterpart of
// TestT1440_VectorIterClosureHasNoEnvDrop — same arm, opposite expectation.
func TestT1440_MovedVectorCaptureStillDropped(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2];
			f := move || -> int {
				return v.len;
			};
			int n = f();
		}
	`)
	envDrop := findEnvDropContaining(ir, "@Vector.drop")
	if envDrop == "" {
		t.Fatal("expected an env_drop calling @Vector.drop for a moved vector capture")
	}
}

// Same guard one level up: a move-captured heap user type with an explicit drop
// still routes through the heap-user-type arm, which the early return now sits
// in front of.
func TestT1440_MovedUserTypeCaptureStillDropped(t *testing.T) {
	ir := generateIR(t, `
		type T1440C {
			int id;
			drop(~this) {}
		}
		main() {
			T1440C c = T1440C(id: 1);
			f := move || -> int {
				return c.id;
			};
			int n = f();
		}
	`)
	envDrop := findEnvDropContaining(ir, "@T1440C.drop")
	if envDrop == "" {
		t.Fatal("expected an env_drop calling the T1440C drop for a moved user-type capture")
	}
}

// Locking in the pre-existing behaviour the hoist inherited: a closure inside a
// method of a heap user type captures the borrowed receiver, and env_drop must
// not free it. Previously enforced by the name check inside the heap-user-type
// arm; now by the single early return.
func TestT1440_UserTypeThisCaptureNotDropped(t *testing.T) {
	ir := generateIR(t, `
		type T1440O {
			int id;
			drop(~this) {}
			run() int {
				f := move || -> int {
					return this.id;
				};
				return f();
			}
		}
		main() {
			T1440O o = T1440O(id: 1);
			int n = o.run();
		}
	`)
	if envDrop := findEnvDropContaining(ir, "@T1440O.drop"); envDrop != "" {
		t.Fatalf("borrowed `this` capture must not be dropped by env_drop, got:\n%s", envDrop)
	}
}

// An Optional element routes through maybeRegisterOptionalDrop rather than the
// plain string path, so it needs its own guard — `string?[]` was one of the
// crashing shapes in the report.
func TestT1440_ForInCustomIterDropsOptionalElement(t *testing.T) {
	ir := generateIR(t, `
		count(string?[] v) int {
			int n = 0;
			for e in v.iter() {
				n = n + 1;
			}
			return n;
		}
		main() {
			string?[] v = ["ab", none];
			int n = count(v);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@promise_string_drop")
}

// continue jumps to the update block from inside the body, so — like break — it
// goes through emitScopeCleanup(loopScopeDepth) and must drop the in-flight
// element. Distinct branch target from the break case above.
func TestT1440_ForInCustomIterDropsElementOnContinue(t *testing.T) {
	ir := generateIR(t, `
		count(string[] v) int {
			int n = 0;
			for e in v.iter() {
				if e.len == 2 {
					continue;
				}
				n = n + 1;
			}
			return n;
		}
		main() {
			string[] v = ["ab"];
			int n = count(v);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@promise_string_drop")
}

// A `_` binding registers no local, but maybeRegisterDrop is still called with
// the element type — the yielded element is owned either way. Without this the
// element leaks whenever the body ignores it.
func TestT1440_ForInCustomIterDropsWildcardElement(t *testing.T) {
	ir := generateIR(t, `
		count(string[] v) int {
			int n = 0;
			for _ in v.iter() {
				n = n + 1;
			}
			return n;
		}
		main() {
			string[] v = ["ab"];
			int n = count(v);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@promise_string_drop")
}

// Inside a monomorphized generic the element type only becomes concrete after
// c.typeSubst is applied to next()'s return type. maybeRegisterDrop is handed
// that substituted type, so the string instantiation must drop while the int
// instantiation must not — one generic body, two different answers.
func TestT1440_ForInCustomIterDropsElementInGenericContext(t *testing.T) {
	ir := generateIR(t, `
		gcount[T](T[] v) int {
			int n = 0;
			for e in v.iter() {
				n = n + 1;
			}
			return n;
		}
		main() {
			string[] sv = ["ab"];
			int[] iv = [1];
			int a = gcount[string](sv);
			int b = gcount[int](iv);
		}
	`)
	strFn := extractDefine(ir, "gcount[string]")
	if strFn == "" {
		t.Fatalf("gcount[string] not found in IR")
	}
	assertContains(t, strFn, "@promise_string_drop")

	intFn := extractDefine(ir, "gcount[int]")
	if intFn == "" {
		t.Fatalf("gcount[int] not found in IR")
	}
	assertNotContains(t, intFn, "@promise_string_drop")
}

// The indexed form allocates and increments an index around the body. The
// element drop is emitted between the body and the update block, so the two
// must not interfere — the index increment still has to run on the
// fall-through path after the drop.
func TestT1440_ForInCustomIterIndexedBindingDropsElement(t *testing.T) {
	ir := generateIR(t, `
		count(string[] v) int {
			int n = 0;
			for i, e in v.iter() {
				n = n + i + e.len;
			}
			return n;
		}
		main() {
			string[] v = ["ab"];
			int n = count(v);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@promise_string_drop")
	assertContains(t, fn, "iter.update")
}

// The other duck-typed for-in kind: sema.ForInIter (the iterable has iter(),
// not next()). genForInCustomStream calls .iter() and then delegates to
// genForInCustomIter, so it inherits the element drop — but through a separate
// entry point that the direct `v.iter()` tests never exercise.
func TestT1440_ForInCustomStreamDropsStringElement(t *testing.T) {
	ir := generateIR(t, `
		type T1440Bag {
			string[] items;
			iter() Iterator[string] {
				return this.items.iter();
			}
		}
		count(T1440Bag b) int {
			int n = 0;
			for e in b {
				n = n + 1;
			}
			return n;
		}
		main() {
			string[] items = ["ab"];
			T1440Bag b = T1440Bag(items: move items);
			int n = count(b);
		}
	`)
	fn := extractDefine(ir, "__user.count")
	if fn == "" {
		t.Fatalf("count not found in IR")
	}
	assertContains(t, fn, "@promise_string_drop")
}

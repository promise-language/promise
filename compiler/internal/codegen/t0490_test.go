package codegen

import (
	"strings"
	"testing"
)

// T0490: Slice assignment `vec[a:b] = src` for tuple-needs-drop and heap
// user-type element types must NOT call destructive Vector.drop(src) at the
// call site (B0313 skip), and must NOT clearDropFlag — the [:]= body dups
// elements on read (T0398/T0412), so src retains independent ownership and
// needs normal scope cleanup. Mirrors T0386's string-element carve-out.

func TestT0490SliceAssignTupleSkipsB0313(t *testing.T) {
	ir := generateIR(t, `
		main() {
			src := (string, int)[]();
			src.push(("hello" + "", 1));
			v := (string, int)[]();
			v.push(("a" + "", 9));
			v[0:1] = src;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[(string, int)].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[(string, int)].[:]=\n%s", mainFn)
	}
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "%src.dropflag") {
		t.Errorf("expected src.dropflag to be read after [:]= for scope cleanup — skipB0313 must not disarm src for tuple-needs-drop\nmain after [:]=:\n%s", rest)
	}
}

func TestT0490SliceAssignHeapUserSkipsB0313(t *testing.T) {
	ir := generateIR(t, `
		type Box_t0490 { string name; drop(~this) {} }
		main() {
			src := Box_t0490[]();
			src.push(Box_t0490(name: "a" + ""));
			v := Box_t0490[]();
			v.push(Box_t0490(name: "x" + ""));
			v[0:1] = src;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[Box_t0490].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[Box_t0490].[:]=\n%s", mainFn)
	}
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "%src.dropflag") {
		t.Errorf("expected src.dropflag to be read after [:]= for scope cleanup — skipB0313 must not disarm src for heap user-type\nmain after [:]=:\n%s", rest)
	}
}

// Negative regression: pure-value tuple element type still takes the B0313
// destructive path because the [:]= body aliases-on-read (no dup flag fires
// in stmt.go:4894-4903 for value tuples).
func TestT0490ValueTupleSliceAssignKeepsB0313(t *testing.T) {
	ir := generateIR(t, `
		main() {
			src := (int, int)[]();
			src.push((1, 2));
			v := (int, int)[]();
			v.push((9, 9));
			v[0:1] = src;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[(int, int)].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[(int, int)].[:]=\n%s", mainFn)
	}
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "call void @Vector.drop") {
		t.Errorf("expected destructive Vector.drop after [:]= for value-tuple element — skipB0313 must NOT fire when [:]= aliases\nmain after [:]=:\n%s", rest)
	}
}

// Negative regression: Vector[Vector[T]] element type — inner vector is aliased
// on read inside [:]= (no dup-flag fires for plain Vector elements at stmt.go:
// 4894-4903), so the destructive B0313 path is correct. The T0490 fix must NOT
// accidentally trigger skipB0313 here.
func TestT0490InnerVectorSliceAssignKeepsB0313(t *testing.T) {
	ir := generateIR(t, `
		main() {
			src := Vector[string[]]();
			a := string[]();   a.push("a" + "");
			src.push(a);
			v := Vector[string[]]();
			b := string[]();   b.push("b" + "");
			v.push(b);
			v[0:1] = src;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[Vector[string]].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[Vector[string]].[:]=\n%s", mainFn)
	}
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "call void @Vector.drop") {
		t.Errorf("expected destructive Vector.drop after [:]= for Vector[Vector[T]] element — skipB0313 must NOT fire when [:]= aliases\nmain after [:]=:\n%s", rest)
	}
}

// Negative regression: Vector[Channel[T]] element type — channel element is
// aliased on read inside [:]=, B0313 destructive path applies.
func TestT0490ChannelSliceAssignKeepsB0313(t *testing.T) {
	ir := generateIR(t, `
		main() {
			src := Vector[Channel[int]]();
			ch := Channel[int](1);
			src.push(ch);
			v := Vector[Channel[int]]();
			ch2 := Channel[int](1);
			v.push(ch2);
			v[0:1] = src;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[Channel[int]].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[Channel[int]].[:]=\n%s", mainFn)
	}
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "call void @Vector.drop") {
		t.Errorf("expected destructive Vector.drop after [:]= for Vector[Channel[T]] element — skipB0313 must NOT fire when [:]= aliases\nmain after [:]=:\n%s", rest)
	}
}

// Positive: synthesized-drop heap user-type element (no explicit drop method
// but contains a droppable string field → NeedsSynthDrop()=true). Exercises
// the second predicate inside isDroppableHeapUserType (stmt.go:3813).
func TestT0490SliceAssignSynthDropHeapUserSkipsB0313(t *testing.T) {
	ir := generateIR(t, `
		type SynthBox_t0490 { string name; }
		main() {
			src := SynthBox_t0490[]();
			src.push(SynthBox_t0490(name: "a" + ""));
			v := SynthBox_t0490[]();
			v.push(SynthBox_t0490(name: "x" + ""));
			v[0:1] = src;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[SynthBox_t0490].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[SynthBox_t0490].[:]=\n%s", mainFn)
	}
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "%src.dropflag") {
		t.Errorf("expected src.dropflag to be read after [:]= for scope cleanup — skipB0313 must fire for synth-drop heap user-type element\nmain after [:]=:\n%s", rest)
	}
}

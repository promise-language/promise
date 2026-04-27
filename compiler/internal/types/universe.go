package types

// Universe is the outermost scope containing all built-in types.
var Universe *Scope

// Predeclared Named types for built-in primitives.
// These are empty stubs — sema populates them with fields/methods from stdlib.
var (
	TypInt    *Named
	TypI8     *Named
	TypI16    *Named
	TypI32    *Named
	TypI64    *Named
	TypUint   *Named
	TypU8     *Named
	TypU16    *Named
	TypU32    *Named
	TypU64    *Named
	TypF32    *Named
	TypF64    *Named
	TypBool   *Named
	TypChar   *Named
	TypString *Named
	TypVoid   *Named
	TypNone   *Named
	TypError  *Named

	// Generic stdlib types
	TypTask    *Named // task[T] — concurrency handle from go expressions
	TypChannel *Named // channel[T] — channel type
	TypIter    *Named // iter[T] — synchronous iterator interface
	TypStream  *Named // stream[T] — asynchronous iterator interface
	TypVector  *Named // Vector[T] — dynamic array
	TypMap     *Named // map[K, V] — map container type
	TypRange   *Named // range — integer range from .. and ..= operators
)

func init() {
	Universe = NewScope(nil, Pos{}, Pos{}, "universe")

	defNamed := func(name string) *Named {
		tn := NewTypeName(Pos{}, name, nil)
		n := NewNamed(tn, nil)
		Universe.Insert(tn)
		return n
	}

	defGeneric := func(name string, paramNames ...string) *Named {
		tn := NewTypeName(Pos{}, name, nil)
		params := make([]*TypeParam, len(paramNames))
		for i, pn := range paramNames {
			ptn := NewTypeName(Pos{}, pn, nil)
			params[i] = NewTypeParam(ptn, nil, i)
		}
		n := NewNamed(tn, params)
		Universe.Insert(tn)
		return n
	}

	TypInt = defNamed("int")
	TypI8 = defNamed("i8")
	TypI16 = defNamed("i16")
	TypI32 = defNamed("i32")
	TypI64 = defNamed("i64")
	TypUint = defNamed("uint")
	TypU8 = defNamed("u8")
	TypU16 = defNamed("u16")
	TypU32 = defNamed("u32")
	TypU64 = defNamed("u64")
	TypF32 = defNamed("f32")
	TypF64 = defNamed("f64")
	TypBool = defNamed("bool")
	TypChar = defNamed("char")
	TypString = defNamed("string")
	TypVoid = defNamed("void")
	TypNone = defNamed("none")
	TypError = defNamed("error")

	// Generic stdlib types
	TypTask = defGeneric("task", "T")
	TypChannel = defGeneric("channel", "T")
	TypIter = defGeneric("iter", "T")
	TypStream = defGeneric("stream", "T")
	TypVector = defGeneric("Vector", "T")
	TypMap = defGeneric("map", "K", "V")

	TypRange = defNamed("range")
}

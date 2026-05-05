package types

// Universe is the outermost scope containing all built-in types.
var Universe *Scope

// Predeclared Named types for built-in primitives (native types).
// These are empty stubs in the Universe scope — sema populates them with fields/methods from stdlib.
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

	// Generic native types (in Universe scope)
	TypTask    *Named // Task[T] — concurrency handle from go expressions
	TypChannel *Named // Channel[T] — channel type
	TypVector  *Named // Vector[T] — dynamic array
)

// Non-native stdlib types — NOT in the Universe scope. These are regular types
// declared by the std module. The global pointers are populated by sema's
// populateUniverseTypes() hook after std's declare pass, providing identity
// for compiler features (error handling, for-in, range operators, map sugar).
var (
	TypError  *Named // error — base error type for failable functions
	TypIter   *Named // Iterator[T] — synchronous iterator interface
	TypStream *Named // Stream[T] — asynchronous iterator interface
	TypMap    *Named // Map[K, V] — map container type
	TypRange  *Named // Range[T] — generic range from .. and ..= operators
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

	// Generic native types
	TypTask = defGeneric("Task", "T")
	TypChannel = defGeneric("Channel", "T")
	TypVector = defGeneric("Vector", "T")

	// Lowercase sugar aliases for native generic types
	defAlias := func(alias string, target *Named) {
		tn := NewTypeName(Pos{}, alias, target.Obj().Type())
		Universe.Insert(tn)
	}
	defAlias("task", TypTask)
	defAlias("channel", TypChannel)

	// Non-native types (error, Map, Range, Iterator, Stream) and their aliases
	// (map, iter, stream) are NOT registered here. They are regular types declared
	// by the std module and populated via sema.populateUniverseTypes() after
	// std's declare pass.
}

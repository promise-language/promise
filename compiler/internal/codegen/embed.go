package codegen

import (
	"fmt"
	"math"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// defineEmbedGetter generates the body of a module-level getter that returns
// compile-time embedded file contents. For string embeds, returns a runtime
// string wrapping a global constant. For u8[] embeds, allocates a fresh vector
// and copies the global data into it.
func (c *Compiler) defineEmbedGetter(fd *ast.FuncDecl, fn *ir.Func, embed *sema.EmbedInfo) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.stmtTemps = nil                         // T0073
	c.stmtTempMap = make(map[value.Value]int) // T0073
	c.tempTrackingEnabled = false             // T0073
	c.blockCounter = 0

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	switch embed.Kind {
	case sema.EmbedString:
		c.defineEmbedStringGetter(fn, entry, embed)
	case sema.EmbedBytes:
		c.defineEmbedBytesGetter(fn, entry, embed)
	case sema.EmbedDir:
		c.defineEmbedDirGetter(fn, embed)
	}
}

// defineEmbedStringGetter emits a getter body that returns a string from embedded data.
// Creates a global constant and wraps it with promise_string_new.
func (c *Compiler) defineEmbedStringGetter(fn *ir.Func, entry *ir.Block, embed *sema.EmbedInfo) {
	s := string(embed.Data)
	data := constant.NewCharArrayFromString(s)

	globalName := fmt.Sprintf(".embed.%d", c.strCounter)
	c.strCounter++

	global := c.module.NewGlobalDef(globalName, data)
	global.Immutable = true
	global.Linkage = enum.LinkagePrivate

	ptr := entry.NewGetElementPtr(global.ContentType, global,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	result := entry.NewCall(c.funcs["promise_string_new"],
		ptr, constant.NewInt(irtypes.I64, int64(len(s))))
	entry.NewRet(result)
}

// defineEmbedBytesGetter emits a getter body that returns a u8[] (Vector[u8])
// from embedded data. Allocates a fresh vector on each call and copies data in.
func (c *Compiler) defineEmbedBytesGetter(fn *ir.Func, entry *ir.Block, embed *sema.EmbedInfo) {
	dataLen := int64(len(embed.Data))

	// Create global constant for the byte data
	data := constant.NewCharArrayFromString(string(embed.Data))
	globalName := fmt.Sprintf(".embed.%d", c.strCounter)
	c.strCounter++

	global := c.module.NewGlobalDef(globalName, data)
	global.Immutable = true
	global.Linkage = enum.LinkagePrivate

	// Allocate vector: header (16 bytes) + data
	headerSize := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))
	dataSize := constant.NewInt(irtypes.I64, dataLen)
	totalSize := entry.NewAdd(headerSize, dataSize)

	rawPtr := entry.NewCall(c.palAlloc, totalSize)

	// Check for OOM
	isNull := entry.NewICmp(enum.IPredEQ, rawPtr, constant.NewNull(irtypes.I8Ptr))
	oomBlk := fn.NewBlock("oom")
	initBlk := fn.NewBlock("init")
	entry.NewCondBr(isNull, oomBlk, initBlk)

	// OOM path: panic
	panicGlobal := c.getCStrGlobal("out of memory")
	msgPtr := oomBlk.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	retType := fn.Sig.RetType
	if _, isVoid := retType.(*irtypes.VoidType); isVoid {
		oomBlk.NewRet(nil)
	} else {
		oomBlk.NewRet(c.zeroValue(retType))
	}

	// Init path: set header (len, cap) and copy data
	headerType := vectorHeaderType()
	hdrPtr := initBlk.NewBitCast(rawPtr, irtypes.NewPointer(headerType))
	lenPtr := initBlk.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initBlk.NewStore(dataSize, lenPtr)
	capPtr := initBlk.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initBlk.NewStore(dataSize, capPtr)

	// Copy embedded data after header
	if dataLen > 0 {
		dataDst := initBlk.NewGetElementPtr(irtypes.I8, rawPtr, headerSize)
		dataSrc := initBlk.NewGetElementPtr(global.ContentType, global,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		initBlk.NewCall(c.funcs["llvm.memcpy"], dataDst, dataSrc, dataSize, constant.False)
	}

	initBlk.NewRet(rawPtr)
}

// defineEmbedDirGetter emits a getter body that returns an EmbeddedFiles value
// from a compile-time embedded directory tree (T0031). Constructs:
// - u8[] data blob (concatenated file contents)
// - int[] offsets and int[] sizes (parallel arrays for data indexing)
// - EmbeddedFile[] entries (one per file/directory)
// - EmbeddedFiles wrapping everything
func (c *Compiler) defineEmbedDirGetter(fn *ir.Func, embed *sema.EmbedInfo) {
	n := len(embed.DirEntries)

	// Layouts for the two types
	fileLayout := c.layouts[types.TypEmbeddedFile]
	filesLayout := c.layouts[types.TypEmbeddedFiles]
	if fileLayout == nil || filesLayout == nil {
		panic("codegen: EmbeddedFile/EmbeddedFiles layouts not computed — missing std/embed.pr?")
	}

	// --- Create global constants ---

	// 1. Data blob: [M x i8] containing concatenated file contents
	dataLen := int64(len(embed.Data))
	var dataGlobal *ir.Global
	if dataLen > 0 {
		dataConst := constant.NewCharArrayFromString(string(embed.Data))
		dataGlobalName := fmt.Sprintf(".embed_dir.data.%d", c.strCounter)
		c.strCounter++
		dataGlobal = c.module.NewGlobalDef(dataGlobalName, dataConst)
		dataGlobal.Immutable = true
		dataGlobal.Linkage = enum.LinkagePrivate
	}

	// 2. Offsets and sizes as [N x i64] globals
	offsetConsts := make([]constant.Constant, n)
	sizeConsts := make([]constant.Constant, n)
	for i, e := range embed.DirEntries {
		offsetConsts[i] = constant.NewInt(irtypes.I64, e.Offset)
		sizeConsts[i] = constant.NewInt(irtypes.I64, e.Size)
	}
	var offsetsGlobal, sizesGlobal *ir.Global
	if n > 0 {
		offsetArr := constant.NewArray(irtypes.NewArray(uint64(n), irtypes.I64), offsetConsts...)
		oName := fmt.Sprintf(".embed_dir.offsets.%d", c.strCounter)
		c.strCounter++
		offsetsGlobal = c.module.NewGlobalDef(oName, offsetArr)
		offsetsGlobal.Immutable = true
		offsetsGlobal.Linkage = enum.LinkagePrivate

		sizeArr := constant.NewArray(irtypes.NewArray(uint64(n), irtypes.I64), sizeConsts...)
		sName := fmt.Sprintf(".embed_dir.sizes.%d", c.strCounter)
		c.strCounter++
		sizesGlobal = c.module.NewGlobalDef(sName, sizeArr)
		sizesGlobal.Immutable = true
		sizesGlobal.Linkage = enum.LinkagePrivate
	}

	// 3. Path and name as .rodata string instance globals (T0060 format)
	// Layout: { i8* _variant, i64 len|bit63, [N x i8] data }
	// Bit 63 set on len marks these as literal strings — promise_string_drop is a no-op.
	pathGlobals := make([]*ir.Global, n)
	nameGlobals := make([]*ir.Global, n)
	for i, e := range embed.DirEntries {
		pLen := len(e.Path)
		pType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64, irtypes.NewArray(uint64(pLen), irtypes.I8))
		pInit := constant.NewStruct(pType,
			constant.NewNull(irtypes.I8Ptr),
			constant.NewInt(irtypes.I64, int64(pLen)|math.MinInt64),
			constant.NewCharArrayFromString(e.Path),
		)
		pName := fmt.Sprintf(".embed_dir.path.%d.%d", c.strCounter, i)
		pg := c.module.NewGlobalDef(pName, pInit)
		pg.Immutable = true
		pg.Linkage = enum.LinkagePrivate
		pathGlobals[i] = pg

		nLen := len(e.Name)
		nType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64, irtypes.NewArray(uint64(nLen), irtypes.I8))
		nInit := constant.NewStruct(nType,
			constant.NewNull(irtypes.I8Ptr),
			constant.NewInt(irtypes.I64, int64(nLen)|math.MinInt64),
			constant.NewCharArrayFromString(e.Name),
		)
		nName := fmt.Sprintf(".embed_dir.name.%d.%d", c.strCounter, i)
		ng := c.module.NewGlobalDef(nName, nInit)
		ng.Immutable = true
		ng.Linkage = enum.LinkagePrivate
		nameGlobals[i] = ng
	}
	c.strCounter++

	// --- Helper: allocate a vector with known data from a global, returns i8* ---
	allocVecFromGlobal := func(blk *ir.Block, elemSize int64, count int, global *ir.Global) (value.Value, *ir.Block) {
		headerSizeVal := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))
		dataSizeVal := constant.NewInt(irtypes.I64, int64(count)*elemSize)
		totalSizeVal := blk.NewAdd(headerSizeVal, dataSizeVal)

		rawPtr := blk.NewCall(c.palAlloc, totalSizeVal)
		isNull := blk.NewICmp(enum.IPredEQ, rawPtr, constant.NewNull(irtypes.I8Ptr))
		oomBlk := fn.NewBlock(fmt.Sprintf("oom.%d", c.blockCounter))
		c.blockCounter++
		okBlk := fn.NewBlock(fmt.Sprintf("vec.init.%d", c.blockCounter))
		c.blockCounter++
		blk.NewCondBr(isNull, oomBlk, okBlk)

		panicGlobal := c.getCStrGlobal("out of memory")
		msgPtr := oomBlk.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
		oomBlk.NewRet(c.zeroValue(fn.Sig.RetType))

		// Set header: len and cap
		headerType := vectorHeaderType()
		hdrPtr := okBlk.NewBitCast(rawPtr, irtypes.NewPointer(headerType))
		lenPtr := okBlk.NewGetElementPtr(headerType, hdrPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		okBlk.NewStore(constant.NewInt(irtypes.I64, int64(count)), lenPtr)
		capPtr := okBlk.NewGetElementPtr(headerType, hdrPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		okBlk.NewStore(constant.NewInt(irtypes.I64, int64(count)), capPtr)

		// Copy data from global
		if count > 0 && global != nil {
			dataDst := okBlk.NewGetElementPtr(irtypes.I8, rawPtr, headerSizeVal)
			dataSrc := okBlk.NewGetElementPtr(global.ContentType, global,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			okBlk.NewCall(c.funcs["llvm.memcpy"], dataDst, dataSrc, dataSizeVal, constant.False)
		}
		return rawPtr, okBlk
	}

	// --- Helper: allocate empty vector, returns i8* ---
	allocEmptyVec := func(blk *ir.Block) value.Value {
		// Empty vector: just header with len=0, cap=0
		headerSizeVal := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))
		rawPtr := blk.NewCall(c.palAlloc, headerSizeVal)
		// No OOM check for tiny alloc (matches existing pattern for empty vectors)
		headerType := vectorHeaderType()
		hdrPtr := blk.NewBitCast(rawPtr, irtypes.NewPointer(headerType))
		lenPtr := blk.NewGetElementPtr(headerType, hdrPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		blk.NewStore(constant.NewInt(irtypes.I64, 0), lenPtr)
		capPtr := blk.NewGetElementPtr(headerType, hdrPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		blk.NewStore(constant.NewInt(irtypes.I64, 0), capPtr)
		return rawPtr
	}

	// --- Helper: allocate a user type instance ---
	allocInstance := func(blk *ir.Block, layout *TypeDeclLayout, named *types.Named) (value.Value, value.Value) {
		// Returns (rawPtr i8*, typedPtr T_i*)
		instType := layout.Instance.LLVMType
		instPtrType := layout.InstancePtrType
		nullPtr := constant.NewNull(instPtrType)
		sizePtr := blk.NewGetElementPtr(instType, nullPtr, constant.NewInt(irtypes.I32, 1))
		sizeRaw := blk.NewPtrToInt(sizePtr, c.ptrIntType())
		var size value.Value = sizeRaw
		if c.isWasm {
			size = blk.NewZExt(sizeRaw, irtypes.I64)
		}
		rawPtr := blk.NewCall(c.palAlloc, size)
		typedPtr := blk.NewBitCast(rawPtr, instPtrType)

		// Set _variant (RTTI)
		variantFieldPtr := blk.NewGetElementPtr(instType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
		if tiGlobal := c.typeInfoGlobals[named]; tiGlobal != nil {
			blk.NewStore(blk.NewBitCast(tiGlobal, variantPtrType), variantFieldPtr)
		} else {
			blk.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
		}
		return rawPtr, typedPtr
	}

	// --- Helper: build value struct { vtable_ptr, instance_ptr } ---
	buildValueStruct := func(blk *ir.Block, named *types.Named, rawPtr value.Value) value.Value {
		var vtablePtr value.Value
		if vtGlobal := c.vtableGlobals[named]; vtGlobal != nil {
			vtablePtr = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
		} else {
			vtablePtr = constant.NewNull(irtypes.I8Ptr)
		}
		val := blk.NewInsertValue(constant.NewUndef(userValueType()), vtablePtr, 0)
		val = blk.NewInsertValue(val, rawPtr, 1)
		return val
	}

	curBlock := c.block

	// --- Step 1: Allocate u8[] data blob ---
	var dataVec value.Value
	if dataLen > 0 {
		var nextBlk *ir.Block
		dataVec, nextBlk = allocVecFromGlobal(curBlock, 1, int(dataLen), dataGlobal)
		curBlock = nextBlk
	} else {
		dataVec = allocEmptyVec(curBlock)
	}

	// --- Step 2: Allocate int[] offsets ---
	var offsetsVec value.Value
	if n > 0 {
		var nextBlk *ir.Block
		offsetsVec, nextBlk = allocVecFromGlobal(curBlock, 8, n, offsetsGlobal)
		curBlock = nextBlk
	} else {
		offsetsVec = allocEmptyVec(curBlock)
	}

	// --- Step 3: Allocate int[] sizes ---
	var sizesVec value.Value
	if n > 0 {
		var nextBlk *ir.Block
		sizesVec, nextBlk = allocVecFromGlobal(curBlock, 8, n, sizesGlobal)
		curBlock = nextBlk
	} else {
		sizesVec = allocEmptyVec(curBlock)
	}

	// --- Step 4: Construct EmbeddedFile instances and vector ---
	var entriesVec value.Value
	if n > 0 {
		elemLLVM := userValueType()
		elemSize := int64(c.typeSize(elemLLVM))

		// Allocate entries vector: header + n * elemSize
		headerSizeVal := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))
		dataSizeVal := constant.NewInt(irtypes.I64, int64(n)*elemSize)
		totalSizeVal := curBlock.NewAdd(headerSizeVal, dataSizeVal)

		entriesRaw := curBlock.NewCall(c.palAlloc, totalSizeVal)
		isNull := curBlock.NewICmp(enum.IPredEQ, entriesRaw, constant.NewNull(irtypes.I8Ptr))
		oomBlk := fn.NewBlock(fmt.Sprintf("oom.entries.%d", c.blockCounter))
		c.blockCounter++
		okBlk := fn.NewBlock(fmt.Sprintf("entries.init.%d", c.blockCounter))
		c.blockCounter++
		curBlock.NewCondBr(isNull, oomBlk, okBlk)

		panicGlobal := c.getCStrGlobal("out of memory")
		msgPtr := oomBlk.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
		oomBlk.NewRet(c.zeroValue(fn.Sig.RetType))

		// Set header
		headerType := vectorHeaderType()
		hdrPtr := okBlk.NewBitCast(entriesRaw, irtypes.NewPointer(headerType))
		lenPtr := okBlk.NewGetElementPtr(headerType, hdrPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		okBlk.NewStore(constant.NewInt(irtypes.I64, int64(n)), lenPtr)
		capPtr := okBlk.NewGetElementPtr(headerType, hdrPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		okBlk.NewStore(constant.NewInt(irtypes.I64, int64(n)), capPtr)

		// Typed pointer to data area
		dataBase := okBlk.NewGetElementPtr(irtypes.I8, entriesRaw, headerSizeVal)
		dataTypedPtr := okBlk.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

		curBlock = okBlk

		// Create each EmbeddedFile and store in vector
		fileInstType := fileLayout.Instance.LLVMType
		for i, e := range embed.DirEntries {
			// Bitcast .rodata string instance globals to i8* (zero-alloc literal strings)
			pathStr := curBlock.NewBitCast(pathGlobals[i], irtypes.I8Ptr)
			nameStr := curBlock.NewBitCast(nameGlobals[i], irtypes.I8Ptr)

			// Allocate EmbeddedFile instance
			fileRaw, fileTyped := allocInstance(curBlock, fileLayout, types.TypEmbeddedFile)
			_ = fileRaw

			// Set fields: _name, _path, _size, _is_dir
			nameIdx := fileLayout.InstanceFieldIndex["_name"]
			nameFieldPtr := curBlock.NewGetElementPtr(fileInstType, fileTyped,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(nameIdx)))
			curBlock.NewStore(nameStr, nameFieldPtr)

			pathIdx := fileLayout.InstanceFieldIndex["_path"]
			pathFieldPtr := curBlock.NewGetElementPtr(fileInstType, fileTyped,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pathIdx)))
			curBlock.NewStore(pathStr, pathFieldPtr)

			sizeIdx := fileLayout.InstanceFieldIndex["_size"]
			sizeFieldPtr := curBlock.NewGetElementPtr(fileInstType, fileTyped,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(sizeIdx)))
			curBlock.NewStore(constant.NewInt(irtypes.I64, e.Size), sizeFieldPtr)

			isDirIdx := fileLayout.InstanceFieldIndex["_is_dir"]
			isDirFieldPtr := curBlock.NewGetElementPtr(fileInstType, fileTyped,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(isDirIdx)))
			var isDirVal constant.Constant
			if e.IsDir {
				isDirVal = constant.True
			} else {
				isDirVal = constant.False
			}
			curBlock.NewStore(isDirVal, isDirFieldPtr)

			// Build value struct and store in entries vector
			fileVal := buildValueStruct(curBlock, types.TypEmbeddedFile, fileRaw)
			elemPtr := curBlock.NewGetElementPtr(elemLLVM, dataTypedPtr,
				constant.NewInt(irtypes.I64, int64(i)))
			curBlock.NewStore(fileVal, elemPtr)
		}

		entriesVec = entriesRaw
	} else {
		entriesVec = allocEmptyVec(curBlock)
	}

	// --- Step 5: Construct EmbeddedFiles instance ---
	filesRaw, filesTyped := allocInstance(curBlock, filesLayout, types.TypEmbeddedFiles)
	filesInstType := filesLayout.Instance.LLVMType

	// Set _entries field
	entriesIdx := filesLayout.InstanceFieldIndex["_entries"]
	entriesFieldPtr := curBlock.NewGetElementPtr(filesInstType, filesTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(entriesIdx)))
	curBlock.NewStore(entriesVec, entriesFieldPtr)

	// Set _data field
	dataIdx := filesLayout.InstanceFieldIndex["_data"]
	dataFieldPtr := curBlock.NewGetElementPtr(filesInstType, filesTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(dataIdx)))
	curBlock.NewStore(dataVec, dataFieldPtr)

	// Set _offsets field
	offsetsIdx := filesLayout.InstanceFieldIndex["_offsets"]
	offsetsFieldPtr := curBlock.NewGetElementPtr(filesInstType, filesTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(offsetsIdx)))
	curBlock.NewStore(offsetsVec, offsetsFieldPtr)

	// Set _sizes field
	sizesIdx := filesLayout.InstanceFieldIndex["_sizes"]
	sizesFieldPtr := curBlock.NewGetElementPtr(filesInstType, filesTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(sizesIdx)))
	curBlock.NewStore(sizesVec, sizesFieldPtr)

	// Build EmbeddedFiles value struct and return
	result := buildValueStruct(curBlock, types.TypEmbeddedFiles, filesRaw)
	curBlock.NewRet(result)
}

package codegen

import (
	"fmt"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
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
	oomBlk.NewUnreachable()

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

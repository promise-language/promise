package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/codegen/pal"
)

// defineCryptoPALBodies adds LLVM IR function bodies to crypto extern declarations
// from modules/crypto/random.pr. Lazily emits the PAL function only when the
// crypto module is imported.
//
// Must run after compileModules() so that crypto externs are declared in c.module.Funcs.
func (c *Compiler) defineCryptoPALBodies() {
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	fn, ok := irFuncByName["promise_crypto_random_fill"]
	if !ok {
		return // crypto module not imported or random_bytes not used
	}

	// Lazily emit PAL function
	p := pal.ForTarget(c.module.TargetTriple)
	c.palCryptoRandomBytes = p.EmitCryptoRandomBytes(c.module)

	c.defineCryptoRandomFillBody(fn)
}

// defineCryptoRandomFillBody: void @promise_crypto_random_fill(i8* sret, i8* ~buf)
// Calls the PAL CSPRNG to fill the u8[] buffer with random bytes.
// Returns 0 on success, -1 on error (as a Promise int via sret).
func (c *Compiler) defineCryptoRandomFillBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	// fn.Params[1] is u8[] vector (~buf — moved ownership, LLVM type is i8*)
	vecPtr := fn.Params[1]
	dataPtr, dataLen := extractVectorDataLen(entry, vecPtr)

	// Call PAL: i32 @pal_crypto_random_bytes(i8* buf, i64 len)
	rc := entry.NewCall(c.palCryptoRandomBytes, dataPtr, dataLen)
	rcI64 := entry.NewSExt(rc, irtypes.I64)

	// Return rc as Promise int (0 = success, -1 = error)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

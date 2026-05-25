package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
)

// CoverageRegion describes a source code region tracked for test coverage.
// Each region gets a global i64 counter that is incremented on execution.
type CoverageRegion struct {
	File      string // source file path
	StartLine int    // 1-based
	EndLine   int    // 1-based
	FuncName  string // enclosing function or method name
	Kind      string // "function", "method", "if.then", "if.else", "while.body", "for.body", "loop.body", "match.arm"
}

// addCoverageRegion registers a new coverage region and creates its global counter.
// Returns the index of the new region (for emitCoverageIncrement).
func (c *Compiler) addCoverageRegion(file string, startLine, endLine int, funcName, kind string) int {
	idx := len(c.coverageRegions)
	c.coverageRegions = append(c.coverageRegions, CoverageRegion{
		File:      file,
		StartLine: startLine,
		EndLine:   endLine,
		FuncName:  funcName,
		Kind:      kind,
	})

	// Default (LinkageNone) linkage — serialized as an externally-visible
	// definition (`@__promise_cov_N = global i64 0`). This is essential for
	// generic-method coverage (T0574): a monomorphized method's body lives in
	// a per-instance .bc while the reporter reads the counter from the main
	// IR's test main. stripGlobals externalizes non-private globals in every
	// split IR, so the increment (instance .bc) and the reporter read (main
	// IR) resolve to the single definition kept in the main IR. Private
	// linkage would instead produce independent per-translation-unit copies,
	// so the always-zero main copy would be read → "not covered".
	g := c.module.NewGlobalDef(fmt.Sprintf("__promise_cov_%d", idx), constant.NewInt(irtypes.I64, 0))
	c.coverageGlobals = append(c.coverageGlobals, g)
	return idx
}

// emitCoverageIncrement emits a load+add+store sequence to increment the
// coverage counter at the given index. Call this at the start of each
// instrumented region.
func (c *Compiler) emitCoverageIncrement(idx int) {
	if idx >= len(c.coverageGlobals) {
		return
	}
	g := c.coverageGlobals[idx]
	val := c.block.NewLoad(irtypes.I64, g)
	inc := c.block.NewAdd(val, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(inc, g)
}

// shouldInstrument returns true if the current codegen context should be
// instrumented for coverage. Filters out module code, test functions, and
// generated code.
func (c *Compiler) shouldInstrument() bool {
	if !c.coverageEnabled {
		return false
	}
	// Only instrument user code (not modules/std)
	if c.compilingModule != "" {
		return false
	}
	return true
}

// isTestFunc returns true if the named function is a test function.
func (c *Compiler) isTestFunc(name string) bool {
	for _, t := range c.info.Tests {
		if t.Name() == name {
			return true
		}
	}
	return false
}

// currentCoverageFuncName returns the display name for the function currently
// being compiled. For methods, includes the owner type name.
func (c *Compiler) currentCoverageFuncName() string {
	if c.fn == nil {
		return ""
	}
	return c.fn.GlobalName
}

// emitCoverageOutput generates the coverage data output section in the test
// main function. After tests complete, prints counter values as text lines
// delimited by marker strings, so the Go test runner can parse them.
func (c *Compiler) emitCoverageOutput(block *ir.Block, _ *ir.Func) *ir.Block {
	if len(c.coverageRegions) == 0 {
		return block
	}

	stdout := constant.NewInt(irtypes.I32, 1)

	// Print marker: "===PROMISE_COV===\n"
	markerData := constant.NewCharArrayFromString("===PROMISE_COV===\n")
	markerGlobal := c.module.NewGlobalDef(".str.cov_marker_start", markerData)
	markerGlobal.Immutable = true
	markerPtr := block.NewGetElementPtr(markerGlobal.ContentType, markerGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewCall(c.palWrite, stdout, markerPtr, constant.NewInt(irtypes.I64, int64(len("===PROMISE_COV===\n"))))

	// For each counter: load value, convert to string, print, free
	nlPtr := block.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	for i := range c.coverageRegions {
		g := c.coverageGlobals[i]
		val := block.NewLoad(irtypes.I64, g)
		str := block.NewCall(c.funcs["promise_int_to_string"], val)
		dataPtr, dataLen := c.extractStringDataLenFromInstance(block, str)
		block.NewCall(c.palWrite, stdout, dataPtr, dataLen)
		block.NewCall(c.palFree, str)
		block.NewCall(c.palWrite, stdout, nlPtr, constant.NewInt(irtypes.I64, 1))
	}

	// Print end marker: "===END_COV===\n"
	endMarkerData := constant.NewCharArrayFromString("===END_COV===\n")
	endMarkerGlobal := c.module.NewGlobalDef(".str.cov_marker_end", endMarkerData)
	endMarkerGlobal.Immutable = true
	endMarkerPtr := block.NewGetElementPtr(endMarkerGlobal.ContentType, endMarkerGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewCall(c.palWrite, stdout, endMarkerPtr, constant.NewInt(irtypes.I64, int64(len("===END_COV===\n"))))

	return block
}

package astcache

const (
	tagNil = 0

	// Expressions (1-40)
	tagBinaryExpr         = 1
	tagUnaryExpr          = 2
	tagCallExpr           = 3
	tagIndexExpr          = 4
	tagSliceExpr          = 5
	tagSliceTypeExpr      = 6
	tagMemberExpr         = 7
	tagOptionalChainExpr  = 8
	tagIsExpr             = 9
	tagCastExpr           = 10
	tagErrorPropagateExpr = 11
	tagErrorPanicExpr     = 12
	tagOptionalUnwrapExpr = 13
	tagErrorHandlerExpr   = 14
	tagIfExpr             = 15
	tagMatchExpr          = 16
	tagGoExpr             = 17
	tagUnsafeExpr         = 18
	tagLambdaExpr         = 19
	tagIntLit             = 20
	tagFloatLit           = 21
	tagBoolLit            = 22
	tagNoneLit            = 23
	tagCharLit            = 24
	tagStringLit          = 25
	tagIdentExpr          = 26
	tagThisExpr           = 27
	tagParenExpr          = 28
	tagTupleLit           = 29
	tagArrayLit           = 30
	tagMapLit             = 31
	tagAutoCloneExpr      = 32 // T0605: synth-only; never serialized (defensive)
	tagTypeRefExpr        = 33 // T0670: typeInstCallExpr wrapper for optional/function type args

	// Statements (51-80)
	tagBlock              = 51
	tagTypedVarDecl       = 52
	tagInferredVarDecl    = 53
	tagDestructureVarDecl = 54
	tagUseVarDecl         = 55
	tagAssignStmt         = 56
	tagReturnStmt         = 57
	tagRaiseStmt          = 58
	tagYieldStmt          = 59
	tagYieldDelegateStmt  = 60
	tagBreakStmt          = 61
	tagContinueStmt       = 62
	tagExprStmt           = 63
	tagIfStmt             = 64
	tagForInStmt          = 65
	tagIncDecStmt         = 66
	tagClassicForStmt     = 67
	tagInfiniteLoop       = 68
	tagWhileStmt          = 69
	tagWhileUnwrapStmt    = 70
	tagSelectStmt         = 71

	// TypeRefs (81-100)
	tagNamedTypeRef     = 81
	tagQualifiedTypeRef = 82
	tagTupleTypeRef     = 83
	tagFunctionTypeRef  = 84
	tagSharedRefTypeRef = 85
	tagMutRefTypeRef    = 86
	tagPointerTypeRef   = 87
	tagOptionalTypeRef  = 88
	tagSliceTypeRef     = 89
	tagArrayTypeRef     = 90

	// MatchPatterns (101-110)
	tagEnumDestructureMatchPattern  = 101
	tagEnumVariantMatchPattern      = 102
	tagTypeBindingMatchPattern      = 103
	tagShortDestructureMatchPattern = 104
	tagNameMatchPattern             = 105
	tagLiteralMatchPattern          = 106
	tagWildcardMatchPattern         = 107
	tagExpressionMatchPattern       = 108

	// IsPatterns (111-120)
	tagDestructureIsPattern = 111
	tagIdentIsPattern       = 112

	// Declarations (121-130)
	tagTypeDecl = 121
	tagEnumDecl = 122
	tagFuncDecl = 123

	// StringParts (141-150)
	tagStringText   = 141
	tagStringEscape = 142
	tagStringInterp = 143
)

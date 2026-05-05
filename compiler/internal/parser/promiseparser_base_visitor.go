// Code generated from PromiseParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package parser // PromiseParser
import "github.com/antlr4-go/antlr/v4"

type BasePromiseParserVisitor struct {
	*antlr.BaseParseTreeVisitor
}

func (v *BasePromiseParserVisitor) VisitCompilationUnit(ctx *CompilationUnitContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitCatalogImport(ctx *CatalogImportContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSourcedImport(ctx *SourcedImportContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitDeclaration(ctx *DeclarationContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitBindingName(ctx *BindingNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitStringLiteral(ctx *StringLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitStringPart(ctx *StringPartContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeDecl(ctx *TypeDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitInheritance(ctx *InheritanceContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeParams(ctx *TypeParamsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeParam(ctx *TypeParamContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeConstraint(ctx *TypeConstraintContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeMember(ctx *TypeMemberContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFieldDecl(ctx *FieldDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMethodDecl(ctx *MethodDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitGetterDecl(ctx *GetterDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSetterDecl(ctx *SetterDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMemberBody(ctx *MemberBodyContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMethodName(ctx *MethodNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitEnumDecl(ctx *EnumDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitEnumVariant(ctx *EnumVariantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitEnumField(ctx *EnumFieldContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitEnumMember(ctx *EnumMemberContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFuncDecl(ctx *FuncDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitReturnType(ctx *ReturnTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitParams(ctx *ParamsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitParamList(ctx *ParamListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitReceiverParam(ctx *ReceiverParamContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitVariadicParam(ctx *VariadicParamContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitRegularParam(ctx *RegularParamContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitRefMod(ctx *RefModContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitArgs(ctx *ArgsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitArgList(ctx *ArgListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitArg(ctx *ArgContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMetaAnnotation(ctx *MetaAnnotationContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMetaParams(ctx *MetaParamsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMetaParam(ctx *MetaParamContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitNamedType(ctx *NamedTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitQualifiedType(ctx *QualifiedTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitArrayType(ctx *ArrayTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMutRefType(ctx *MutRefTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitPointerType(ctx *PointerTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitOptionalType(ctx *OptionalTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTupleType(ctx *TupleTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFunctionType(ctx *FunctionTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitParenType(ctx *ParenTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSharedRefType(ctx *SharedRefTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSliceType(ctx *SliceTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFuncTypeReturn(ctx *FuncTypeReturnContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeArgs(ctx *TypeArgsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeRefList(ctx *TypeRefListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitBlock(ctx *BlockContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitStatement(ctx *StatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitUseVarDecl(ctx *UseVarDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypedVarDecl(ctx *TypedVarDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitInferredVarDecl(ctx *InferredVarDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitDestructureVarDecl(ctx *DestructureVarDeclContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitAssignmentStmt(ctx *AssignmentStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitAssignOp(ctx *AssignOpContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIncDecStmt(ctx *IncDecStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitReturnStmt(ctx *ReturnStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitRaiseStmt(ctx *RaiseStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitBreakStmt(ctx *BreakStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitContinueStmt(ctx *ContinueStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitYieldStmt(ctx *YieldStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitYieldDelegateStmt(ctx *YieldDelegateStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitExpressionStmt(ctx *ExpressionStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIfStmt(ctx *IfStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIfUnwrapCond(ctx *IfUnwrapCondContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIfExprCond(ctx *IfExprCondContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitElseClause(ctx *ElseClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitForInStmt(ctx *ForInStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitClassicForStmt(ctx *ClassicForStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitInfiniteLoopStmt(ctx *InfiniteLoopStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitForInitTyped(ctx *ForInitTypedContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitForInitInferred(ctx *ForInitInferredContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitForUpdateAssign(ctx *ForUpdateAssignContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitForUpdateIncDec(ctx *ForUpdateIncDecContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitForUpdateExpr(ctx *ForUpdateExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitWhileUnwrapStmt(ctx *WhileUnwrapStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitWhileExprStmt(ctx *WhileExprStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSelectStmt(ctx *SelectStmtContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSelectCase(ctx *SelectCaseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSelectDefault(ctx *SelectDefaultContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSliceTypeExpr(ctx *SliceTypeExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitCastExpr(ctx *CastExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitUnaryNegExpr(ctx *UnaryNegExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitAdditiveExpr(ctx *AdditiveExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitBitwiseNotExpr(ctx *BitwiseNotExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitPrimaryExpr(ctx *PrimaryExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitExclusiveRangeExpr(ctx *ExclusiveRangeExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMemberAccessExpr(ctx *MemberAccessExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitErrorPropagateExpr(ctx *ErrorPropagateExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitCallExpr(ctx *CallExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIsExpr(ctx *IsExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitReceiveExpr(ctx *ReceiveExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitErrorHandlerExpr(ctx *ErrorHandlerExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitInclusiveRangeExpr(ctx *InclusiveRangeExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitLogicalAndExpr(ctx *LogicalAndExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitComparisonExpr(ctx *ComparisonExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitSliceExpr(ctx *SliceExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitElvisExpr(ctx *ElvisExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitShiftExpr(ctx *ShiftExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitBitwiseExpr(ctx *BitwiseExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitLogicalOrExpr(ctx *LogicalOrExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIndexExpr(ctx *IndexExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitOptionalChainExpr(ctx *OptionalChainExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitErrorUnwrapExpr(ctx *ErrorUnwrapExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitUnaryNotExpr(ctx *UnaryNotExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMultiplicativeExpr(ctx *MultiplicativeExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitEqualityExpr(ctx *EqualityExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIntLiteral(ctx *IntLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFloatLiteral(ctx *FloatLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTrueLiteral(ctx *TrueLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFalseLiteral(ctx *FalseLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitNoneLiteral(ctx *NoneLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitCharLiteral(ctx *CharLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitStringLit(ctx *StringLitContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIdentExpr(ctx *IdentExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitThisExpr(ctx *ThisExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitParenExpr(ctx *ParenExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTupleLiteral(ctx *TupleLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitArrayLiteral(ctx *ArrayLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMapLiteral(ctx *MapLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitLambda(ctx *LambdaContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIfExpression(ctx *IfExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMatchExpression(ctx *MatchExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitGoExpression(ctx *GoExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitUnsafeExpression(ctx *UnsafeExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMapEntry(ctx *MapEntryContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitLambdaExpr(ctx *LambdaExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitLambdaParams(ctx *LambdaParamsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypedLambdaParam(ctx *TypedLambdaParamContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitUntypedLambdaParam(ctx *UntypedLambdaParamContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIfExpr(ctx *IfExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMatchExpr(ctx *MatchExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitMatchArm(ctx *MatchArmContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitEnumDestructurePattern(ctx *EnumDestructurePatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitEnumVariantPattern(ctx *EnumVariantPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTypeBindingPattern(ctx *TypeBindingPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitShortDestructurePattern(ctx *ShortDestructurePatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitNamePattern(ctx *NamePatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIntLiteralPattern(ctx *IntLiteralPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFloatLiteralPattern(ctx *FloatLiteralPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitCharLiteralPattern(ctx *CharLiteralPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitTrueLiteralPattern(ctx *TrueLiteralPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitFalseLiteralPattern(ctx *FalseLiteralPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitNoneLiteralPattern(ctx *NoneLiteralPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitStringLiteralPattern(ctx *StringLiteralPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitWildcardPattern(ctx *WildcardPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitExpressionPattern(ctx *ExpressionPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitDestructureIsPattern(ctx *DestructureIsPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitIdentIsPattern(ctx *IdentIsPatternContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitPatternFields(ctx *PatternFieldsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitGoExpr(ctx *GoExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BasePromiseParserVisitor) VisitUnsafeBlock(ctx *UnsafeBlockContext) interface{} {
	return v.VisitChildren(ctx)
}

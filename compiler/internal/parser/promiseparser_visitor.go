// Code generated from PromiseParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package parser // PromiseParser
import "github.com/antlr4-go/antlr/v4"

// A complete Visitor for a parse tree produced by PromiseParser.
type PromiseParserVisitor interface {
	antlr.ParseTreeVisitor

	// Visit a parse tree produced by PromiseParser#compilationUnit.
	VisitCompilationUnit(ctx *CompilationUnitContext) interface{}

	// Visit a parse tree produced by PromiseParser#catalogImport.
	VisitCatalogImport(ctx *CatalogImportContext) interface{}

	// Visit a parse tree produced by PromiseParser#sourcedImport.
	VisitSourcedImport(ctx *SourcedImportContext) interface{}

	// Visit a parse tree produced by PromiseParser#declaration.
	VisitDeclaration(ctx *DeclarationContext) interface{}

	// Visit a parse tree produced by PromiseParser#bindingName.
	VisitBindingName(ctx *BindingNameContext) interface{}

	// Visit a parse tree produced by PromiseParser#stringLiteral.
	VisitStringLiteral(ctx *StringLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#stringPart.
	VisitStringPart(ctx *StringPartContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeDecl.
	VisitTypeDecl(ctx *TypeDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#inheritance.
	VisitInheritance(ctx *InheritanceContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeParams.
	VisitTypeParams(ctx *TypeParamsContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeParam.
	VisitTypeParam(ctx *TypeParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeConstraint.
	VisitTypeConstraint(ctx *TypeConstraintContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeMember.
	VisitTypeMember(ctx *TypeMemberContext) interface{}

	// Visit a parse tree produced by PromiseParser#fieldDecl.
	VisitFieldDecl(ctx *FieldDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#methodDecl.
	VisitMethodDecl(ctx *MethodDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#getterDecl.
	VisitGetterDecl(ctx *GetterDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#setterDecl.
	VisitSetterDecl(ctx *SetterDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#memberBody.
	VisitMemberBody(ctx *MemberBodyContext) interface{}

	// Visit a parse tree produced by PromiseParser#methodName.
	VisitMethodName(ctx *MethodNameContext) interface{}

	// Visit a parse tree produced by PromiseParser#enumDecl.
	VisitEnumDecl(ctx *EnumDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#enumVariant.
	VisitEnumVariant(ctx *EnumVariantContext) interface{}

	// Visit a parse tree produced by PromiseParser#enumField.
	VisitEnumField(ctx *EnumFieldContext) interface{}

	// Visit a parse tree produced by PromiseParser#enumMember.
	VisitEnumMember(ctx *EnumMemberContext) interface{}

	// Visit a parse tree produced by PromiseParser#funcDecl.
	VisitFuncDecl(ctx *FuncDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#returnType.
	VisitReturnType(ctx *ReturnTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#params.
	VisitParams(ctx *ParamsContext) interface{}

	// Visit a parse tree produced by PromiseParser#paramList.
	VisitParamList(ctx *ParamListContext) interface{}

	// Visit a parse tree produced by PromiseParser#receiverParam.
	VisitReceiverParam(ctx *ReceiverParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#variadicParam.
	VisitVariadicParam(ctx *VariadicParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#moveParam.
	VisitMoveParam(ctx *MoveParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#regularParam.
	VisitRegularParam(ctx *RegularParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#refMod.
	VisitRefMod(ctx *RefModContext) interface{}

	// Visit a parse tree produced by PromiseParser#args.
	VisitArgs(ctx *ArgsContext) interface{}

	// Visit a parse tree produced by PromiseParser#argList.
	VisitArgList(ctx *ArgListContext) interface{}

	// Visit a parse tree produced by PromiseParser#arg.
	VisitArg(ctx *ArgContext) interface{}

	// Visit a parse tree produced by PromiseParser#metaAnnotation.
	VisitMetaAnnotation(ctx *MetaAnnotationContext) interface{}

	// Visit a parse tree produced by PromiseParser#metaParams.
	VisitMetaParams(ctx *MetaParamsContext) interface{}

	// Visit a parse tree produced by PromiseParser#metaParam.
	VisitMetaParam(ctx *MetaParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#namedType.
	VisitNamedType(ctx *NamedTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#qualifiedType.
	VisitQualifiedType(ctx *QualifiedTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#arrayType.
	VisitArrayType(ctx *ArrayTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#mutRefType.
	VisitMutRefType(ctx *MutRefTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#pointerType.
	VisitPointerType(ctx *PointerTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#optionalType.
	VisitOptionalType(ctx *OptionalTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#tupleType.
	VisitTupleType(ctx *TupleTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#functionType.
	VisitFunctionType(ctx *FunctionTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#parenType.
	VisitParenType(ctx *ParenTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#sharedRefType.
	VisitSharedRefType(ctx *SharedRefTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#sliceType.
	VisitSliceType(ctx *SliceTypeContext) interface{}

	// Visit a parse tree produced by PromiseParser#funcTypeReturn.
	VisitFuncTypeReturn(ctx *FuncTypeReturnContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeArgs.
	VisitTypeArgs(ctx *TypeArgsContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeRefList.
	VisitTypeRefList(ctx *TypeRefListContext) interface{}

	// Visit a parse tree produced by PromiseParser#block.
	VisitBlock(ctx *BlockContext) interface{}

	// Visit a parse tree produced by PromiseParser#semi.
	VisitSemi(ctx *SemiContext) interface{}

	// Visit a parse tree produced by PromiseParser#statement.
	VisitStatement(ctx *StatementContext) interface{}

	// Visit a parse tree produced by PromiseParser#useVarDecl.
	VisitUseVarDecl(ctx *UseVarDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#typedVarDecl.
	VisitTypedVarDecl(ctx *TypedVarDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#inferredVarDecl.
	VisitInferredVarDecl(ctx *InferredVarDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#destructureVarDecl.
	VisitDestructureVarDecl(ctx *DestructureVarDeclContext) interface{}

	// Visit a parse tree produced by PromiseParser#assignmentStmt.
	VisitAssignmentStmt(ctx *AssignmentStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#assignOp.
	VisitAssignOp(ctx *AssignOpContext) interface{}

	// Visit a parse tree produced by PromiseParser#incDecStmt.
	VisitIncDecStmt(ctx *IncDecStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#returnStmt.
	VisitReturnStmt(ctx *ReturnStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#raiseStmt.
	VisitRaiseStmt(ctx *RaiseStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#breakStmt.
	VisitBreakStmt(ctx *BreakStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#continueStmt.
	VisitContinueStmt(ctx *ContinueStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#yieldStmt.
	VisitYieldStmt(ctx *YieldStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#yieldDelegateStmt.
	VisitYieldDelegateStmt(ctx *YieldDelegateStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#expressionStmt.
	VisitExpressionStmt(ctx *ExpressionStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#ifStmt.
	VisitIfStmt(ctx *IfStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#ifUnwrapCond.
	VisitIfUnwrapCond(ctx *IfUnwrapCondContext) interface{}

	// Visit a parse tree produced by PromiseParser#ifExprCond.
	VisitIfExprCond(ctx *IfExprCondContext) interface{}

	// Visit a parse tree produced by PromiseParser#elseClause.
	VisitElseClause(ctx *ElseClauseContext) interface{}

	// Visit a parse tree produced by PromiseParser#forInStmt.
	VisitForInStmt(ctx *ForInStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#classicForStmt.
	VisitClassicForStmt(ctx *ClassicForStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#infiniteLoopStmt.
	VisitInfiniteLoopStmt(ctx *InfiniteLoopStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#forInitTyped.
	VisitForInitTyped(ctx *ForInitTypedContext) interface{}

	// Visit a parse tree produced by PromiseParser#forInitInferred.
	VisitForInitInferred(ctx *ForInitInferredContext) interface{}

	// Visit a parse tree produced by PromiseParser#forUpdateAssign.
	VisitForUpdateAssign(ctx *ForUpdateAssignContext) interface{}

	// Visit a parse tree produced by PromiseParser#forUpdateIncDec.
	VisitForUpdateIncDec(ctx *ForUpdateIncDecContext) interface{}

	// Visit a parse tree produced by PromiseParser#forUpdateExpr.
	VisitForUpdateExpr(ctx *ForUpdateExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#whileUnwrapStmt.
	VisitWhileUnwrapStmt(ctx *WhileUnwrapStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#whileExprStmt.
	VisitWhileExprStmt(ctx *WhileExprStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#selectStmt.
	VisitSelectStmt(ctx *SelectStmtContext) interface{}

	// Visit a parse tree produced by PromiseParser#selectCase.
	VisitSelectCase(ctx *SelectCaseContext) interface{}

	// Visit a parse tree produced by PromiseParser#selectDefault.
	VisitSelectDefault(ctx *SelectDefaultContext) interface{}

	// Visit a parse tree produced by PromiseParser#optionalUnwrapExpr.
	VisitOptionalUnwrapExpr(ctx *OptionalUnwrapExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#sliceTypeExpr.
	VisitSliceTypeExpr(ctx *SliceTypeExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#castExpr.
	VisitCastExpr(ctx *CastExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#unaryNegExpr.
	VisitUnaryNegExpr(ctx *UnaryNegExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#additiveExpr.
	VisitAdditiveExpr(ctx *AdditiveExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#bitwiseNotExpr.
	VisitBitwiseNotExpr(ctx *BitwiseNotExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#errorPanicExpr.
	VisitErrorPanicExpr(ctx *ErrorPanicExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#primaryExpr.
	VisitPrimaryExpr(ctx *PrimaryExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#exclusiveRangeExpr.
	VisitExclusiveRangeExpr(ctx *ExclusiveRangeExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#memberAccessExpr.
	VisitMemberAccessExpr(ctx *MemberAccessExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#errorPropagateExpr.
	VisitErrorPropagateExpr(ctx *ErrorPropagateExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#callExpr.
	VisitCallExpr(ctx *CallExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#isExpr.
	VisitIsExpr(ctx *IsExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#receiveExpr.
	VisitReceiveExpr(ctx *ReceiveExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#errorHandlerExpr.
	VisitErrorHandlerExpr(ctx *ErrorHandlerExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#inclusiveRangeExpr.
	VisitInclusiveRangeExpr(ctx *InclusiveRangeExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#logicalAndExpr.
	VisitLogicalAndExpr(ctx *LogicalAndExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#comparisonExpr.
	VisitComparisonExpr(ctx *ComparisonExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#sliceExpr.
	VisitSliceExpr(ctx *SliceExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#elvisExpr.
	VisitElvisExpr(ctx *ElvisExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#shiftExpr.
	VisitShiftExpr(ctx *ShiftExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#bitwiseExpr.
	VisitBitwiseExpr(ctx *BitwiseExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#logicalOrExpr.
	VisitLogicalOrExpr(ctx *LogicalOrExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#indexExpr.
	VisitIndexExpr(ctx *IndexExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#optionalChainExpr.
	VisitOptionalChainExpr(ctx *OptionalChainExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#unaryNotExpr.
	VisitUnaryNotExpr(ctx *UnaryNotExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#multiplicativeExpr.
	VisitMultiplicativeExpr(ctx *MultiplicativeExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#equalityExpr.
	VisitEqualityExpr(ctx *EqualityExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#intLiteral.
	VisitIntLiteral(ctx *IntLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#floatLiteral.
	VisitFloatLiteral(ctx *FloatLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#trueLiteral.
	VisitTrueLiteral(ctx *TrueLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#falseLiteral.
	VisitFalseLiteral(ctx *FalseLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#noneLiteral.
	VisitNoneLiteral(ctx *NoneLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#charLiteral.
	VisitCharLiteral(ctx *CharLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#stringLit.
	VisitStringLit(ctx *StringLitContext) interface{}

	// Visit a parse tree produced by PromiseParser#identExpr.
	VisitIdentExpr(ctx *IdentExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#thisExpr.
	VisitThisExpr(ctx *ThisExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#parenExpr.
	VisitParenExpr(ctx *ParenExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#tupleLiteral.
	VisitTupleLiteral(ctx *TupleLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#arrayLiteral.
	VisitArrayLiteral(ctx *ArrayLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#mapLiteral.
	VisitMapLiteral(ctx *MapLiteralContext) interface{}

	// Visit a parse tree produced by PromiseParser#lambda.
	VisitLambda(ctx *LambdaContext) interface{}

	// Visit a parse tree produced by PromiseParser#ifExpression.
	VisitIfExpression(ctx *IfExpressionContext) interface{}

	// Visit a parse tree produced by PromiseParser#matchExpression.
	VisitMatchExpression(ctx *MatchExpressionContext) interface{}

	// Visit a parse tree produced by PromiseParser#goExpression.
	VisitGoExpression(ctx *GoExpressionContext) interface{}

	// Visit a parse tree produced by PromiseParser#unsafeExpression.
	VisitUnsafeExpression(ctx *UnsafeExpressionContext) interface{}

	// Visit a parse tree produced by PromiseParser#mapEntry.
	VisitMapEntry(ctx *MapEntryContext) interface{}

	// Visit a parse tree produced by PromiseParser#lambdaExpr.
	VisitLambdaExpr(ctx *LambdaExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#lambdaParams.
	VisitLambdaParams(ctx *LambdaParamsContext) interface{}

	// Visit a parse tree produced by PromiseParser#typedLambdaParam.
	VisitTypedLambdaParam(ctx *TypedLambdaParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#untypedLambdaParam.
	VisitUntypedLambdaParam(ctx *UntypedLambdaParamContext) interface{}

	// Visit a parse tree produced by PromiseParser#ifExpr.
	VisitIfExpr(ctx *IfExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#matchExpr.
	VisitMatchExpr(ctx *MatchExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#matchArm.
	VisitMatchArm(ctx *MatchArmContext) interface{}

	// Visit a parse tree produced by PromiseParser#enumDestructurePattern.
	VisitEnumDestructurePattern(ctx *EnumDestructurePatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#enumVariantPattern.
	VisitEnumVariantPattern(ctx *EnumVariantPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#typeBindingPattern.
	VisitTypeBindingPattern(ctx *TypeBindingPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#shortDestructurePattern.
	VisitShortDestructurePattern(ctx *ShortDestructurePatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#namePattern.
	VisitNamePattern(ctx *NamePatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#intLiteralPattern.
	VisitIntLiteralPattern(ctx *IntLiteralPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#floatLiteralPattern.
	VisitFloatLiteralPattern(ctx *FloatLiteralPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#charLiteralPattern.
	VisitCharLiteralPattern(ctx *CharLiteralPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#trueLiteralPattern.
	VisitTrueLiteralPattern(ctx *TrueLiteralPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#falseLiteralPattern.
	VisitFalseLiteralPattern(ctx *FalseLiteralPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#noneLiteralPattern.
	VisitNoneLiteralPattern(ctx *NoneLiteralPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#stringLiteralPattern.
	VisitStringLiteralPattern(ctx *StringLiteralPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#wildcardPattern.
	VisitWildcardPattern(ctx *WildcardPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#expressionPattern.
	VisitExpressionPattern(ctx *ExpressionPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#destructureIsPattern.
	VisitDestructureIsPattern(ctx *DestructureIsPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#identIsPattern.
	VisitIdentIsPattern(ctx *IdentIsPatternContext) interface{}

	// Visit a parse tree produced by PromiseParser#patternFields.
	VisitPatternFields(ctx *PatternFieldsContext) interface{}

	// Visit a parse tree produced by PromiseParser#goExpr.
	VisitGoExpr(ctx *GoExprContext) interface{}

	// Visit a parse tree produced by PromiseParser#unsafeBlock.
	VisitUnsafeBlock(ctx *UnsafeBlockContext) interface{}
}

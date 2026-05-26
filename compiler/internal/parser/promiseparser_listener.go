// Code generated from PromiseParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package parser // PromiseParser
import "github.com/antlr4-go/antlr/v4"

// PromiseParserListener is a complete listener for a parse tree produced by PromiseParser.
type PromiseParserListener interface {
	antlr.ParseTreeListener

	// EnterCompilationUnit is called when entering the compilationUnit production.
	EnterCompilationUnit(c *CompilationUnitContext)

	// EnterCatalogImport is called when entering the catalogImport production.
	EnterCatalogImport(c *CatalogImportContext)

	// EnterSourcedImport is called when entering the sourcedImport production.
	EnterSourcedImport(c *SourcedImportContext)

	// EnterDeclaration is called when entering the declaration production.
	EnterDeclaration(c *DeclarationContext)

	// EnterBindingName is called when entering the bindingName production.
	EnterBindingName(c *BindingNameContext)

	// EnterStringLiteral is called when entering the stringLiteral production.
	EnterStringLiteral(c *StringLiteralContext)

	// EnterStringPart is called when entering the stringPart production.
	EnterStringPart(c *StringPartContext)

	// EnterTypeDecl is called when entering the typeDecl production.
	EnterTypeDecl(c *TypeDeclContext)

	// EnterInheritance is called when entering the inheritance production.
	EnterInheritance(c *InheritanceContext)

	// EnterTypeParams is called when entering the typeParams production.
	EnterTypeParams(c *TypeParamsContext)

	// EnterTypeParam is called when entering the typeParam production.
	EnterTypeParam(c *TypeParamContext)

	// EnterTypeConstraint is called when entering the typeConstraint production.
	EnterTypeConstraint(c *TypeConstraintContext)

	// EnterTypeMember is called when entering the typeMember production.
	EnterTypeMember(c *TypeMemberContext)

	// EnterFieldDecl is called when entering the fieldDecl production.
	EnterFieldDecl(c *FieldDeclContext)

	// EnterMethodDecl is called when entering the methodDecl production.
	EnterMethodDecl(c *MethodDeclContext)

	// EnterGetterDecl is called when entering the getterDecl production.
	EnterGetterDecl(c *GetterDeclContext)

	// EnterSetterDecl is called when entering the setterDecl production.
	EnterSetterDecl(c *SetterDeclContext)

	// EnterMemberBody is called when entering the memberBody production.
	EnterMemberBody(c *MemberBodyContext)

	// EnterMethodName is called when entering the methodName production.
	EnterMethodName(c *MethodNameContext)

	// EnterEnumDecl is called when entering the enumDecl production.
	EnterEnumDecl(c *EnumDeclContext)

	// EnterEnumVariant is called when entering the enumVariant production.
	EnterEnumVariant(c *EnumVariantContext)

	// EnterEnumField is called when entering the enumField production.
	EnterEnumField(c *EnumFieldContext)

	// EnterEnumMember is called when entering the enumMember production.
	EnterEnumMember(c *EnumMemberContext)

	// EnterFuncDecl is called when entering the funcDecl production.
	EnterFuncDecl(c *FuncDeclContext)

	// EnterReturnType is called when entering the returnType production.
	EnterReturnType(c *ReturnTypeContext)

	// EnterParams is called when entering the params production.
	EnterParams(c *ParamsContext)

	// EnterParamList is called when entering the paramList production.
	EnterParamList(c *ParamListContext)

	// EnterReceiverParam is called when entering the receiverParam production.
	EnterReceiverParam(c *ReceiverParamContext)

	// EnterVariadicParam is called when entering the variadicParam production.
	EnterVariadicParam(c *VariadicParamContext)

	// EnterMoveParam is called when entering the moveParam production.
	EnterMoveParam(c *MoveParamContext)

	// EnterRegularParam is called when entering the regularParam production.
	EnterRegularParam(c *RegularParamContext)

	// EnterRefMod is called when entering the refMod production.
	EnterRefMod(c *RefModContext)

	// EnterArgs is called when entering the args production.
	EnterArgs(c *ArgsContext)

	// EnterArgList is called when entering the argList production.
	EnterArgList(c *ArgListContext)

	// EnterArg is called when entering the arg production.
	EnterArg(c *ArgContext)

	// EnterMetaAnnotation is called when entering the metaAnnotation production.
	EnterMetaAnnotation(c *MetaAnnotationContext)

	// EnterMetaParams is called when entering the metaParams production.
	EnterMetaParams(c *MetaParamsContext)

	// EnterMetaParam is called when entering the metaParam production.
	EnterMetaParam(c *MetaParamContext)

	// EnterNamedType is called when entering the namedType production.
	EnterNamedType(c *NamedTypeContext)

	// EnterQualifiedType is called when entering the qualifiedType production.
	EnterQualifiedType(c *QualifiedTypeContext)

	// EnterArrayType is called when entering the arrayType production.
	EnterArrayType(c *ArrayTypeContext)

	// EnterMutRefType is called when entering the mutRefType production.
	EnterMutRefType(c *MutRefTypeContext)

	// EnterPointerType is called when entering the pointerType production.
	EnterPointerType(c *PointerTypeContext)

	// EnterOptionalType is called when entering the optionalType production.
	EnterOptionalType(c *OptionalTypeContext)

	// EnterTupleType is called when entering the tupleType production.
	EnterTupleType(c *TupleTypeContext)

	// EnterFunctionType is called when entering the functionType production.
	EnterFunctionType(c *FunctionTypeContext)

	// EnterParenType is called when entering the parenType production.
	EnterParenType(c *ParenTypeContext)

	// EnterSharedRefType is called when entering the sharedRefType production.
	EnterSharedRefType(c *SharedRefTypeContext)

	// EnterSliceType is called when entering the sliceType production.
	EnterSliceType(c *SliceTypeContext)

	// EnterFuncTypeReturn is called when entering the funcTypeReturn production.
	EnterFuncTypeReturn(c *FuncTypeReturnContext)

	// EnterTypeArgs is called when entering the typeArgs production.
	EnterTypeArgs(c *TypeArgsContext)

	// EnterTypeRefList is called when entering the typeRefList production.
	EnterTypeRefList(c *TypeRefListContext)

	// EnterBlock is called when entering the block production.
	EnterBlock(c *BlockContext)

	// EnterSemi is called when entering the semi production.
	EnterSemi(c *SemiContext)

	// EnterStatement is called when entering the statement production.
	EnterStatement(c *StatementContext)

	// EnterUseVarDecl is called when entering the useVarDecl production.
	EnterUseVarDecl(c *UseVarDeclContext)

	// EnterTypedVarDecl is called when entering the typedVarDecl production.
	EnterTypedVarDecl(c *TypedVarDeclContext)

	// EnterInferredVarDecl is called when entering the inferredVarDecl production.
	EnterInferredVarDecl(c *InferredVarDeclContext)

	// EnterDestructureVarDecl is called when entering the destructureVarDecl production.
	EnterDestructureVarDecl(c *DestructureVarDeclContext)

	// EnterAssignmentStmt is called when entering the assignmentStmt production.
	EnterAssignmentStmt(c *AssignmentStmtContext)

	// EnterAssignOp is called when entering the assignOp production.
	EnterAssignOp(c *AssignOpContext)

	// EnterIncDecStmt is called when entering the incDecStmt production.
	EnterIncDecStmt(c *IncDecStmtContext)

	// EnterReturnStmt is called when entering the returnStmt production.
	EnterReturnStmt(c *ReturnStmtContext)

	// EnterRaiseStmt is called when entering the raiseStmt production.
	EnterRaiseStmt(c *RaiseStmtContext)

	// EnterBreakStmt is called when entering the breakStmt production.
	EnterBreakStmt(c *BreakStmtContext)

	// EnterContinueStmt is called when entering the continueStmt production.
	EnterContinueStmt(c *ContinueStmtContext)

	// EnterYieldStmt is called when entering the yieldStmt production.
	EnterYieldStmt(c *YieldStmtContext)

	// EnterYieldDelegateStmt is called when entering the yieldDelegateStmt production.
	EnterYieldDelegateStmt(c *YieldDelegateStmtContext)

	// EnterExpressionStmt is called when entering the expressionStmt production.
	EnterExpressionStmt(c *ExpressionStmtContext)

	// EnterIfStmt is called when entering the ifStmt production.
	EnterIfStmt(c *IfStmtContext)

	// EnterIfUnwrapCond is called when entering the ifUnwrapCond production.
	EnterIfUnwrapCond(c *IfUnwrapCondContext)

	// EnterIfExprCond is called when entering the ifExprCond production.
	EnterIfExprCond(c *IfExprCondContext)

	// EnterElseClause is called when entering the elseClause production.
	EnterElseClause(c *ElseClauseContext)

	// EnterForInStmt is called when entering the forInStmt production.
	EnterForInStmt(c *ForInStmtContext)

	// EnterClassicForStmt is called when entering the classicForStmt production.
	EnterClassicForStmt(c *ClassicForStmtContext)

	// EnterInfiniteLoopStmt is called when entering the infiniteLoopStmt production.
	EnterInfiniteLoopStmt(c *InfiniteLoopStmtContext)

	// EnterForInitTyped is called when entering the forInitTyped production.
	EnterForInitTyped(c *ForInitTypedContext)

	// EnterForInitInferred is called when entering the forInitInferred production.
	EnterForInitInferred(c *ForInitInferredContext)

	// EnterForUpdateAssign is called when entering the forUpdateAssign production.
	EnterForUpdateAssign(c *ForUpdateAssignContext)

	// EnterForUpdateIncDec is called when entering the forUpdateIncDec production.
	EnterForUpdateIncDec(c *ForUpdateIncDecContext)

	// EnterForUpdateExpr is called when entering the forUpdateExpr production.
	EnterForUpdateExpr(c *ForUpdateExprContext)

	// EnterWhileUnwrapStmt is called when entering the whileUnwrapStmt production.
	EnterWhileUnwrapStmt(c *WhileUnwrapStmtContext)

	// EnterWhileExprStmt is called when entering the whileExprStmt production.
	EnterWhileExprStmt(c *WhileExprStmtContext)

	// EnterSelectStmt is called when entering the selectStmt production.
	EnterSelectStmt(c *SelectStmtContext)

	// EnterSelectCase is called when entering the selectCase production.
	EnterSelectCase(c *SelectCaseContext)

	// EnterSelectDefault is called when entering the selectDefault production.
	EnterSelectDefault(c *SelectDefaultContext)

	// EnterOptionalUnwrapExpr is called when entering the optionalUnwrapExpr production.
	EnterOptionalUnwrapExpr(c *OptionalUnwrapExprContext)

	// EnterSliceTypeExpr is called when entering the sliceTypeExpr production.
	EnterSliceTypeExpr(c *SliceTypeExprContext)

	// EnterCastExpr is called when entering the castExpr production.
	EnterCastExpr(c *CastExprContext)

	// EnterUnaryNegExpr is called when entering the unaryNegExpr production.
	EnterUnaryNegExpr(c *UnaryNegExprContext)

	// EnterAdditiveExpr is called when entering the additiveExpr production.
	EnterAdditiveExpr(c *AdditiveExprContext)

	// EnterBitwiseNotExpr is called when entering the bitwiseNotExpr production.
	EnterBitwiseNotExpr(c *BitwiseNotExprContext)

	// EnterTypeInstCallExpr is called when entering the typeInstCallExpr production.
	EnterTypeInstCallExpr(c *TypeInstCallExprContext)

	// EnterErrorPanicExpr is called when entering the errorPanicExpr production.
	EnterErrorPanicExpr(c *ErrorPanicExprContext)

	// EnterPrimaryExpr is called when entering the primaryExpr production.
	EnterPrimaryExpr(c *PrimaryExprContext)

	// EnterExclusiveRangeExpr is called when entering the exclusiveRangeExpr production.
	EnterExclusiveRangeExpr(c *ExclusiveRangeExprContext)

	// EnterMemberAccessExpr is called when entering the memberAccessExpr production.
	EnterMemberAccessExpr(c *MemberAccessExprContext)

	// EnterErrorPropagateExpr is called when entering the errorPropagateExpr production.
	EnterErrorPropagateExpr(c *ErrorPropagateExprContext)

	// EnterCallExpr is called when entering the callExpr production.
	EnterCallExpr(c *CallExprContext)

	// EnterIsExpr is called when entering the isExpr production.
	EnterIsExpr(c *IsExprContext)

	// EnterReceiveExpr is called when entering the receiveExpr production.
	EnterReceiveExpr(c *ReceiveExprContext)

	// EnterErrorHandlerExpr is called when entering the errorHandlerExpr production.
	EnterErrorHandlerExpr(c *ErrorHandlerExprContext)

	// EnterInclusiveRangeExpr is called when entering the inclusiveRangeExpr production.
	EnterInclusiveRangeExpr(c *InclusiveRangeExprContext)

	// EnterLogicalAndExpr is called when entering the logicalAndExpr production.
	EnterLogicalAndExpr(c *LogicalAndExprContext)

	// EnterComparisonExpr is called when entering the comparisonExpr production.
	EnterComparisonExpr(c *ComparisonExprContext)

	// EnterSliceExpr is called when entering the sliceExpr production.
	EnterSliceExpr(c *SliceExprContext)

	// EnterElvisExpr is called when entering the elvisExpr production.
	EnterElvisExpr(c *ElvisExprContext)

	// EnterShiftExpr is called when entering the shiftExpr production.
	EnterShiftExpr(c *ShiftExprContext)

	// EnterBitwiseExpr is called when entering the bitwiseExpr production.
	EnterBitwiseExpr(c *BitwiseExprContext)

	// EnterLogicalOrExpr is called when entering the logicalOrExpr production.
	EnterLogicalOrExpr(c *LogicalOrExprContext)

	// EnterIndexExpr is called when entering the indexExpr production.
	EnterIndexExpr(c *IndexExprContext)

	// EnterOptionalChainExpr is called when entering the optionalChainExpr production.
	EnterOptionalChainExpr(c *OptionalChainExprContext)

	// EnterUnaryNotExpr is called when entering the unaryNotExpr production.
	EnterUnaryNotExpr(c *UnaryNotExprContext)

	// EnterMultiplicativeExpr is called when entering the multiplicativeExpr production.
	EnterMultiplicativeExpr(c *MultiplicativeExprContext)

	// EnterEqualityExpr is called when entering the equalityExpr production.
	EnterEqualityExpr(c *EqualityExprContext)

	// EnterIntLiteral is called when entering the intLiteral production.
	EnterIntLiteral(c *IntLiteralContext)

	// EnterFloatLiteral is called when entering the floatLiteral production.
	EnterFloatLiteral(c *FloatLiteralContext)

	// EnterTrueLiteral is called when entering the trueLiteral production.
	EnterTrueLiteral(c *TrueLiteralContext)

	// EnterFalseLiteral is called when entering the falseLiteral production.
	EnterFalseLiteral(c *FalseLiteralContext)

	// EnterNoneLiteral is called when entering the noneLiteral production.
	EnterNoneLiteral(c *NoneLiteralContext)

	// EnterCharLiteral is called when entering the charLiteral production.
	EnterCharLiteral(c *CharLiteralContext)

	// EnterStringLit is called when entering the stringLit production.
	EnterStringLit(c *StringLitContext)

	// EnterIdentExpr is called when entering the identExpr production.
	EnterIdentExpr(c *IdentExprContext)

	// EnterThisExpr is called when entering the thisExpr production.
	EnterThisExpr(c *ThisExprContext)

	// EnterParenExpr is called when entering the parenExpr production.
	EnterParenExpr(c *ParenExprContext)

	// EnterTupleLiteral is called when entering the tupleLiteral production.
	EnterTupleLiteral(c *TupleLiteralContext)

	// EnterArrayLiteral is called when entering the arrayLiteral production.
	EnterArrayLiteral(c *ArrayLiteralContext)

	// EnterMapLiteral is called when entering the mapLiteral production.
	EnterMapLiteral(c *MapLiteralContext)

	// EnterLambda is called when entering the lambda production.
	EnterLambda(c *LambdaContext)

	// EnterIfExpression is called when entering the ifExpression production.
	EnterIfExpression(c *IfExpressionContext)

	// EnterMatchExpression is called when entering the matchExpression production.
	EnterMatchExpression(c *MatchExpressionContext)

	// EnterGoExpression is called when entering the goExpression production.
	EnterGoExpression(c *GoExpressionContext)

	// EnterUnsafeExpression is called when entering the unsafeExpression production.
	EnterUnsafeExpression(c *UnsafeExpressionContext)

	// EnterMapEntry is called when entering the mapEntry production.
	EnterMapEntry(c *MapEntryContext)

	// EnterLambdaExpr is called when entering the lambdaExpr production.
	EnterLambdaExpr(c *LambdaExprContext)

	// EnterLambdaParams is called when entering the lambdaParams production.
	EnterLambdaParams(c *LambdaParamsContext)

	// EnterTypedLambdaParam is called when entering the typedLambdaParam production.
	EnterTypedLambdaParam(c *TypedLambdaParamContext)

	// EnterUntypedLambdaParam is called when entering the untypedLambdaParam production.
	EnterUntypedLambdaParam(c *UntypedLambdaParamContext)

	// EnterIfExpr is called when entering the ifExpr production.
	EnterIfExpr(c *IfExprContext)

	// EnterMatchExpr is called when entering the matchExpr production.
	EnterMatchExpr(c *MatchExprContext)

	// EnterMatchArm is called when entering the matchArm production.
	EnterMatchArm(c *MatchArmContext)

	// EnterEnumDestructurePattern is called when entering the enumDestructurePattern production.
	EnterEnumDestructurePattern(c *EnumDestructurePatternContext)

	// EnterEnumVariantPattern is called when entering the enumVariantPattern production.
	EnterEnumVariantPattern(c *EnumVariantPatternContext)

	// EnterTypeBindingPattern is called when entering the typeBindingPattern production.
	EnterTypeBindingPattern(c *TypeBindingPatternContext)

	// EnterShortDestructurePattern is called when entering the shortDestructurePattern production.
	EnterShortDestructurePattern(c *ShortDestructurePatternContext)

	// EnterNamePattern is called when entering the namePattern production.
	EnterNamePattern(c *NamePatternContext)

	// EnterIntLiteralPattern is called when entering the intLiteralPattern production.
	EnterIntLiteralPattern(c *IntLiteralPatternContext)

	// EnterFloatLiteralPattern is called when entering the floatLiteralPattern production.
	EnterFloatLiteralPattern(c *FloatLiteralPatternContext)

	// EnterCharLiteralPattern is called when entering the charLiteralPattern production.
	EnterCharLiteralPattern(c *CharLiteralPatternContext)

	// EnterTrueLiteralPattern is called when entering the trueLiteralPattern production.
	EnterTrueLiteralPattern(c *TrueLiteralPatternContext)

	// EnterFalseLiteralPattern is called when entering the falseLiteralPattern production.
	EnterFalseLiteralPattern(c *FalseLiteralPatternContext)

	// EnterNoneLiteralPattern is called when entering the noneLiteralPattern production.
	EnterNoneLiteralPattern(c *NoneLiteralPatternContext)

	// EnterStringLiteralPattern is called when entering the stringLiteralPattern production.
	EnterStringLiteralPattern(c *StringLiteralPatternContext)

	// EnterWildcardPattern is called when entering the wildcardPattern production.
	EnterWildcardPattern(c *WildcardPatternContext)

	// EnterExpressionPattern is called when entering the expressionPattern production.
	EnterExpressionPattern(c *ExpressionPatternContext)

	// EnterDestructureIsPattern is called when entering the destructureIsPattern production.
	EnterDestructureIsPattern(c *DestructureIsPatternContext)

	// EnterIdentIsPattern is called when entering the identIsPattern production.
	EnterIdentIsPattern(c *IdentIsPatternContext)

	// EnterPatternFields is called when entering the patternFields production.
	EnterPatternFields(c *PatternFieldsContext)

	// EnterGoExpr is called when entering the goExpr production.
	EnterGoExpr(c *GoExprContext)

	// EnterUnsafeBlock is called when entering the unsafeBlock production.
	EnterUnsafeBlock(c *UnsafeBlockContext)

	// ExitCompilationUnit is called when exiting the compilationUnit production.
	ExitCompilationUnit(c *CompilationUnitContext)

	// ExitCatalogImport is called when exiting the catalogImport production.
	ExitCatalogImport(c *CatalogImportContext)

	// ExitSourcedImport is called when exiting the sourcedImport production.
	ExitSourcedImport(c *SourcedImportContext)

	// ExitDeclaration is called when exiting the declaration production.
	ExitDeclaration(c *DeclarationContext)

	// ExitBindingName is called when exiting the bindingName production.
	ExitBindingName(c *BindingNameContext)

	// ExitStringLiteral is called when exiting the stringLiteral production.
	ExitStringLiteral(c *StringLiteralContext)

	// ExitStringPart is called when exiting the stringPart production.
	ExitStringPart(c *StringPartContext)

	// ExitTypeDecl is called when exiting the typeDecl production.
	ExitTypeDecl(c *TypeDeclContext)

	// ExitInheritance is called when exiting the inheritance production.
	ExitInheritance(c *InheritanceContext)

	// ExitTypeParams is called when exiting the typeParams production.
	ExitTypeParams(c *TypeParamsContext)

	// ExitTypeParam is called when exiting the typeParam production.
	ExitTypeParam(c *TypeParamContext)

	// ExitTypeConstraint is called when exiting the typeConstraint production.
	ExitTypeConstraint(c *TypeConstraintContext)

	// ExitTypeMember is called when exiting the typeMember production.
	ExitTypeMember(c *TypeMemberContext)

	// ExitFieldDecl is called when exiting the fieldDecl production.
	ExitFieldDecl(c *FieldDeclContext)

	// ExitMethodDecl is called when exiting the methodDecl production.
	ExitMethodDecl(c *MethodDeclContext)

	// ExitGetterDecl is called when exiting the getterDecl production.
	ExitGetterDecl(c *GetterDeclContext)

	// ExitSetterDecl is called when exiting the setterDecl production.
	ExitSetterDecl(c *SetterDeclContext)

	// ExitMemberBody is called when exiting the memberBody production.
	ExitMemberBody(c *MemberBodyContext)

	// ExitMethodName is called when exiting the methodName production.
	ExitMethodName(c *MethodNameContext)

	// ExitEnumDecl is called when exiting the enumDecl production.
	ExitEnumDecl(c *EnumDeclContext)

	// ExitEnumVariant is called when exiting the enumVariant production.
	ExitEnumVariant(c *EnumVariantContext)

	// ExitEnumField is called when exiting the enumField production.
	ExitEnumField(c *EnumFieldContext)

	// ExitEnumMember is called when exiting the enumMember production.
	ExitEnumMember(c *EnumMemberContext)

	// ExitFuncDecl is called when exiting the funcDecl production.
	ExitFuncDecl(c *FuncDeclContext)

	// ExitReturnType is called when exiting the returnType production.
	ExitReturnType(c *ReturnTypeContext)

	// ExitParams is called when exiting the params production.
	ExitParams(c *ParamsContext)

	// ExitParamList is called when exiting the paramList production.
	ExitParamList(c *ParamListContext)

	// ExitReceiverParam is called when exiting the receiverParam production.
	ExitReceiverParam(c *ReceiverParamContext)

	// ExitVariadicParam is called when exiting the variadicParam production.
	ExitVariadicParam(c *VariadicParamContext)

	// ExitMoveParam is called when exiting the moveParam production.
	ExitMoveParam(c *MoveParamContext)

	// ExitRegularParam is called when exiting the regularParam production.
	ExitRegularParam(c *RegularParamContext)

	// ExitRefMod is called when exiting the refMod production.
	ExitRefMod(c *RefModContext)

	// ExitArgs is called when exiting the args production.
	ExitArgs(c *ArgsContext)

	// ExitArgList is called when exiting the argList production.
	ExitArgList(c *ArgListContext)

	// ExitArg is called when exiting the arg production.
	ExitArg(c *ArgContext)

	// ExitMetaAnnotation is called when exiting the metaAnnotation production.
	ExitMetaAnnotation(c *MetaAnnotationContext)

	// ExitMetaParams is called when exiting the metaParams production.
	ExitMetaParams(c *MetaParamsContext)

	// ExitMetaParam is called when exiting the metaParam production.
	ExitMetaParam(c *MetaParamContext)

	// ExitNamedType is called when exiting the namedType production.
	ExitNamedType(c *NamedTypeContext)

	// ExitQualifiedType is called when exiting the qualifiedType production.
	ExitQualifiedType(c *QualifiedTypeContext)

	// ExitArrayType is called when exiting the arrayType production.
	ExitArrayType(c *ArrayTypeContext)

	// ExitMutRefType is called when exiting the mutRefType production.
	ExitMutRefType(c *MutRefTypeContext)

	// ExitPointerType is called when exiting the pointerType production.
	ExitPointerType(c *PointerTypeContext)

	// ExitOptionalType is called when exiting the optionalType production.
	ExitOptionalType(c *OptionalTypeContext)

	// ExitTupleType is called when exiting the tupleType production.
	ExitTupleType(c *TupleTypeContext)

	// ExitFunctionType is called when exiting the functionType production.
	ExitFunctionType(c *FunctionTypeContext)

	// ExitParenType is called when exiting the parenType production.
	ExitParenType(c *ParenTypeContext)

	// ExitSharedRefType is called when exiting the sharedRefType production.
	ExitSharedRefType(c *SharedRefTypeContext)

	// ExitSliceType is called when exiting the sliceType production.
	ExitSliceType(c *SliceTypeContext)

	// ExitFuncTypeReturn is called when exiting the funcTypeReturn production.
	ExitFuncTypeReturn(c *FuncTypeReturnContext)

	// ExitTypeArgs is called when exiting the typeArgs production.
	ExitTypeArgs(c *TypeArgsContext)

	// ExitTypeRefList is called when exiting the typeRefList production.
	ExitTypeRefList(c *TypeRefListContext)

	// ExitBlock is called when exiting the block production.
	ExitBlock(c *BlockContext)

	// ExitSemi is called when exiting the semi production.
	ExitSemi(c *SemiContext)

	// ExitStatement is called when exiting the statement production.
	ExitStatement(c *StatementContext)

	// ExitUseVarDecl is called when exiting the useVarDecl production.
	ExitUseVarDecl(c *UseVarDeclContext)

	// ExitTypedVarDecl is called when exiting the typedVarDecl production.
	ExitTypedVarDecl(c *TypedVarDeclContext)

	// ExitInferredVarDecl is called when exiting the inferredVarDecl production.
	ExitInferredVarDecl(c *InferredVarDeclContext)

	// ExitDestructureVarDecl is called when exiting the destructureVarDecl production.
	ExitDestructureVarDecl(c *DestructureVarDeclContext)

	// ExitAssignmentStmt is called when exiting the assignmentStmt production.
	ExitAssignmentStmt(c *AssignmentStmtContext)

	// ExitAssignOp is called when exiting the assignOp production.
	ExitAssignOp(c *AssignOpContext)

	// ExitIncDecStmt is called when exiting the incDecStmt production.
	ExitIncDecStmt(c *IncDecStmtContext)

	// ExitReturnStmt is called when exiting the returnStmt production.
	ExitReturnStmt(c *ReturnStmtContext)

	// ExitRaiseStmt is called when exiting the raiseStmt production.
	ExitRaiseStmt(c *RaiseStmtContext)

	// ExitBreakStmt is called when exiting the breakStmt production.
	ExitBreakStmt(c *BreakStmtContext)

	// ExitContinueStmt is called when exiting the continueStmt production.
	ExitContinueStmt(c *ContinueStmtContext)

	// ExitYieldStmt is called when exiting the yieldStmt production.
	ExitYieldStmt(c *YieldStmtContext)

	// ExitYieldDelegateStmt is called when exiting the yieldDelegateStmt production.
	ExitYieldDelegateStmt(c *YieldDelegateStmtContext)

	// ExitExpressionStmt is called when exiting the expressionStmt production.
	ExitExpressionStmt(c *ExpressionStmtContext)

	// ExitIfStmt is called when exiting the ifStmt production.
	ExitIfStmt(c *IfStmtContext)

	// ExitIfUnwrapCond is called when exiting the ifUnwrapCond production.
	ExitIfUnwrapCond(c *IfUnwrapCondContext)

	// ExitIfExprCond is called when exiting the ifExprCond production.
	ExitIfExprCond(c *IfExprCondContext)

	// ExitElseClause is called when exiting the elseClause production.
	ExitElseClause(c *ElseClauseContext)

	// ExitForInStmt is called when exiting the forInStmt production.
	ExitForInStmt(c *ForInStmtContext)

	// ExitClassicForStmt is called when exiting the classicForStmt production.
	ExitClassicForStmt(c *ClassicForStmtContext)

	// ExitInfiniteLoopStmt is called when exiting the infiniteLoopStmt production.
	ExitInfiniteLoopStmt(c *InfiniteLoopStmtContext)

	// ExitForInitTyped is called when exiting the forInitTyped production.
	ExitForInitTyped(c *ForInitTypedContext)

	// ExitForInitInferred is called when exiting the forInitInferred production.
	ExitForInitInferred(c *ForInitInferredContext)

	// ExitForUpdateAssign is called when exiting the forUpdateAssign production.
	ExitForUpdateAssign(c *ForUpdateAssignContext)

	// ExitForUpdateIncDec is called when exiting the forUpdateIncDec production.
	ExitForUpdateIncDec(c *ForUpdateIncDecContext)

	// ExitForUpdateExpr is called when exiting the forUpdateExpr production.
	ExitForUpdateExpr(c *ForUpdateExprContext)

	// ExitWhileUnwrapStmt is called when exiting the whileUnwrapStmt production.
	ExitWhileUnwrapStmt(c *WhileUnwrapStmtContext)

	// ExitWhileExprStmt is called when exiting the whileExprStmt production.
	ExitWhileExprStmt(c *WhileExprStmtContext)

	// ExitSelectStmt is called when exiting the selectStmt production.
	ExitSelectStmt(c *SelectStmtContext)

	// ExitSelectCase is called when exiting the selectCase production.
	ExitSelectCase(c *SelectCaseContext)

	// ExitSelectDefault is called when exiting the selectDefault production.
	ExitSelectDefault(c *SelectDefaultContext)

	// ExitOptionalUnwrapExpr is called when exiting the optionalUnwrapExpr production.
	ExitOptionalUnwrapExpr(c *OptionalUnwrapExprContext)

	// ExitSliceTypeExpr is called when exiting the sliceTypeExpr production.
	ExitSliceTypeExpr(c *SliceTypeExprContext)

	// ExitCastExpr is called when exiting the castExpr production.
	ExitCastExpr(c *CastExprContext)

	// ExitUnaryNegExpr is called when exiting the unaryNegExpr production.
	ExitUnaryNegExpr(c *UnaryNegExprContext)

	// ExitAdditiveExpr is called when exiting the additiveExpr production.
	ExitAdditiveExpr(c *AdditiveExprContext)

	// ExitBitwiseNotExpr is called when exiting the bitwiseNotExpr production.
	ExitBitwiseNotExpr(c *BitwiseNotExprContext)

	// ExitTypeInstCallExpr is called when exiting the typeInstCallExpr production.
	ExitTypeInstCallExpr(c *TypeInstCallExprContext)

	// ExitErrorPanicExpr is called when exiting the errorPanicExpr production.
	ExitErrorPanicExpr(c *ErrorPanicExprContext)

	// ExitPrimaryExpr is called when exiting the primaryExpr production.
	ExitPrimaryExpr(c *PrimaryExprContext)

	// ExitExclusiveRangeExpr is called when exiting the exclusiveRangeExpr production.
	ExitExclusiveRangeExpr(c *ExclusiveRangeExprContext)

	// ExitMemberAccessExpr is called when exiting the memberAccessExpr production.
	ExitMemberAccessExpr(c *MemberAccessExprContext)

	// ExitErrorPropagateExpr is called when exiting the errorPropagateExpr production.
	ExitErrorPropagateExpr(c *ErrorPropagateExprContext)

	// ExitCallExpr is called when exiting the callExpr production.
	ExitCallExpr(c *CallExprContext)

	// ExitIsExpr is called when exiting the isExpr production.
	ExitIsExpr(c *IsExprContext)

	// ExitReceiveExpr is called when exiting the receiveExpr production.
	ExitReceiveExpr(c *ReceiveExprContext)

	// ExitErrorHandlerExpr is called when exiting the errorHandlerExpr production.
	ExitErrorHandlerExpr(c *ErrorHandlerExprContext)

	// ExitInclusiveRangeExpr is called when exiting the inclusiveRangeExpr production.
	ExitInclusiveRangeExpr(c *InclusiveRangeExprContext)

	// ExitLogicalAndExpr is called when exiting the logicalAndExpr production.
	ExitLogicalAndExpr(c *LogicalAndExprContext)

	// ExitComparisonExpr is called when exiting the comparisonExpr production.
	ExitComparisonExpr(c *ComparisonExprContext)

	// ExitSliceExpr is called when exiting the sliceExpr production.
	ExitSliceExpr(c *SliceExprContext)

	// ExitElvisExpr is called when exiting the elvisExpr production.
	ExitElvisExpr(c *ElvisExprContext)

	// ExitShiftExpr is called when exiting the shiftExpr production.
	ExitShiftExpr(c *ShiftExprContext)

	// ExitBitwiseExpr is called when exiting the bitwiseExpr production.
	ExitBitwiseExpr(c *BitwiseExprContext)

	// ExitLogicalOrExpr is called when exiting the logicalOrExpr production.
	ExitLogicalOrExpr(c *LogicalOrExprContext)

	// ExitIndexExpr is called when exiting the indexExpr production.
	ExitIndexExpr(c *IndexExprContext)

	// ExitOptionalChainExpr is called when exiting the optionalChainExpr production.
	ExitOptionalChainExpr(c *OptionalChainExprContext)

	// ExitUnaryNotExpr is called when exiting the unaryNotExpr production.
	ExitUnaryNotExpr(c *UnaryNotExprContext)

	// ExitMultiplicativeExpr is called when exiting the multiplicativeExpr production.
	ExitMultiplicativeExpr(c *MultiplicativeExprContext)

	// ExitEqualityExpr is called when exiting the equalityExpr production.
	ExitEqualityExpr(c *EqualityExprContext)

	// ExitIntLiteral is called when exiting the intLiteral production.
	ExitIntLiteral(c *IntLiteralContext)

	// ExitFloatLiteral is called when exiting the floatLiteral production.
	ExitFloatLiteral(c *FloatLiteralContext)

	// ExitTrueLiteral is called when exiting the trueLiteral production.
	ExitTrueLiteral(c *TrueLiteralContext)

	// ExitFalseLiteral is called when exiting the falseLiteral production.
	ExitFalseLiteral(c *FalseLiteralContext)

	// ExitNoneLiteral is called when exiting the noneLiteral production.
	ExitNoneLiteral(c *NoneLiteralContext)

	// ExitCharLiteral is called when exiting the charLiteral production.
	ExitCharLiteral(c *CharLiteralContext)

	// ExitStringLit is called when exiting the stringLit production.
	ExitStringLit(c *StringLitContext)

	// ExitIdentExpr is called when exiting the identExpr production.
	ExitIdentExpr(c *IdentExprContext)

	// ExitThisExpr is called when exiting the thisExpr production.
	ExitThisExpr(c *ThisExprContext)

	// ExitParenExpr is called when exiting the parenExpr production.
	ExitParenExpr(c *ParenExprContext)

	// ExitTupleLiteral is called when exiting the tupleLiteral production.
	ExitTupleLiteral(c *TupleLiteralContext)

	// ExitArrayLiteral is called when exiting the arrayLiteral production.
	ExitArrayLiteral(c *ArrayLiteralContext)

	// ExitMapLiteral is called when exiting the mapLiteral production.
	ExitMapLiteral(c *MapLiteralContext)

	// ExitLambda is called when exiting the lambda production.
	ExitLambda(c *LambdaContext)

	// ExitIfExpression is called when exiting the ifExpression production.
	ExitIfExpression(c *IfExpressionContext)

	// ExitMatchExpression is called when exiting the matchExpression production.
	ExitMatchExpression(c *MatchExpressionContext)

	// ExitGoExpression is called when exiting the goExpression production.
	ExitGoExpression(c *GoExpressionContext)

	// ExitUnsafeExpression is called when exiting the unsafeExpression production.
	ExitUnsafeExpression(c *UnsafeExpressionContext)

	// ExitMapEntry is called when exiting the mapEntry production.
	ExitMapEntry(c *MapEntryContext)

	// ExitLambdaExpr is called when exiting the lambdaExpr production.
	ExitLambdaExpr(c *LambdaExprContext)

	// ExitLambdaParams is called when exiting the lambdaParams production.
	ExitLambdaParams(c *LambdaParamsContext)

	// ExitTypedLambdaParam is called when exiting the typedLambdaParam production.
	ExitTypedLambdaParam(c *TypedLambdaParamContext)

	// ExitUntypedLambdaParam is called when exiting the untypedLambdaParam production.
	ExitUntypedLambdaParam(c *UntypedLambdaParamContext)

	// ExitIfExpr is called when exiting the ifExpr production.
	ExitIfExpr(c *IfExprContext)

	// ExitMatchExpr is called when exiting the matchExpr production.
	ExitMatchExpr(c *MatchExprContext)

	// ExitMatchArm is called when exiting the matchArm production.
	ExitMatchArm(c *MatchArmContext)

	// ExitEnumDestructurePattern is called when exiting the enumDestructurePattern production.
	ExitEnumDestructurePattern(c *EnumDestructurePatternContext)

	// ExitEnumVariantPattern is called when exiting the enumVariantPattern production.
	ExitEnumVariantPattern(c *EnumVariantPatternContext)

	// ExitTypeBindingPattern is called when exiting the typeBindingPattern production.
	ExitTypeBindingPattern(c *TypeBindingPatternContext)

	// ExitShortDestructurePattern is called when exiting the shortDestructurePattern production.
	ExitShortDestructurePattern(c *ShortDestructurePatternContext)

	// ExitNamePattern is called when exiting the namePattern production.
	ExitNamePattern(c *NamePatternContext)

	// ExitIntLiteralPattern is called when exiting the intLiteralPattern production.
	ExitIntLiteralPattern(c *IntLiteralPatternContext)

	// ExitFloatLiteralPattern is called when exiting the floatLiteralPattern production.
	ExitFloatLiteralPattern(c *FloatLiteralPatternContext)

	// ExitCharLiteralPattern is called when exiting the charLiteralPattern production.
	ExitCharLiteralPattern(c *CharLiteralPatternContext)

	// ExitTrueLiteralPattern is called when exiting the trueLiteralPattern production.
	ExitTrueLiteralPattern(c *TrueLiteralPatternContext)

	// ExitFalseLiteralPattern is called when exiting the falseLiteralPattern production.
	ExitFalseLiteralPattern(c *FalseLiteralPatternContext)

	// ExitNoneLiteralPattern is called when exiting the noneLiteralPattern production.
	ExitNoneLiteralPattern(c *NoneLiteralPatternContext)

	// ExitStringLiteralPattern is called when exiting the stringLiteralPattern production.
	ExitStringLiteralPattern(c *StringLiteralPatternContext)

	// ExitWildcardPattern is called when exiting the wildcardPattern production.
	ExitWildcardPattern(c *WildcardPatternContext)

	// ExitExpressionPattern is called when exiting the expressionPattern production.
	ExitExpressionPattern(c *ExpressionPatternContext)

	// ExitDestructureIsPattern is called when exiting the destructureIsPattern production.
	ExitDestructureIsPattern(c *DestructureIsPatternContext)

	// ExitIdentIsPattern is called when exiting the identIsPattern production.
	ExitIdentIsPattern(c *IdentIsPatternContext)

	// ExitPatternFields is called when exiting the patternFields production.
	ExitPatternFields(c *PatternFieldsContext)

	// ExitGoExpr is called when exiting the goExpr production.
	ExitGoExpr(c *GoExprContext)

	// ExitUnsafeBlock is called when exiting the unsafeBlock production.
	ExitUnsafeBlock(c *UnsafeBlockContext)
}

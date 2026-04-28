// Code generated from PromiseParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package parser // PromiseParser
import "github.com/antlr4-go/antlr/v4"

// BasePromiseParserListener is a complete listener for a parse tree produced by PromiseParser.
type BasePromiseParserListener struct{}

var _ PromiseParserListener = &BasePromiseParserListener{}

// VisitTerminal is called when a terminal node is visited.
func (s *BasePromiseParserListener) VisitTerminal(node antlr.TerminalNode) {}

// VisitErrorNode is called when an error node is visited.
func (s *BasePromiseParserListener) VisitErrorNode(node antlr.ErrorNode) {}

// EnterEveryRule is called when any rule is entered.
func (s *BasePromiseParserListener) EnterEveryRule(ctx antlr.ParserRuleContext) {}

// ExitEveryRule is called when any rule is exited.
func (s *BasePromiseParserListener) ExitEveryRule(ctx antlr.ParserRuleContext) {}

// EnterCompilationUnit is called when production compilationUnit is entered.
func (s *BasePromiseParserListener) EnterCompilationUnit(ctx *CompilationUnitContext) {}

// ExitCompilationUnit is called when production compilationUnit is exited.
func (s *BasePromiseParserListener) ExitCompilationUnit(ctx *CompilationUnitContext) {}

// EnterUseDecl is called when production useDecl is entered.
func (s *BasePromiseParserListener) EnterUseDecl(ctx *UseDeclContext) {}

// ExitUseDecl is called when production useDecl is exited.
func (s *BasePromiseParserListener) ExitUseDecl(ctx *UseDeclContext) {}

// EnterDeclaration is called when production declaration is entered.
func (s *BasePromiseParserListener) EnterDeclaration(ctx *DeclarationContext) {}

// ExitDeclaration is called when production declaration is exited.
func (s *BasePromiseParserListener) ExitDeclaration(ctx *DeclarationContext) {}

// EnterBindingName is called when production bindingName is entered.
func (s *BasePromiseParserListener) EnterBindingName(ctx *BindingNameContext) {}

// ExitBindingName is called when production bindingName is exited.
func (s *BasePromiseParserListener) ExitBindingName(ctx *BindingNameContext) {}

// EnterStringLiteral is called when production stringLiteral is entered.
func (s *BasePromiseParserListener) EnterStringLiteral(ctx *StringLiteralContext) {}

// ExitStringLiteral is called when production stringLiteral is exited.
func (s *BasePromiseParserListener) ExitStringLiteral(ctx *StringLiteralContext) {}

// EnterStringPart is called when production stringPart is entered.
func (s *BasePromiseParserListener) EnterStringPart(ctx *StringPartContext) {}

// ExitStringPart is called when production stringPart is exited.
func (s *BasePromiseParserListener) ExitStringPart(ctx *StringPartContext) {}

// EnterTypeDecl is called when production typeDecl is entered.
func (s *BasePromiseParserListener) EnterTypeDecl(ctx *TypeDeclContext) {}

// ExitTypeDecl is called when production typeDecl is exited.
func (s *BasePromiseParserListener) ExitTypeDecl(ctx *TypeDeclContext) {}

// EnterInheritance is called when production inheritance is entered.
func (s *BasePromiseParserListener) EnterInheritance(ctx *InheritanceContext) {}

// ExitInheritance is called when production inheritance is exited.
func (s *BasePromiseParserListener) ExitInheritance(ctx *InheritanceContext) {}

// EnterTypeParams is called when production typeParams is entered.
func (s *BasePromiseParserListener) EnterTypeParams(ctx *TypeParamsContext) {}

// ExitTypeParams is called when production typeParams is exited.
func (s *BasePromiseParserListener) ExitTypeParams(ctx *TypeParamsContext) {}

// EnterTypeParam is called when production typeParam is entered.
func (s *BasePromiseParserListener) EnterTypeParam(ctx *TypeParamContext) {}

// ExitTypeParam is called when production typeParam is exited.
func (s *BasePromiseParserListener) ExitTypeParam(ctx *TypeParamContext) {}

// EnterTypeConstraint is called when production typeConstraint is entered.
func (s *BasePromiseParserListener) EnterTypeConstraint(ctx *TypeConstraintContext) {}

// ExitTypeConstraint is called when production typeConstraint is exited.
func (s *BasePromiseParserListener) ExitTypeConstraint(ctx *TypeConstraintContext) {}

// EnterTypeMember is called when production typeMember is entered.
func (s *BasePromiseParserListener) EnterTypeMember(ctx *TypeMemberContext) {}

// ExitTypeMember is called when production typeMember is exited.
func (s *BasePromiseParserListener) ExitTypeMember(ctx *TypeMemberContext) {}

// EnterFieldDecl is called when production fieldDecl is entered.
func (s *BasePromiseParserListener) EnterFieldDecl(ctx *FieldDeclContext) {}

// ExitFieldDecl is called when production fieldDecl is exited.
func (s *BasePromiseParserListener) ExitFieldDecl(ctx *FieldDeclContext) {}

// EnterMethodDecl is called when production methodDecl is entered.
func (s *BasePromiseParserListener) EnterMethodDecl(ctx *MethodDeclContext) {}

// ExitMethodDecl is called when production methodDecl is exited.
func (s *BasePromiseParserListener) ExitMethodDecl(ctx *MethodDeclContext) {}

// EnterGetterDecl is called when production getterDecl is entered.
func (s *BasePromiseParserListener) EnterGetterDecl(ctx *GetterDeclContext) {}

// ExitGetterDecl is called when production getterDecl is exited.
func (s *BasePromiseParserListener) ExitGetterDecl(ctx *GetterDeclContext) {}

// EnterSetterDecl is called when production setterDecl is entered.
func (s *BasePromiseParserListener) EnterSetterDecl(ctx *SetterDeclContext) {}

// ExitSetterDecl is called when production setterDecl is exited.
func (s *BasePromiseParserListener) ExitSetterDecl(ctx *SetterDeclContext) {}

// EnterMemberBody is called when production memberBody is entered.
func (s *BasePromiseParserListener) EnterMemberBody(ctx *MemberBodyContext) {}

// ExitMemberBody is called when production memberBody is exited.
func (s *BasePromiseParserListener) ExitMemberBody(ctx *MemberBodyContext) {}

// EnterMethodName is called when production methodName is entered.
func (s *BasePromiseParserListener) EnterMethodName(ctx *MethodNameContext) {}

// ExitMethodName is called when production methodName is exited.
func (s *BasePromiseParserListener) ExitMethodName(ctx *MethodNameContext) {}

// EnterEnumDecl is called when production enumDecl is entered.
func (s *BasePromiseParserListener) EnterEnumDecl(ctx *EnumDeclContext) {}

// ExitEnumDecl is called when production enumDecl is exited.
func (s *BasePromiseParserListener) ExitEnumDecl(ctx *EnumDeclContext) {}

// EnterEnumVariant is called when production enumVariant is entered.
func (s *BasePromiseParserListener) EnterEnumVariant(ctx *EnumVariantContext) {}

// ExitEnumVariant is called when production enumVariant is exited.
func (s *BasePromiseParserListener) ExitEnumVariant(ctx *EnumVariantContext) {}

// EnterEnumField is called when production enumField is entered.
func (s *BasePromiseParserListener) EnterEnumField(ctx *EnumFieldContext) {}

// ExitEnumField is called when production enumField is exited.
func (s *BasePromiseParserListener) ExitEnumField(ctx *EnumFieldContext) {}

// EnterFuncDecl is called when production funcDecl is entered.
func (s *BasePromiseParserListener) EnterFuncDecl(ctx *FuncDeclContext) {}

// ExitFuncDecl is called when production funcDecl is exited.
func (s *BasePromiseParserListener) ExitFuncDecl(ctx *FuncDeclContext) {}

// EnterReturnType is called when production returnType is entered.
func (s *BasePromiseParserListener) EnterReturnType(ctx *ReturnTypeContext) {}

// ExitReturnType is called when production returnType is exited.
func (s *BasePromiseParserListener) ExitReturnType(ctx *ReturnTypeContext) {}

// EnterParams is called when production params is entered.
func (s *BasePromiseParserListener) EnterParams(ctx *ParamsContext) {}

// ExitParams is called when production params is exited.
func (s *BasePromiseParserListener) ExitParams(ctx *ParamsContext) {}

// EnterParamList is called when production paramList is entered.
func (s *BasePromiseParserListener) EnterParamList(ctx *ParamListContext) {}

// ExitParamList is called when production paramList is exited.
func (s *BasePromiseParserListener) ExitParamList(ctx *ParamListContext) {}

// EnterReceiverParam is called when production receiverParam is entered.
func (s *BasePromiseParserListener) EnterReceiverParam(ctx *ReceiverParamContext) {}

// ExitReceiverParam is called when production receiverParam is exited.
func (s *BasePromiseParserListener) ExitReceiverParam(ctx *ReceiverParamContext) {}

// EnterParam is called when production param is entered.
func (s *BasePromiseParserListener) EnterParam(ctx *ParamContext) {}

// ExitParam is called when production param is exited.
func (s *BasePromiseParserListener) ExitParam(ctx *ParamContext) {}

// EnterRefMod is called when production refMod is entered.
func (s *BasePromiseParserListener) EnterRefMod(ctx *RefModContext) {}

// ExitRefMod is called when production refMod is exited.
func (s *BasePromiseParserListener) ExitRefMod(ctx *RefModContext) {}

// EnterArgs is called when production args is entered.
func (s *BasePromiseParserListener) EnterArgs(ctx *ArgsContext) {}

// ExitArgs is called when production args is exited.
func (s *BasePromiseParserListener) ExitArgs(ctx *ArgsContext) {}

// EnterArgList is called when production argList is entered.
func (s *BasePromiseParserListener) EnterArgList(ctx *ArgListContext) {}

// ExitArgList is called when production argList is exited.
func (s *BasePromiseParserListener) ExitArgList(ctx *ArgListContext) {}

// EnterArg is called when production arg is entered.
func (s *BasePromiseParserListener) EnterArg(ctx *ArgContext) {}

// ExitArg is called when production arg is exited.
func (s *BasePromiseParserListener) ExitArg(ctx *ArgContext) {}

// EnterMetaAnnotation is called when production metaAnnotation is entered.
func (s *BasePromiseParserListener) EnterMetaAnnotation(ctx *MetaAnnotationContext) {}

// ExitMetaAnnotation is called when production metaAnnotation is exited.
func (s *BasePromiseParserListener) ExitMetaAnnotation(ctx *MetaAnnotationContext) {}

// EnterMetaParams is called when production metaParams is entered.
func (s *BasePromiseParserListener) EnterMetaParams(ctx *MetaParamsContext) {}

// ExitMetaParams is called when production metaParams is exited.
func (s *BasePromiseParserListener) ExitMetaParams(ctx *MetaParamsContext) {}

// EnterMetaParam is called when production metaParam is entered.
func (s *BasePromiseParserListener) EnterMetaParam(ctx *MetaParamContext) {}

// ExitMetaParam is called when production metaParam is exited.
func (s *BasePromiseParserListener) ExitMetaParam(ctx *MetaParamContext) {}

// EnterNamedType is called when production namedType is entered.
func (s *BasePromiseParserListener) EnterNamedType(ctx *NamedTypeContext) {}

// ExitNamedType is called when production namedType is exited.
func (s *BasePromiseParserListener) ExitNamedType(ctx *NamedTypeContext) {}

// EnterArrayType is called when production arrayType is entered.
func (s *BasePromiseParserListener) EnterArrayType(ctx *ArrayTypeContext) {}

// ExitArrayType is called when production arrayType is exited.
func (s *BasePromiseParserListener) ExitArrayType(ctx *ArrayTypeContext) {}

// EnterMutRefType is called when production mutRefType is entered.
func (s *BasePromiseParserListener) EnterMutRefType(ctx *MutRefTypeContext) {}

// ExitMutRefType is called when production mutRefType is exited.
func (s *BasePromiseParserListener) ExitMutRefType(ctx *MutRefTypeContext) {}

// EnterPointerType is called when production pointerType is entered.
func (s *BasePromiseParserListener) EnterPointerType(ctx *PointerTypeContext) {}

// ExitPointerType is called when production pointerType is exited.
func (s *BasePromiseParserListener) ExitPointerType(ctx *PointerTypeContext) {}

// EnterOptionalType is called when production optionalType is entered.
func (s *BasePromiseParserListener) EnterOptionalType(ctx *OptionalTypeContext) {}

// ExitOptionalType is called when production optionalType is exited.
func (s *BasePromiseParserListener) ExitOptionalType(ctx *OptionalTypeContext) {}

// EnterTupleType is called when production tupleType is entered.
func (s *BasePromiseParserListener) EnterTupleType(ctx *TupleTypeContext) {}

// ExitTupleType is called when production tupleType is exited.
func (s *BasePromiseParserListener) ExitTupleType(ctx *TupleTypeContext) {}

// EnterFunctionType is called when production functionType is entered.
func (s *BasePromiseParserListener) EnterFunctionType(ctx *FunctionTypeContext) {}

// ExitFunctionType is called when production functionType is exited.
func (s *BasePromiseParserListener) ExitFunctionType(ctx *FunctionTypeContext) {}

// EnterParenType is called when production parenType is entered.
func (s *BasePromiseParserListener) EnterParenType(ctx *ParenTypeContext) {}

// ExitParenType is called when production parenType is exited.
func (s *BasePromiseParserListener) ExitParenType(ctx *ParenTypeContext) {}

// EnterSharedRefType is called when production sharedRefType is entered.
func (s *BasePromiseParserListener) EnterSharedRefType(ctx *SharedRefTypeContext) {}

// ExitSharedRefType is called when production sharedRefType is exited.
func (s *BasePromiseParserListener) ExitSharedRefType(ctx *SharedRefTypeContext) {}

// EnterSliceType is called when production sliceType is entered.
func (s *BasePromiseParserListener) EnterSliceType(ctx *SliceTypeContext) {}

// ExitSliceType is called when production sliceType is exited.
func (s *BasePromiseParserListener) ExitSliceType(ctx *SliceTypeContext) {}

// EnterTypeArgs is called when production typeArgs is entered.
func (s *BasePromiseParserListener) EnterTypeArgs(ctx *TypeArgsContext) {}

// ExitTypeArgs is called when production typeArgs is exited.
func (s *BasePromiseParserListener) ExitTypeArgs(ctx *TypeArgsContext) {}

// EnterTypeRefList is called when production typeRefList is entered.
func (s *BasePromiseParserListener) EnterTypeRefList(ctx *TypeRefListContext) {}

// ExitTypeRefList is called when production typeRefList is exited.
func (s *BasePromiseParserListener) ExitTypeRefList(ctx *TypeRefListContext) {}

// EnterBlock is called when production block is entered.
func (s *BasePromiseParserListener) EnterBlock(ctx *BlockContext) {}

// ExitBlock is called when production block is exited.
func (s *BasePromiseParserListener) ExitBlock(ctx *BlockContext) {}

// EnterStatement is called when production statement is entered.
func (s *BasePromiseParserListener) EnterStatement(ctx *StatementContext) {}

// ExitStatement is called when production statement is exited.
func (s *BasePromiseParserListener) ExitStatement(ctx *StatementContext) {}

// EnterUseVarDecl is called when production useVarDecl is entered.
func (s *BasePromiseParserListener) EnterUseVarDecl(ctx *UseVarDeclContext) {}

// ExitUseVarDecl is called when production useVarDecl is exited.
func (s *BasePromiseParserListener) ExitUseVarDecl(ctx *UseVarDeclContext) {}

// EnterTypedVarDecl is called when production typedVarDecl is entered.
func (s *BasePromiseParserListener) EnterTypedVarDecl(ctx *TypedVarDeclContext) {}

// ExitTypedVarDecl is called when production typedVarDecl is exited.
func (s *BasePromiseParserListener) ExitTypedVarDecl(ctx *TypedVarDeclContext) {}

// EnterInferredVarDecl is called when production inferredVarDecl is entered.
func (s *BasePromiseParserListener) EnterInferredVarDecl(ctx *InferredVarDeclContext) {}

// ExitInferredVarDecl is called when production inferredVarDecl is exited.
func (s *BasePromiseParserListener) ExitInferredVarDecl(ctx *InferredVarDeclContext) {}

// EnterDestructureVarDecl is called when production destructureVarDecl is entered.
func (s *BasePromiseParserListener) EnterDestructureVarDecl(ctx *DestructureVarDeclContext) {}

// ExitDestructureVarDecl is called when production destructureVarDecl is exited.
func (s *BasePromiseParserListener) ExitDestructureVarDecl(ctx *DestructureVarDeclContext) {}

// EnterAssignmentStmt is called when production assignmentStmt is entered.
func (s *BasePromiseParserListener) EnterAssignmentStmt(ctx *AssignmentStmtContext) {}

// ExitAssignmentStmt is called when production assignmentStmt is exited.
func (s *BasePromiseParserListener) ExitAssignmentStmt(ctx *AssignmentStmtContext) {}

// EnterAssignOp is called when production assignOp is entered.
func (s *BasePromiseParserListener) EnterAssignOp(ctx *AssignOpContext) {}

// ExitAssignOp is called when production assignOp is exited.
func (s *BasePromiseParserListener) ExitAssignOp(ctx *AssignOpContext) {}

// EnterIncDecStmt is called when production incDecStmt is entered.
func (s *BasePromiseParserListener) EnterIncDecStmt(ctx *IncDecStmtContext) {}

// ExitIncDecStmt is called when production incDecStmt is exited.
func (s *BasePromiseParserListener) ExitIncDecStmt(ctx *IncDecStmtContext) {}

// EnterReturnStmt is called when production returnStmt is entered.
func (s *BasePromiseParserListener) EnterReturnStmt(ctx *ReturnStmtContext) {}

// ExitReturnStmt is called when production returnStmt is exited.
func (s *BasePromiseParserListener) ExitReturnStmt(ctx *ReturnStmtContext) {}

// EnterRaiseStmt is called when production raiseStmt is entered.
func (s *BasePromiseParserListener) EnterRaiseStmt(ctx *RaiseStmtContext) {}

// ExitRaiseStmt is called when production raiseStmt is exited.
func (s *BasePromiseParserListener) ExitRaiseStmt(ctx *RaiseStmtContext) {}

// EnterBreakStmt is called when production breakStmt is entered.
func (s *BasePromiseParserListener) EnterBreakStmt(ctx *BreakStmtContext) {}

// ExitBreakStmt is called when production breakStmt is exited.
func (s *BasePromiseParserListener) ExitBreakStmt(ctx *BreakStmtContext) {}

// EnterContinueStmt is called when production continueStmt is entered.
func (s *BasePromiseParserListener) EnterContinueStmt(ctx *ContinueStmtContext) {}

// ExitContinueStmt is called when production continueStmt is exited.
func (s *BasePromiseParserListener) ExitContinueStmt(ctx *ContinueStmtContext) {}

// EnterYieldStmt is called when production yieldStmt is entered.
func (s *BasePromiseParserListener) EnterYieldStmt(ctx *YieldStmtContext) {}

// ExitYieldStmt is called when production yieldStmt is exited.
func (s *BasePromiseParserListener) ExitYieldStmt(ctx *YieldStmtContext) {}

// EnterYieldDelegateStmt is called when production yieldDelegateStmt is entered.
func (s *BasePromiseParserListener) EnterYieldDelegateStmt(ctx *YieldDelegateStmtContext) {}

// ExitYieldDelegateStmt is called when production yieldDelegateStmt is exited.
func (s *BasePromiseParserListener) ExitYieldDelegateStmt(ctx *YieldDelegateStmtContext) {}

// EnterExpressionStmt is called when production expressionStmt is entered.
func (s *BasePromiseParserListener) EnterExpressionStmt(ctx *ExpressionStmtContext) {}

// ExitExpressionStmt is called when production expressionStmt is exited.
func (s *BasePromiseParserListener) ExitExpressionStmt(ctx *ExpressionStmtContext) {}

// EnterIfStmt is called when production ifStmt is entered.
func (s *BasePromiseParserListener) EnterIfStmt(ctx *IfStmtContext) {}

// ExitIfStmt is called when production ifStmt is exited.
func (s *BasePromiseParserListener) ExitIfStmt(ctx *IfStmtContext) {}

// EnterIfUnwrapCond is called when production ifUnwrapCond is entered.
func (s *BasePromiseParserListener) EnterIfUnwrapCond(ctx *IfUnwrapCondContext) {}

// ExitIfUnwrapCond is called when production ifUnwrapCond is exited.
func (s *BasePromiseParserListener) ExitIfUnwrapCond(ctx *IfUnwrapCondContext) {}

// EnterIfExprCond is called when production ifExprCond is entered.
func (s *BasePromiseParserListener) EnterIfExprCond(ctx *IfExprCondContext) {}

// ExitIfExprCond is called when production ifExprCond is exited.
func (s *BasePromiseParserListener) ExitIfExprCond(ctx *IfExprCondContext) {}

// EnterElseClause is called when production elseClause is entered.
func (s *BasePromiseParserListener) EnterElseClause(ctx *ElseClauseContext) {}

// ExitElseClause is called when production elseClause is exited.
func (s *BasePromiseParserListener) ExitElseClause(ctx *ElseClauseContext) {}

// EnterForInStmt is called when production forInStmt is entered.
func (s *BasePromiseParserListener) EnterForInStmt(ctx *ForInStmtContext) {}

// ExitForInStmt is called when production forInStmt is exited.
func (s *BasePromiseParserListener) ExitForInStmt(ctx *ForInStmtContext) {}

// EnterClassicForStmt is called when production classicForStmt is entered.
func (s *BasePromiseParserListener) EnterClassicForStmt(ctx *ClassicForStmtContext) {}

// ExitClassicForStmt is called when production classicForStmt is exited.
func (s *BasePromiseParserListener) ExitClassicForStmt(ctx *ClassicForStmtContext) {}

// EnterInfiniteLoopStmt is called when production infiniteLoopStmt is entered.
func (s *BasePromiseParserListener) EnterInfiniteLoopStmt(ctx *InfiniteLoopStmtContext) {}

// ExitInfiniteLoopStmt is called when production infiniteLoopStmt is exited.
func (s *BasePromiseParserListener) ExitInfiniteLoopStmt(ctx *InfiniteLoopStmtContext) {}

// EnterForInitTyped is called when production forInitTyped is entered.
func (s *BasePromiseParserListener) EnterForInitTyped(ctx *ForInitTypedContext) {}

// ExitForInitTyped is called when production forInitTyped is exited.
func (s *BasePromiseParserListener) ExitForInitTyped(ctx *ForInitTypedContext) {}

// EnterForInitInferred is called when production forInitInferred is entered.
func (s *BasePromiseParserListener) EnterForInitInferred(ctx *ForInitInferredContext) {}

// ExitForInitInferred is called when production forInitInferred is exited.
func (s *BasePromiseParserListener) ExitForInitInferred(ctx *ForInitInferredContext) {}

// EnterForUpdateAssign is called when production forUpdateAssign is entered.
func (s *BasePromiseParserListener) EnterForUpdateAssign(ctx *ForUpdateAssignContext) {}

// ExitForUpdateAssign is called when production forUpdateAssign is exited.
func (s *BasePromiseParserListener) ExitForUpdateAssign(ctx *ForUpdateAssignContext) {}

// EnterForUpdateIncDec is called when production forUpdateIncDec is entered.
func (s *BasePromiseParserListener) EnterForUpdateIncDec(ctx *ForUpdateIncDecContext) {}

// ExitForUpdateIncDec is called when production forUpdateIncDec is exited.
func (s *BasePromiseParserListener) ExitForUpdateIncDec(ctx *ForUpdateIncDecContext) {}

// EnterForUpdateExpr is called when production forUpdateExpr is entered.
func (s *BasePromiseParserListener) EnterForUpdateExpr(ctx *ForUpdateExprContext) {}

// ExitForUpdateExpr is called when production forUpdateExpr is exited.
func (s *BasePromiseParserListener) ExitForUpdateExpr(ctx *ForUpdateExprContext) {}

// EnterWhileUnwrapStmt is called when production whileUnwrapStmt is entered.
func (s *BasePromiseParserListener) EnterWhileUnwrapStmt(ctx *WhileUnwrapStmtContext) {}

// ExitWhileUnwrapStmt is called when production whileUnwrapStmt is exited.
func (s *BasePromiseParserListener) ExitWhileUnwrapStmt(ctx *WhileUnwrapStmtContext) {}

// EnterWhileExprStmt is called when production whileExprStmt is entered.
func (s *BasePromiseParserListener) EnterWhileExprStmt(ctx *WhileExprStmtContext) {}

// ExitWhileExprStmt is called when production whileExprStmt is exited.
func (s *BasePromiseParserListener) ExitWhileExprStmt(ctx *WhileExprStmtContext) {}

// EnterCastExpr is called when production castExpr is entered.
func (s *BasePromiseParserListener) EnterCastExpr(ctx *CastExprContext) {}

// ExitCastExpr is called when production castExpr is exited.
func (s *BasePromiseParserListener) ExitCastExpr(ctx *CastExprContext) {}

// EnterUnaryNegExpr is called when production unaryNegExpr is entered.
func (s *BasePromiseParserListener) EnterUnaryNegExpr(ctx *UnaryNegExprContext) {}

// ExitUnaryNegExpr is called when production unaryNegExpr is exited.
func (s *BasePromiseParserListener) ExitUnaryNegExpr(ctx *UnaryNegExprContext) {}

// EnterAdditiveExpr is called when production additiveExpr is entered.
func (s *BasePromiseParserListener) EnterAdditiveExpr(ctx *AdditiveExprContext) {}

// ExitAdditiveExpr is called when production additiveExpr is exited.
func (s *BasePromiseParserListener) ExitAdditiveExpr(ctx *AdditiveExprContext) {}

// EnterBitwiseNotExpr is called when production bitwiseNotExpr is entered.
func (s *BasePromiseParserListener) EnterBitwiseNotExpr(ctx *BitwiseNotExprContext) {}

// ExitBitwiseNotExpr is called when production bitwiseNotExpr is exited.
func (s *BasePromiseParserListener) ExitBitwiseNotExpr(ctx *BitwiseNotExprContext) {}

// EnterPrimaryExpr is called when production primaryExpr is entered.
func (s *BasePromiseParserListener) EnterPrimaryExpr(ctx *PrimaryExprContext) {}

// ExitPrimaryExpr is called when production primaryExpr is exited.
func (s *BasePromiseParserListener) ExitPrimaryExpr(ctx *PrimaryExprContext) {}

// EnterExclusiveRangeExpr is called when production exclusiveRangeExpr is entered.
func (s *BasePromiseParserListener) EnterExclusiveRangeExpr(ctx *ExclusiveRangeExprContext) {}

// ExitExclusiveRangeExpr is called when production exclusiveRangeExpr is exited.
func (s *BasePromiseParserListener) ExitExclusiveRangeExpr(ctx *ExclusiveRangeExprContext) {}

// EnterMemberAccessExpr is called when production memberAccessExpr is entered.
func (s *BasePromiseParserListener) EnterMemberAccessExpr(ctx *MemberAccessExprContext) {}

// ExitMemberAccessExpr is called when production memberAccessExpr is exited.
func (s *BasePromiseParserListener) ExitMemberAccessExpr(ctx *MemberAccessExprContext) {}

// EnterErrorPropagateExpr is called when production errorPropagateExpr is entered.
func (s *BasePromiseParserListener) EnterErrorPropagateExpr(ctx *ErrorPropagateExprContext) {}

// ExitErrorPropagateExpr is called when production errorPropagateExpr is exited.
func (s *BasePromiseParserListener) ExitErrorPropagateExpr(ctx *ErrorPropagateExprContext) {}

// EnterCallExpr is called when production callExpr is entered.
func (s *BasePromiseParserListener) EnterCallExpr(ctx *CallExprContext) {}

// ExitCallExpr is called when production callExpr is exited.
func (s *BasePromiseParserListener) ExitCallExpr(ctx *CallExprContext) {}

// EnterIsExpr is called when production isExpr is entered.
func (s *BasePromiseParserListener) EnterIsExpr(ctx *IsExprContext) {}

// ExitIsExpr is called when production isExpr is exited.
func (s *BasePromiseParserListener) ExitIsExpr(ctx *IsExprContext) {}

// EnterReceiveExpr is called when production receiveExpr is entered.
func (s *BasePromiseParserListener) EnterReceiveExpr(ctx *ReceiveExprContext) {}

// ExitReceiveExpr is called when production receiveExpr is exited.
func (s *BasePromiseParserListener) ExitReceiveExpr(ctx *ReceiveExprContext) {}

// EnterErrorHandlerExpr is called when production errorHandlerExpr is entered.
func (s *BasePromiseParserListener) EnterErrorHandlerExpr(ctx *ErrorHandlerExprContext) {}

// ExitErrorHandlerExpr is called when production errorHandlerExpr is exited.
func (s *BasePromiseParserListener) ExitErrorHandlerExpr(ctx *ErrorHandlerExprContext) {}

// EnterInclusiveRangeExpr is called when production inclusiveRangeExpr is entered.
func (s *BasePromiseParserListener) EnterInclusiveRangeExpr(ctx *InclusiveRangeExprContext) {}

// ExitInclusiveRangeExpr is called when production inclusiveRangeExpr is exited.
func (s *BasePromiseParserListener) ExitInclusiveRangeExpr(ctx *InclusiveRangeExprContext) {}

// EnterLogicalAndExpr is called when production logicalAndExpr is entered.
func (s *BasePromiseParserListener) EnterLogicalAndExpr(ctx *LogicalAndExprContext) {}

// ExitLogicalAndExpr is called when production logicalAndExpr is exited.
func (s *BasePromiseParserListener) ExitLogicalAndExpr(ctx *LogicalAndExprContext) {}

// EnterComparisonExpr is called when production comparisonExpr is entered.
func (s *BasePromiseParserListener) EnterComparisonExpr(ctx *ComparisonExprContext) {}

// ExitComparisonExpr is called when production comparisonExpr is exited.
func (s *BasePromiseParserListener) ExitComparisonExpr(ctx *ComparisonExprContext) {}

// EnterSliceExpr is called when production sliceExpr is entered.
func (s *BasePromiseParserListener) EnterSliceExpr(ctx *SliceExprContext) {}

// ExitSliceExpr is called when production sliceExpr is exited.
func (s *BasePromiseParserListener) ExitSliceExpr(ctx *SliceExprContext) {}

// EnterElvisExpr is called when production elvisExpr is entered.
func (s *BasePromiseParserListener) EnterElvisExpr(ctx *ElvisExprContext) {}

// ExitElvisExpr is called when production elvisExpr is exited.
func (s *BasePromiseParserListener) ExitElvisExpr(ctx *ElvisExprContext) {}

// EnterShiftExpr is called when production shiftExpr is entered.
func (s *BasePromiseParserListener) EnterShiftExpr(ctx *ShiftExprContext) {}

// ExitShiftExpr is called when production shiftExpr is exited.
func (s *BasePromiseParserListener) ExitShiftExpr(ctx *ShiftExprContext) {}

// EnterBitwiseExpr is called when production bitwiseExpr is entered.
func (s *BasePromiseParserListener) EnterBitwiseExpr(ctx *BitwiseExprContext) {}

// ExitBitwiseExpr is called when production bitwiseExpr is exited.
func (s *BasePromiseParserListener) ExitBitwiseExpr(ctx *BitwiseExprContext) {}

// EnterLogicalOrExpr is called when production logicalOrExpr is entered.
func (s *BasePromiseParserListener) EnterLogicalOrExpr(ctx *LogicalOrExprContext) {}

// ExitLogicalOrExpr is called when production logicalOrExpr is exited.
func (s *BasePromiseParserListener) ExitLogicalOrExpr(ctx *LogicalOrExprContext) {}

// EnterIndexExpr is called when production indexExpr is entered.
func (s *BasePromiseParserListener) EnterIndexExpr(ctx *IndexExprContext) {}

// ExitIndexExpr is called when production indexExpr is exited.
func (s *BasePromiseParserListener) ExitIndexExpr(ctx *IndexExprContext) {}

// EnterOptionalChainExpr is called when production optionalChainExpr is entered.
func (s *BasePromiseParserListener) EnterOptionalChainExpr(ctx *OptionalChainExprContext) {}

// ExitOptionalChainExpr is called when production optionalChainExpr is exited.
func (s *BasePromiseParserListener) ExitOptionalChainExpr(ctx *OptionalChainExprContext) {}

// EnterErrorUnwrapExpr is called when production errorUnwrapExpr is entered.
func (s *BasePromiseParserListener) EnterErrorUnwrapExpr(ctx *ErrorUnwrapExprContext) {}

// ExitErrorUnwrapExpr is called when production errorUnwrapExpr is exited.
func (s *BasePromiseParserListener) ExitErrorUnwrapExpr(ctx *ErrorUnwrapExprContext) {}

// EnterUnaryNotExpr is called when production unaryNotExpr is entered.
func (s *BasePromiseParserListener) EnterUnaryNotExpr(ctx *UnaryNotExprContext) {}

// ExitUnaryNotExpr is called when production unaryNotExpr is exited.
func (s *BasePromiseParserListener) ExitUnaryNotExpr(ctx *UnaryNotExprContext) {}

// EnterMultiplicativeExpr is called when production multiplicativeExpr is entered.
func (s *BasePromiseParserListener) EnterMultiplicativeExpr(ctx *MultiplicativeExprContext) {}

// ExitMultiplicativeExpr is called when production multiplicativeExpr is exited.
func (s *BasePromiseParserListener) ExitMultiplicativeExpr(ctx *MultiplicativeExprContext) {}

// EnterEqualityExpr is called when production equalityExpr is entered.
func (s *BasePromiseParserListener) EnterEqualityExpr(ctx *EqualityExprContext) {}

// ExitEqualityExpr is called when production equalityExpr is exited.
func (s *BasePromiseParserListener) ExitEqualityExpr(ctx *EqualityExprContext) {}

// EnterIntLiteral is called when production intLiteral is entered.
func (s *BasePromiseParserListener) EnterIntLiteral(ctx *IntLiteralContext) {}

// ExitIntLiteral is called when production intLiteral is exited.
func (s *BasePromiseParserListener) ExitIntLiteral(ctx *IntLiteralContext) {}

// EnterFloatLiteral is called when production floatLiteral is entered.
func (s *BasePromiseParserListener) EnterFloatLiteral(ctx *FloatLiteralContext) {}

// ExitFloatLiteral is called when production floatLiteral is exited.
func (s *BasePromiseParserListener) ExitFloatLiteral(ctx *FloatLiteralContext) {}

// EnterTrueLiteral is called when production trueLiteral is entered.
func (s *BasePromiseParserListener) EnterTrueLiteral(ctx *TrueLiteralContext) {}

// ExitTrueLiteral is called when production trueLiteral is exited.
func (s *BasePromiseParserListener) ExitTrueLiteral(ctx *TrueLiteralContext) {}

// EnterFalseLiteral is called when production falseLiteral is entered.
func (s *BasePromiseParserListener) EnterFalseLiteral(ctx *FalseLiteralContext) {}

// ExitFalseLiteral is called when production falseLiteral is exited.
func (s *BasePromiseParserListener) ExitFalseLiteral(ctx *FalseLiteralContext) {}

// EnterNoneLiteral is called when production noneLiteral is entered.
func (s *BasePromiseParserListener) EnterNoneLiteral(ctx *NoneLiteralContext) {}

// ExitNoneLiteral is called when production noneLiteral is exited.
func (s *BasePromiseParserListener) ExitNoneLiteral(ctx *NoneLiteralContext) {}

// EnterCharLiteral is called when production charLiteral is entered.
func (s *BasePromiseParserListener) EnterCharLiteral(ctx *CharLiteralContext) {}

// ExitCharLiteral is called when production charLiteral is exited.
func (s *BasePromiseParserListener) ExitCharLiteral(ctx *CharLiteralContext) {}

// EnterStringLit is called when production stringLit is entered.
func (s *BasePromiseParserListener) EnterStringLit(ctx *StringLitContext) {}

// ExitStringLit is called when production stringLit is exited.
func (s *BasePromiseParserListener) ExitStringLit(ctx *StringLitContext) {}

// EnterIdentExpr is called when production identExpr is entered.
func (s *BasePromiseParserListener) EnterIdentExpr(ctx *IdentExprContext) {}

// ExitIdentExpr is called when production identExpr is exited.
func (s *BasePromiseParserListener) ExitIdentExpr(ctx *IdentExprContext) {}

// EnterThisExpr is called when production thisExpr is entered.
func (s *BasePromiseParserListener) EnterThisExpr(ctx *ThisExprContext) {}

// ExitThisExpr is called when production thisExpr is exited.
func (s *BasePromiseParserListener) ExitThisExpr(ctx *ThisExprContext) {}

// EnterParenExpr is called when production parenExpr is entered.
func (s *BasePromiseParserListener) EnterParenExpr(ctx *ParenExprContext) {}

// ExitParenExpr is called when production parenExpr is exited.
func (s *BasePromiseParserListener) ExitParenExpr(ctx *ParenExprContext) {}

// EnterTupleLiteral is called when production tupleLiteral is entered.
func (s *BasePromiseParserListener) EnterTupleLiteral(ctx *TupleLiteralContext) {}

// ExitTupleLiteral is called when production tupleLiteral is exited.
func (s *BasePromiseParserListener) ExitTupleLiteral(ctx *TupleLiteralContext) {}

// EnterArrayLiteral is called when production arrayLiteral is entered.
func (s *BasePromiseParserListener) EnterArrayLiteral(ctx *ArrayLiteralContext) {}

// ExitArrayLiteral is called when production arrayLiteral is exited.
func (s *BasePromiseParserListener) ExitArrayLiteral(ctx *ArrayLiteralContext) {}

// EnterMapLiteral is called when production mapLiteral is entered.
func (s *BasePromiseParserListener) EnterMapLiteral(ctx *MapLiteralContext) {}

// ExitMapLiteral is called when production mapLiteral is exited.
func (s *BasePromiseParserListener) ExitMapLiteral(ctx *MapLiteralContext) {}

// EnterLambda is called when production lambda is entered.
func (s *BasePromiseParserListener) EnterLambda(ctx *LambdaContext) {}

// ExitLambda is called when production lambda is exited.
func (s *BasePromiseParserListener) ExitLambda(ctx *LambdaContext) {}

// EnterIfExpression is called when production ifExpression is entered.
func (s *BasePromiseParserListener) EnterIfExpression(ctx *IfExpressionContext) {}

// ExitIfExpression is called when production ifExpression is exited.
func (s *BasePromiseParserListener) ExitIfExpression(ctx *IfExpressionContext) {}

// EnterMatchExpression is called when production matchExpression is entered.
func (s *BasePromiseParserListener) EnterMatchExpression(ctx *MatchExpressionContext) {}

// ExitMatchExpression is called when production matchExpression is exited.
func (s *BasePromiseParserListener) ExitMatchExpression(ctx *MatchExpressionContext) {}

// EnterGoExpression is called when production goExpression is entered.
func (s *BasePromiseParserListener) EnterGoExpression(ctx *GoExpressionContext) {}

// ExitGoExpression is called when production goExpression is exited.
func (s *BasePromiseParserListener) ExitGoExpression(ctx *GoExpressionContext) {}

// EnterUnsafeExpression is called when production unsafeExpression is entered.
func (s *BasePromiseParserListener) EnterUnsafeExpression(ctx *UnsafeExpressionContext) {}

// ExitUnsafeExpression is called when production unsafeExpression is exited.
func (s *BasePromiseParserListener) ExitUnsafeExpression(ctx *UnsafeExpressionContext) {}

// EnterMapEntry is called when production mapEntry is entered.
func (s *BasePromiseParserListener) EnterMapEntry(ctx *MapEntryContext) {}

// ExitMapEntry is called when production mapEntry is exited.
func (s *BasePromiseParserListener) ExitMapEntry(ctx *MapEntryContext) {}

// EnterLambdaExpr is called when production lambdaExpr is entered.
func (s *BasePromiseParserListener) EnterLambdaExpr(ctx *LambdaExprContext) {}

// ExitLambdaExpr is called when production lambdaExpr is exited.
func (s *BasePromiseParserListener) ExitLambdaExpr(ctx *LambdaExprContext) {}

// EnterLambdaParams is called when production lambdaParams is entered.
func (s *BasePromiseParserListener) EnterLambdaParams(ctx *LambdaParamsContext) {}

// ExitLambdaParams is called when production lambdaParams is exited.
func (s *BasePromiseParserListener) ExitLambdaParams(ctx *LambdaParamsContext) {}

// EnterTypedLambdaParam is called when production typedLambdaParam is entered.
func (s *BasePromiseParserListener) EnterTypedLambdaParam(ctx *TypedLambdaParamContext) {}

// ExitTypedLambdaParam is called when production typedLambdaParam is exited.
func (s *BasePromiseParserListener) ExitTypedLambdaParam(ctx *TypedLambdaParamContext) {}

// EnterUntypedLambdaParam is called when production untypedLambdaParam is entered.
func (s *BasePromiseParserListener) EnterUntypedLambdaParam(ctx *UntypedLambdaParamContext) {}

// ExitUntypedLambdaParam is called when production untypedLambdaParam is exited.
func (s *BasePromiseParserListener) ExitUntypedLambdaParam(ctx *UntypedLambdaParamContext) {}

// EnterIfExpr is called when production ifExpr is entered.
func (s *BasePromiseParserListener) EnterIfExpr(ctx *IfExprContext) {}

// ExitIfExpr is called when production ifExpr is exited.
func (s *BasePromiseParserListener) ExitIfExpr(ctx *IfExprContext) {}

// EnterMatchExpr is called when production matchExpr is entered.
func (s *BasePromiseParserListener) EnterMatchExpr(ctx *MatchExprContext) {}

// ExitMatchExpr is called when production matchExpr is exited.
func (s *BasePromiseParserListener) ExitMatchExpr(ctx *MatchExprContext) {}

// EnterMatchArm is called when production matchArm is entered.
func (s *BasePromiseParserListener) EnterMatchArm(ctx *MatchArmContext) {}

// ExitMatchArm is called when production matchArm is exited.
func (s *BasePromiseParserListener) ExitMatchArm(ctx *MatchArmContext) {}

// EnterEnumDestructurePattern is called when production enumDestructurePattern is entered.
func (s *BasePromiseParserListener) EnterEnumDestructurePattern(ctx *EnumDestructurePatternContext) {}

// ExitEnumDestructurePattern is called when production enumDestructurePattern is exited.
func (s *BasePromiseParserListener) ExitEnumDestructurePattern(ctx *EnumDestructurePatternContext) {}

// EnterEnumVariantPattern is called when production enumVariantPattern is entered.
func (s *BasePromiseParserListener) EnterEnumVariantPattern(ctx *EnumVariantPatternContext) {}

// ExitEnumVariantPattern is called when production enumVariantPattern is exited.
func (s *BasePromiseParserListener) ExitEnumVariantPattern(ctx *EnumVariantPatternContext) {}

// EnterTypeBindingPattern is called when production typeBindingPattern is entered.
func (s *BasePromiseParserListener) EnterTypeBindingPattern(ctx *TypeBindingPatternContext) {}

// ExitTypeBindingPattern is called when production typeBindingPattern is exited.
func (s *BasePromiseParserListener) ExitTypeBindingPattern(ctx *TypeBindingPatternContext) {}

// EnterShortDestructurePattern is called when production shortDestructurePattern is entered.
func (s *BasePromiseParserListener) EnterShortDestructurePattern(ctx *ShortDestructurePatternContext) {
}

// ExitShortDestructurePattern is called when production shortDestructurePattern is exited.
func (s *BasePromiseParserListener) ExitShortDestructurePattern(ctx *ShortDestructurePatternContext) {
}

// EnterNamePattern is called when production namePattern is entered.
func (s *BasePromiseParserListener) EnterNamePattern(ctx *NamePatternContext) {}

// ExitNamePattern is called when production namePattern is exited.
func (s *BasePromiseParserListener) ExitNamePattern(ctx *NamePatternContext) {}

// EnterIntLiteralPattern is called when production intLiteralPattern is entered.
func (s *BasePromiseParserListener) EnterIntLiteralPattern(ctx *IntLiteralPatternContext) {}

// ExitIntLiteralPattern is called when production intLiteralPattern is exited.
func (s *BasePromiseParserListener) ExitIntLiteralPattern(ctx *IntLiteralPatternContext) {}

// EnterFloatLiteralPattern is called when production floatLiteralPattern is entered.
func (s *BasePromiseParserListener) EnterFloatLiteralPattern(ctx *FloatLiteralPatternContext) {}

// ExitFloatLiteralPattern is called when production floatLiteralPattern is exited.
func (s *BasePromiseParserListener) ExitFloatLiteralPattern(ctx *FloatLiteralPatternContext) {}

// EnterTrueLiteralPattern is called when production trueLiteralPattern is entered.
func (s *BasePromiseParserListener) EnterTrueLiteralPattern(ctx *TrueLiteralPatternContext) {}

// ExitTrueLiteralPattern is called when production trueLiteralPattern is exited.
func (s *BasePromiseParserListener) ExitTrueLiteralPattern(ctx *TrueLiteralPatternContext) {}

// EnterFalseLiteralPattern is called when production falseLiteralPattern is entered.
func (s *BasePromiseParserListener) EnterFalseLiteralPattern(ctx *FalseLiteralPatternContext) {}

// ExitFalseLiteralPattern is called when production falseLiteralPattern is exited.
func (s *BasePromiseParserListener) ExitFalseLiteralPattern(ctx *FalseLiteralPatternContext) {}

// EnterNoneLiteralPattern is called when production noneLiteralPattern is entered.
func (s *BasePromiseParserListener) EnterNoneLiteralPattern(ctx *NoneLiteralPatternContext) {}

// ExitNoneLiteralPattern is called when production noneLiteralPattern is exited.
func (s *BasePromiseParserListener) ExitNoneLiteralPattern(ctx *NoneLiteralPatternContext) {}

// EnterStringLiteralPattern is called when production stringLiteralPattern is entered.
func (s *BasePromiseParserListener) EnterStringLiteralPattern(ctx *StringLiteralPatternContext) {}

// ExitStringLiteralPattern is called when production stringLiteralPattern is exited.
func (s *BasePromiseParserListener) ExitStringLiteralPattern(ctx *StringLiteralPatternContext) {}

// EnterWildcardPattern is called when production wildcardPattern is entered.
func (s *BasePromiseParserListener) EnterWildcardPattern(ctx *WildcardPatternContext) {}

// ExitWildcardPattern is called when production wildcardPattern is exited.
func (s *BasePromiseParserListener) ExitWildcardPattern(ctx *WildcardPatternContext) {}

// EnterDestructureIsPattern is called when production destructureIsPattern is entered.
func (s *BasePromiseParserListener) EnterDestructureIsPattern(ctx *DestructureIsPatternContext) {}

// ExitDestructureIsPattern is called when production destructureIsPattern is exited.
func (s *BasePromiseParserListener) ExitDestructureIsPattern(ctx *DestructureIsPatternContext) {}

// EnterIdentIsPattern is called when production identIsPattern is entered.
func (s *BasePromiseParserListener) EnterIdentIsPattern(ctx *IdentIsPatternContext) {}

// ExitIdentIsPattern is called when production identIsPattern is exited.
func (s *BasePromiseParserListener) ExitIdentIsPattern(ctx *IdentIsPatternContext) {}

// EnterPatternFields is called when production patternFields is entered.
func (s *BasePromiseParserListener) EnterPatternFields(ctx *PatternFieldsContext) {}

// ExitPatternFields is called when production patternFields is exited.
func (s *BasePromiseParserListener) ExitPatternFields(ctx *PatternFieldsContext) {}

// EnterGoExpr is called when production goExpr is entered.
func (s *BasePromiseParserListener) EnterGoExpr(ctx *GoExprContext) {}

// ExitGoExpr is called when production goExpr is exited.
func (s *BasePromiseParserListener) ExitGoExpr(ctx *GoExprContext) {}

// EnterUnsafeBlock is called when production unsafeBlock is entered.
func (s *BasePromiseParserListener) EnterUnsafeBlock(ctx *UnsafeBlockContext) {}

// ExitUnsafeBlock is called when production unsafeBlock is exited.
func (s *BasePromiseParserListener) ExitUnsafeBlock(ctx *UnsafeBlockContext) {}

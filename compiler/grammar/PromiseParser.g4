parser grammar PromiseParser;

options { tokenVocab = PromiseLexer; }

// ============================================================
// Top Level
// ============================================================

compilationUnit
    : useDecl* declaration* EOF
    ;

useDecl
    : USE IDENT (AS bindingName)? SEMI              # catalogImport
    | USE bindingName stringLiteral SEMI             # sourcedImport
    ;

declaration
    : typeDecl
    | enumDecl
    | funcDecl
    | getterDecl
    | setterDecl
    ;

// ============================================================
// Binding Names (identifiers or _ discard)
// ============================================================

bindingName
    : IDENT
    | UNDERSCORE
    ;

// ============================================================
// String Literal (handles interpolation segments)
// ============================================================

stringLiteral
    : STRING_OPEN stringPart* STRING_CLOSE
    | RAW_STRING
    | TRIPLE_STRING
    ;

stringPart
    : STRING_TEXT
    | STRING_ESCAPE
    | STRING_INTERP
    ;

// ============================================================
// Type Declarations
// ============================================================

typeDecl
    : TYPE IDENT typeParams? inheritance? metaAnnotation* LBRACE typeMember* RBRACE
    ;

inheritance
    : IS typeRef (COMMA typeRef)*
    ;

typeParams
    : LBRACKET typeParam (COMMA typeParam)* RBRACKET
    ;

typeParam
    : IDENT (COLON typeConstraint)?
    ;

typeConstraint
    : typeRef (PLUS typeRef)*
    ;

typeMember
    : fieldDecl
    | methodDecl
    | getterDecl
    | setterDecl
    ;

fieldDecl
    : typeRef IDENT metaAnnotation* (ASSIGN expression)? SEMI
    ;

methodDecl
    : methodName typeParams? LPAREN params RPAREN returnType? metaAnnotation* (memberBody | SEMI)
    ;

// Getter: `get <name> <type> <annotations>? (<body> | ;)`
// `get` is contextual — lexed as IDENT, validated in AST builder.
getterDecl
    : IDENT IDENT typeRef BANG? metaAnnotation* (memberBody | SEMI)
    ;

// Setter: `set <name>(<type> <param>) <annotations>? (<body> | ;)`
// `set` is contextual — lexed as IDENT, validated in AST builder.
setterDecl
    : IDENT IDENT LPAREN typeRef IDENT RPAREN metaAnnotation* (memberBody | SEMI)
    ;

// Shared body for methods, getters, setters: block or expression body.
memberBody
    : block
    | FAT_ARROW expression SEMI
    ;

// Method name: identifier or operator symbol (for operator overloading)
methodName
    : IDENT
    | PLUS | MINUS | STAR | SLASH | PERCENT
    | EQ | NEQ | LT | GT | LTE | GTE
    | AND | OR | BANG
    | AMP | PIPE | CARET | LSHIFT | RSHIFT | TILDE
    | PLUSPLUS | MINUSMINUS
    | DOTDOT | DOTDOTEQ
    | LBRACKET COLON RBRACKET ASSIGN
    | LBRACKET COLON RBRACKET
    | LBRACKET RBRACKET ASSIGN
    | LBRACKET RBRACKET
    ;

// ============================================================
// Enum Declarations
// ============================================================

enumDecl
    : ENUM IDENT typeParams? metaAnnotation* LBRACE enumVariant (COMMA enumVariant)* COMMA? enumMember* RBRACE
    ;

enumVariant
    : IDENT (LPAREN enumField (COMMA enumField)* RPAREN)? metaAnnotation*
    ;

enumField
    : typeRef IDENT
    ;

enumMember
    : methodDecl
    | getterDecl
    ;

// ============================================================
// Function Declarations
// ============================================================

funcDecl
    : IDENT typeParams? LPAREN params RPAREN returnType? metaAnnotation* (memberBody | SEMI)
    ;

returnType
    : typeRef BANG?
    | BANG
    ;

// ============================================================
// Parameters
// ============================================================

params
    : paramList?
    ;

paramList
    : receiverParam (COMMA param)*
    | param (COMMA param)*
    ;

receiverParam
    : refMod? THIS
    ;

param
    : ELLIPSIS typeRef bindingName metaAnnotation*               # variadicParam
    | typeRef refMod? bindingName metaAnnotation* (ASSIGN expression)?  # regularParam
    ;

refMod
    : AMP
    | TILDE
    ;

// ============================================================
// Arguments (call site)
// ============================================================

args
    : argList?
    ;

argList
    : arg (COMMA arg)*
    ;

arg
    : (IDENT COLON)? expression
    ;

// ============================================================
// Meta Annotations
// ============================================================

metaAnnotation
    : BACKTICK IDENT (LPAREN metaParams RPAREN)?
    ;

metaParams
    : metaParam (COMMA metaParam)*
    ;

metaParam
    : IDENT COLON expression                                   // named
    | expression                                               // positional
    ;

// ============================================================
// Type References
// ============================================================

typeRef
    : IDENT DOT IDENT typeArgs?                                # qualifiedType
    | IDENT typeArgs?                                          # namedType
    | LPAREN typeRef (COMMA typeRef)+ RPAREN                   # tupleType
    | LPAREN typeRefList? RPAREN ARROW funcTypeReturn          # functionType
    | LPAREN typeRef RPAREN                                    # parenType
    | typeRef AMP                                              # sharedRefType
    | typeRef TILDE                                            # mutRefType
    | typeRef STAR                                             # pointerType
    | typeRef QUESTION                                         # optionalType
    | typeRef LBRACKET RBRACKET                                # sliceType
    | typeRef LBRACKET INT_LITERAL RBRACKET                    # arrayType
    ;

// Helper rule for function type return position.
// Using a separate rule avoids ANTLR4's precedence-climbing from
// blocking suffix operators (?, [], &, ~, *) on the return type.
funcTypeReturn
    : typeRef
    ;

typeArgs
    : LBRACKET typeRef (COMMA typeRef)* RBRACKET
    ;

typeRefList
    : typeRef (COMMA typeRef)*
    ;

// ============================================================
// Blocks and Statements
// ============================================================

block
    : LBRACE statement* RBRACE
    ;

statement
    : useVarDecl
    | varDecl
    | returnStmt
    | raiseStmt
    | yieldStmt
    | yieldDelegateStmt
    | breakStmt
    | continueStmt
    | ifStmt
    | forStmt
    | whileStmt
    | selectStmt                                               // block-terminated, no ;
    | matchExpr                                                // block-terminated, no ;
    | unsafeBlock                                              // block-terminated, no ;
    | incDecStmt
    | assignmentStmt
    | expressionStmt
    ;

useVarDecl
    : USE IDENT WALRUS expression SEMI
    ;

varDecl
    : typeRef refMod? bindingName (ASSIGN expression)? SEMI    # typedVarDecl
    | bindingName WALRUS expression SEMI                       # inferredVarDecl
    | LPAREN bindingName COMMA bindingName RPAREN WALRUS expression SEMI   # destructureVarDecl
    ;

assignmentStmt
    : expression assignOp expression SEMI
    ;

assignOp
    : ASSIGN
    | PLUS_ASSIGN
    | MINUS_ASSIGN
    | STAR_ASSIGN
    | SLASH_ASSIGN
    | PERCENT_ASSIGN
    ;

incDecStmt
    : expression (PLUSPLUS | MINUSMINUS) SEMI
    ;

returnStmt
    : RETURN expression? SEMI
    ;

raiseStmt
    : RAISE expression SEMI
    ;

breakStmt
    : BREAK SEMI
    ;

continueStmt
    : CONTINUE SEMI
    ;

yieldStmt
    : YIELD expression SEMI
    ;

yieldDelegateStmt
    : YIELD STAR expression SEMI
    ;

expressionStmt
    : expression SEMI
    ;

// ============================================================
// Control Flow
// ============================================================

ifStmt
    : IF ifCondition block elseClause?
    ;

ifCondition
    : bindingName WALRUS expression                            # ifUnwrapCond
    | expression                                               # ifExprCond
    ;

elseClause
    : ELSE (ifStmt | block)
    ;

forStmt
    : FOR bindingName (COMMA bindingName)? IN expression block # forInStmt
    | FOR forInit expression SEMI forUpdate block              # classicForStmt
    | FOR block                                                # infiniteLoopStmt
    ;

// Classic for-loop initializer (semicolon-terminated)
forInit
    : typeRef IDENT ASSIGN expression SEMI                     # forInitTyped
    | IDENT WALRUS expression SEMI                             # forInitInferred
    ;

// Classic for-loop update (allows assignment, inc/dec, or bare expression)
forUpdate
    : expression assignOp expression                           # forUpdateAssign
    | expression (PLUSPLUS | MINUSMINUS)                        # forUpdateIncDec
    | expression                                               # forUpdateExpr
    ;

whileStmt
    : WHILE bindingName WALRUS expression block                # whileUnwrapStmt
    | WHILE expression block                                   # whileExprStmt
    ;

// ============================================================
// Select Statement
// ============================================================

selectStmt
    : SELECT LBRACE selectCase* selectDefault? RBRACE
    ;

selectCase
    : expression DOT IDENT LPAREN expression RPAREN COLON statement*   // ch.send(v):
    | bindingName WALRUS LT MINUS expression COLON statement*          // val := <-ch:
    ;

selectDefault
    : IDENT COLON statement*  // 'default' is a contextual keyword — validated in AST builder
    ;

// ============================================================
// Expressions
// ============================================================
// ANTLR4 precedence climbing: first alternative = highest precedence.
//
// Precedence table (1 = highest, 14 = lowest):
//   1 (highest): . ?. () [] ? ! (? handler) (? is T handler)
//   2: Unary - ! ~ <-
//   3: * / %
//   4: << >>
//   5: + -
//   6: & ^ |
//   7: .. ..=
//   8: < > <= >= is as
//   9: == !=
//  10: &&
//  11: ||
//  12 (low): ?:
//  Assignment is NOT an expression — handled as assignmentStmt.

expression
    // ANTLR4 gives higher precedence to alternatives listed first.
    // Listed from highest precedence to lowest.

    // Precedence 1 (highest): Postfix — member access, calls, indexing, error ops
    : expression DOT IDENT                                     # memberAccessExpr
    | expression QUESTION_DOT IDENT                            # optionalChainExpr
    | expression LPAREN args RPAREN                            # callExpr
    | expression LBRACKET expression (COMMA expression)* RBRACKET  # indexExpr
    | expression LBRACKET expression? COLON expression? RBRACKET  # sliceExpr
    | expression LBRACKET RBRACKET                               # sliceTypeExpr
    | expression QUESTION bindingName? (IS IDENT typeArgs?)? (block (ELSE bindingName? block | BANG)? | FAT_ARROW expression)  # errorHandlerExpr
    | expression QUESTION                                      # errorPropagateExpr
    | expression BANG                                          # errorUnwrapExpr

    // Precedence 2: Unary prefix
    | MINUS expression                                         # unaryNegExpr
    | BANG expression                                          # unaryNotExpr
    | TILDE expression                                         # bitwiseNotExpr
    | LT MINUS expression                                      # receiveExpr

    // Precedence 3: Multiplicative
    | expression (STAR | SLASH | PERCENT) expression           # multiplicativeExpr

    // Precedence 4: Shift
    | expression (LSHIFT | RSHIFT) expression                  # shiftExpr

    // Precedence 5: Additive
    | expression (PLUS | MINUS) expression                     # additiveExpr

    // Precedence 6: Bitwise
    | expression (AMP | CARET | PIPE) expression               # bitwiseExpr

    // Precedence 7: Range
    | expression DOTDOT expression                             # exclusiveRangeExpr
    | expression DOTDOTEQ expression                           # inclusiveRangeExpr

    // Precedence 8: Comparison + type check/cast
    | expression (LT | GT | LTE | GTE) expression             # comparisonExpr
    | expression IS pattern                                    # isExpr
    | expression AS BANG? typeRef                               # castExpr

    // Precedence 9: Equality
    | expression (EQ | NEQ) expression                         # equalityExpr

    // Precedence 10: Logical AND
    | expression AND expression                                # logicalAndExpr

    // Precedence 11: Logical OR
    | expression OR expression                                 # logicalOrExpr

    // Precedence 12 (lowest): Elvis
    | expression QUESTION_COLON expression                     # elvisExpr

    // Primary atoms (not left-recursive — always highest precedence)
    | primary                                                  # primaryExpr
    ;

primary
    : INT_LITERAL                                              # intLiteral
    | FLOAT_LITERAL                                            # floatLiteral
    | TRUE                                                     # trueLiteral
    | FALSE                                                    # falseLiteral
    | NONE                                                     # noneLiteral
    | CHAR_LITERAL                                             # charLiteral
    | stringLiteral                                            # stringLit
    | IDENT                                                    # identExpr
    | THIS                                                     # thisExpr
    | LPAREN expression RPAREN                                 # parenExpr
    | LPAREN expression COMMA expression (COMMA expression)* RPAREN  # tupleLiteral
    | LBRACKET (expression (COMMA expression)* COMMA?)? RBRACKET     # arrayLiteral
    | LBRACE (COLON | mapEntry (COMMA mapEntry)* COMMA?) RBRACE  # mapLiteral
    | lambdaExpr                                               # lambda
    | ifExpr                                                   # ifExpression
    | matchExpr                                                # matchExpression
    | goExpr                                                   # goExpression
    | unsafeBlock                                              # unsafeExpression
    ;

// ============================================================
// Map Literal
// ============================================================

mapEntry
    : expression COLON expression
    ;

// ============================================================
// Lambda Expressions (pipe-delimited parameters)
// ============================================================

lambdaExpr
    : MOVE? PIPE lambdaParams? PIPE (ARROW typeRef)? block     // with params, block body
    | MOVE? PIPE lambdaParams? PIPE ARROW expression           // with params, expression body
    | MOVE? OR (ARROW typeRef)? block                          // no params (||), block body
    | MOVE? OR ARROW expression                                // no params (||), expression body
    ;

lambdaParams
    : lambdaParam (COMMA lambdaParam)*
    ;

lambdaParam
    : typeRef refMod? bindingName                              # typedLambdaParam
    | bindingName                                              # untypedLambdaParam
    ;

// ============================================================
// If Expression (must have else — produces a value)
// ============================================================

ifExpr
    : IF expression block ELSE block
    ;

// ============================================================
// Match Expression
// ============================================================

matchExpr
    : MATCH expression LBRACE matchArm (COMMA matchArm)* COMMA? RBRACE
    ;

matchArm
    : matchPattern (IF expression)? FAT_ARROW (expression | block)
    ;

matchPattern
    : IDENT DOT IDENT LPAREN patternFields RPAREN             # enumDestructurePattern
    | IDENT DOT IDENT                                          # enumVariantPattern
    | IDENT bindingName                                        # typeBindingPattern
    | IDENT LPAREN patternFields RPAREN                        # shortDestructurePattern
    | IDENT                                                    # namePattern
    | INT_LITERAL                                              # intLiteralPattern
    | FLOAT_LITERAL                                            # floatLiteralPattern
    | CHAR_LITERAL                                             # charLiteralPattern
    | TRUE                                                     # trueLiteralPattern
    | FALSE                                                    # falseLiteralPattern
    | NONE                                                     # noneLiteralPattern
    | stringLiteral                                            # stringLiteralPattern
    | UNDERSCORE                                               # wildcardPattern
    | expression                                               # expressionPattern
    ;

// Pattern for 'is' expressions
pattern
    : IDENT typeArgs? LPAREN patternFields RPAREN              # destructureIsPattern
    | IDENT typeArgs?                                          # identIsPattern
    ;

patternFields
    : bindingName (COMMA bindingName)*
    ;

// ============================================================
// Go Expression
// ============================================================

goExpr
    : GO (block | expression)
    ;

// ============================================================
// Unsafe Block
// ============================================================

unsafeBlock
    : UNSAFE block
    ;

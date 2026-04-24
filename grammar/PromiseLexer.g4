lexer grammar PromiseLexer;

// ============================================================
// Keywords
// ============================================================
TYPE        : 'type';
ENUM        : 'enum';
IS          : 'is';
AS          : 'as';
IF          : 'if';
ELSE        : 'else';
FOR         : 'for';
WHILE       : 'while';
IN          : 'in';
MATCH       : 'match';
GO          : 'go';
USE         : 'use';
RETURN      : 'return';
RAISE       : 'raise';
YIELD       : 'yield';
BREAK       : 'break';
CONTINUE    : 'continue';
UNSAFE      : 'unsafe';
MOVE        : 'move';

// Literal keywords
NONE        : 'none';
TRUE        : 'true';
FALSE       : 'false';
THIS        : 'this';

// Note: 'present' and 'absent' are contextual keywords — lexed as IDENT,
// recognized by the parser only after 'is' in pattern position.

// ============================================================
// Multi-character operators (must precede single-char counterparts)
// ============================================================

// Range
DOTDOTEQ    : '..=';
DOTDOT      : '..';

// Optional chaining / elvis
QUESTION_DOT   : '?.';
QUESTION_COLON : '?:';

// Arrows
ARROW       : '->';
FAT_ARROW   : '=>';

// Comparison
EQ          : '==';
NEQ         : '!=';
LTE         : '<=';
GTE         : '>=';

// Logical
AND         : '&&';
OR          : '||';
PIPE        : '|';

// Compound assignment
PLUS_ASSIGN : '+=';
MINUS_ASSIGN: '-=';
STAR_ASSIGN : '*=';
SLASH_ASSIGN: '/=';
PERCENT_ASSIGN: '%=';

// Walrus (unwrap binding / type inference)
WALRUS      : ':=';

// ============================================================
// Single-character operators and punctuation
// ============================================================
LBRACE      : '{';
RBRACE      : '}';
LPAREN      : '(';
RPAREN      : ')';
LBRACKET    : '[';
RBRACKET    : ']';

SEMI        : ';';
COMMA       : ',';
DOT         : '.';
COLON       : ':';
BACKTICK    : '`';

ASSIGN      : '=';
LT          : '<';
GT          : '>';

PLUS        : '+';
MINUS       : '-';
STAR        : '*';
SLASH       : '/';
PERCENT     : '%';

BANG        : '!';
AMP         : '&';
TILDE       : '~';
QUESTION    : '?';
UNDERSCORE  : '_';

// ============================================================
// Numeric literals
// ============================================================

FLOAT_LITERAL
    : ('0' | [1-9] ([0-9_]* [0-9])?) '.' [0-9] ([0-9_]* [0-9])? ([eE] [+-]? [0-9]+)?
    | ('0' | [1-9] ([0-9_]* [0-9])?) [eE] [+-]? [0-9]+
    ;

INT_LITERAL
    : '0' [xX] [0-9a-fA-F] ([0-9a-fA-F_]* [0-9a-fA-F])?
    | '0' [oO] [0-7] ([0-7_]* [0-7])?
    | '0' [bB] [01] ([01_]* [01])?
    | '0'
    | [1-9] ([0-9_]* [0-9])?
    ;

// ============================================================
// String literals
// ============================================================

// Triple-quoted string (multiline, no interpolation).
// Must precede STRING_OPEN for longest-match.
TRIPLE_STRING : '"""' .*? '"""';

// Raw string (no escape processing).
RAW_STRING    : 'r"' ~["]* '"';

// Regular string with interpolation — enters STRING_MODE.
STRING_OPEN : '"' -> pushMode(STRING_MODE);

// Char literal — same escape set as strings
CHAR_LITERAL: '\'' (~['\\] | '\\' [nrtb\\'0]) '\'';

// ============================================================
// Identifiers
// ============================================================
IDENT       : [a-zA-Z_] [a-zA-Z0-9_]*;

// ============================================================
// Comments
// ============================================================
LINE_COMMENT  : '//' ~[\r\n]* -> skip;
BLOCK_COMMENT : '/*' .*? '*/' -> skip;

// ============================================================
// Whitespace
// ============================================================
WS : [ \t\r\n]+ -> skip;

// ============================================================
// String mode — lexes string content and interpolation markers
// ============================================================
mode STRING_MODE;

STRING_TEXT        : ~["\\\r\n{]+;
STRING_ESCAPE      : '\\' [nrtb\\"0{];
// Interpolation: captures {expr} as a single token including braces.
// Handles one level of nested braces for expressions like {map[key]}.
// Full nested-brace interpolation deferred to a later stage.
STRING_INTERP      : '{' ( ~[{}]+ | '{' ~[}]* '}' )* '}';
STRING_CLOSE       : '"' -> popMode;

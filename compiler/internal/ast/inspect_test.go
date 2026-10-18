package ast

import (
	"strings"
	"testing"
)

// inspectSrc places a uniquely named identifier or type name in each nested
// position Inspect must reach. A position the walk misses would make a name in
// that position invisible to every pass built on Inspect.
const inspectSrc = `
type Child is InheritedType {
  int counter = field_default_ident;

  run(this) int {
    return method_body_ident;
  }
}

enum Shape {
  Circle(VariantFieldType r),
  Point,
}

generic[T: ConstraintType](T a, int b = param_default_ident) int {
  s := "x {interp_ident} y";
  f := |int p| -> lambda_body_ident + p;
  m := match subject_ident {
    Shape.Circle(r) if guard_ident => arm_body_ident,
    _ => 0,
  };
  go { go_block_ident(); };
  v := failing() ? e is HandlerType { handler_body_ident; };
  return 0;
}
`

// inspectNames collects every identifier and named type reference Inspect
// passes to its callback.
func inspectNames(file *File) map[string]bool {
	seen := make(map[string]bool)
	Inspect(file, func(n Node) bool {
		switch n := n.(type) {
		case *IdentExpr:
			seen[n.Name] = true
		case *NamedTypeRef:
			seen[n.Name] = true
		}
		return true
	})
	return seen
}

func TestInspectReachesNestedPositions(t *testing.T) {
	seen := inspectNames(parseAndBuild(t, inspectSrc))
	for _, tt := range []struct {
		position string
		name     string
	}{
		{"inherits list", "InheritedType"},
		{"field default", "field_default_ident"},
		{"method body inside a type", "method_body_ident"},
		{"enum variant field type", "VariantFieldType"},
		{"type-parameter constraint", "ConstraintType"},
		{"parameter default", "param_default_ident"},
		{"string interpolation", "interp_ident"},
		{"lambda body", "lambda_body_ident"},
		{"match subject", "subject_ident"},
		{"match-arm guard", "guard_ident"},
		{"match-arm body", "arm_body_ident"},
		{"go block", "go_block_ident"},
		{"error handler body", "handler_body_ident"},
	} {
		t.Run(tt.position, func(t *testing.T) {
			if !seen[tt.name] {
				t.Errorf("Inspect did not reach %q in the %s", tt.name, tt.position)
			}
		})
	}
}

// Returning false skips the node's children; its siblings are still visited.
func TestInspectPrunesWhenCallbackReturnsFalse(t *testing.T) {
	file := parseAndBuild(t, inspectSrc)
	seen := make(map[string]bool)
	Inspect(file, func(n Node) bool {
		switch n := n.(type) {
		case *FuncDecl:
			return false
		case *IdentExpr:
			seen[n.Name] = true
		}
		return true
	})
	for name := range seen {
		if strings.HasSuffix(name, "_ident") && name != "field_default_ident" && name != "method_body_ident" {
			t.Errorf("reached %q inside a pruned function", name)
		}
	}
	if !seen["method_body_ident"] {
		t.Error("pruning the function also skipped the type declared beside it")
	}
}

// Nil roots — an untyped nil and a typed nil pointer — are not nodes.
func TestInspectNilNeverCallsCallback(t *testing.T) {
	for _, tt := range []struct {
		name string
		root Node
	}{
		{"untyped nil", nil},
		{"typed nil pointer", (*FuncDecl)(nil)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			Inspect(tt.root, func(Node) bool {
				t.Fatal("callback called for a nil root")
				return true
			})
		})
	}
}

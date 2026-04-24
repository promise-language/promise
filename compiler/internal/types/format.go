package types

import "fmt"

// TypeString returns a human-readable string for a type.
func TypeString(typ Type) string {
	if typ == nil {
		return "<nil>"
	}
	return typ.String()
}

// ObjectString returns a human-readable string for an object.
func ObjectString(obj Object) string {
	if obj == nil {
		return "<nil>"
	}
	switch o := obj.(type) {
	case *Var:
		if o.typ != nil {
			return fmt.Sprintf("var %s %s", o.name, o.typ.String())
		}
		return fmt.Sprintf("var %s", o.name)
	case *Func:
		return fmt.Sprintf("func %s%s", o.name, o.typ.String())
	case *TypeName:
		return fmt.Sprintf("type %s", o.name)
	case *Label:
		return fmt.Sprintf("label %s", o.name)
	default:
		return fmt.Sprintf("%s", o.Name())
	}
}

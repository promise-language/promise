package ast

import "reflect"

// Inspect walks the tree rooted at n in pre-order, calling f for every node it
// reaches. When f returns false, the node's children are skipped. A nil n is
// never passed to f.
//
// Children are derived from the node definitions themselves — every exported
// pointer, interface, slice, and struct field is followed — so the walk cannot
// drift from the AST as node kinds or fields are added, which a hand-written
// type switch would. Values that are not nodes (a StringInterp inside a
// StringLit's Parts) are descended without being passed to f, so the
// expression they hold is still reached. The AST is a tree with no parent
// pointers, so the walk terminates.
func Inspect(n Node, f func(Node) bool) {
	inspectValue(reflect.ValueOf(n), reflect.TypeFor[Node](), f)
}

func inspectValue(v reflect.Value, nodeType reflect.Type, f func(Node) bool) {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.Kind() == reflect.Pointer && v.Type().Implements(nodeType) {
			if !f(v.Interface().(Node)) {
				return
			}
		}
		inspectValue(v.Elem(), nodeType, f)
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			inspectValue(v.Index(i), nodeType, f)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).IsExported() {
				inspectValue(v.Field(i), nodeType, f)
			}
		}
	}
}

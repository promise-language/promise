package bindgen

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/promise-language/promise/compiler/internal/webidl"
)

// KeepList is the selection `promise bind webidl -keep` generates: the
// interfaces it names and the members it names on them (docs/wasm-bindings.md,
// "Generating a subset"). It selects from the input rather than declaring it, so
// an entry that names nothing in the input selects nothing — a list derived from
// compat data names far more than the bound specs define.
type KeepList struct {
	interfaces map[string]bool // "Element"
	members    map[string]bool // "Element.setAttribute", "Event.constructor"
}

// keepConstructor is the member an entry names to keep an interface's
// constructors: `Event.constructor`.
const keepConstructor = "constructor"

// ParseKeepList parses a keep list: one entry per line, `Interface` or
// `Interface.member`, with surrounding whitespace trimmed and blank lines
// skipped. path names the list in errors, and every malformed line is reported,
// not only the first.
func ParseKeepList(path, src string) (*KeepList, error) {
	keep := &KeepList{interfaces: make(map[string]bool), members: make(map[string]bool)}
	var errs []error
	for i, line := range strings.Split(src, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ".")
		if strings.IndexFunc(entry, unicode.IsSpace) >= 0 || len(parts) > 2 || slices.Contains(parts, "") {
			errs = append(errs, fmt.Errorf("%s:%d: malformed keep entry %q: want Interface or Interface.member", path, i+1, entry))
			continue
		}
		if len(parts) == 1 {
			keep.interfaces[entry] = true
		} else {
			keep.members[entry] = true
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return keep, nil
}

// KeepDrop is a declaration ApplyKeepList removed because it named a definition
// the list dropped: What names the declaration, Ref the definition it named.
type KeepDrop struct {
	What string
	Ref  string
}

// ApplyKeepList reduces a merged file (webidl.Merge has run, so a mixin's
// members sit on each interface that includes it) to what keep selects, and
// returns what it then had to drop so that nothing kept names a dropped
// definition.
//
// An interface is kept when it is listed, or when a kept interface inherits from
// it: an ancestor stays, with only its listed members, so the `is` clause
// resolves. A member of a kept interface is kept when `Interface.member` is
// listed (`Interface.constructor` for its constructors); constants, iterable
// declarations and anonymous special operations go with their interface.
// Dictionaries, enums, typedefs and callbacks are not listed.
//
// The closure, to a fixed point: a typedef whose type names a dropped definition
// is dropped, and so is a dictionary whose parent is dropped or whose required
// member names one. Then an optional dictionary member naming one is removed,
// and so is an interface member — together with every member sharing its name,
// so the overload ordinals WebIdlToIR assigns never depend on the list.
//
// A reference is a name the lowered type carries (convertWebIdlTypeRef), so a
// name inside a union, Promise<T> or record<K, V> — each lowered to JsValue — is
// not one. A name absent from the input is not a dropped definition: a
// declaration naming it is left to fail to compile, as an unbound parent does.
func ApplyKeepList(file *webidl.File, keep *KeepList) []KeepDrop {
	ifaces := make(map[string]*webidl.Interface)
	for _, iface := range file.Interfaces {
		if !isMixin(iface) {
			ifaces[iface.Name] = iface
		}
	}

	kept := make(map[string]bool)
	for _, iface := range file.Interfaces {
		if isMixin(iface) || !keep.interfaces[iface.Name] {
			continue
		}
		for a := iface; a != nil && !kept[a.Name]; a = ifaces[a.Parent] {
			kept[a.Name] = true
		}
	}

	dropped := make(map[string]bool)
	for name := range ifaces {
		if !kept[name] {
			dropped[name] = true
		}
	}
	// droppedRef returns the first dropped definition the types name, or "".
	droppedRef := func(types ...*webidl.TypeRef) string {
		var hit string
		for _, t := range types {
			if t == nil {
				continue
			}
			if eachName(convertWebIdlTypeRef(t), func(name string) bool {
				hit = name
				return dropped[name]
			}) {
				return hit
			}
		}
		return ""
	}

	var drops []KeepDrop
	for changed := true; changed; {
		changed = false
		for _, td := range file.Typedefs {
			if dropped[td.Name] {
				continue
			}
			if ref := droppedRef(td.Type); ref != "" {
				dropped[td.Name] = true
				drops = append(drops, KeepDrop{What: "typedef " + td.Name, Ref: ref})
				changed = true
			}
		}
		for _, d := range file.Dictionaries {
			if dropped[d.Name] {
				continue
			}
			ref := ""
			if d.Parent != "" && dropped[d.Parent] {
				ref = d.Parent
			}
			for _, m := range d.Members {
				if ref == "" && m.Required {
					ref = droppedRef(m.Type)
				}
			}
			if ref != "" {
				dropped[d.Name] = true
				drops = append(drops, KeepDrop{What: "dictionary " + d.Name, Ref: ref})
				changed = true
			}
		}
	}

	var typedefs []*webidl.Typedef
	for _, td := range file.Typedefs {
		if !dropped[td.Name] {
			typedefs = append(typedefs, td)
		}
	}
	file.Typedefs = typedefs

	var dicts []*webidl.Dictionary
	for _, d := range file.Dictionaries {
		if dropped[d.Name] {
			continue
		}
		var members []*webidl.DictMember
		for _, m := range d.Members {
			if ref := droppedRef(m.Type); ref != "" {
				drops = append(drops, KeepDrop{What: d.Name + "." + m.Name, Ref: ref})
				continue
			}
			members = append(members, m)
		}
		d.Members = members
		dicts = append(dicts, d)
	}
	file.Dictionaries = dicts

	var interfaces []*webidl.Interface
	for _, iface := range file.Interfaces {
		if isMixin(iface) {
			interfaces = append(interfaces, iface)
			continue
		}
		if !kept[iface.Name] {
			continue
		}
		drops = append(drops, keepMembers(iface, keep, droppedRef)...)
		interfaces = append(interfaces, iface)
	}
	file.Interfaces = interfaces

	return drops
}

// keepMembers reduces a kept interface to its selected members, then drops each
// member name whose members name a dropped definition, reporting one KeepDrop
// per name.
func keepMembers(iface *webidl.Interface, keep *KeepList, droppedRef func(...*webidl.TypeRef) string) []KeepDrop {
	type selected struct {
		member webidl.Member
		unit   string
	}
	var members []selected
	dangling := make(map[string]string) // unit → the dropped definition it names
	var order []string                  // dangling units, in member order
	for _, m := range iface.Members {
		unit, listed, types := keepUnit(m)
		if listed && !keep.members[iface.Name+"."+unit] {
			continue
		}
		members = append(members, selected{member: m, unit: unit})
		if _, seen := dangling[unit]; seen {
			continue
		}
		if ref := droppedRef(types...); ref != "" {
			dangling[unit] = ref
			order = append(order, unit)
		}
	}

	iface.Members = nil
	for _, s := range members {
		if _, ok := dangling[s.unit]; !ok {
			iface.Members = append(iface.Members, s.member)
		}
	}

	drops := make([]KeepDrop, 0, len(order))
	for _, unit := range order {
		drops = append(drops, KeepDrop{What: iface.Name + "." + unit, Ref: dangling[unit]})
	}
	return drops
}

// keepUnit returns the name under which a member is kept or dropped — an
// attribute's, every operation of one identifier (an anonymous special operation
// by its keyword), every constructor — whether a keep list must name it for it
// to be kept, and the types it references.
func keepUnit(m webidl.Member) (unit string, listed bool, types []*webidl.TypeRef) {
	switch m := m.(type) {
	case *webidl.Attribute:
		return m.Name, true, []*webidl.TypeRef{m.Type}
	case *webidl.Operation:
		types = []*webidl.TypeRef{m.Return}
		for _, p := range m.Params {
			types = append(types, p.Type)
		}
		if m.Name == "" {
			return m.Special, false, types
		}
		return m.Name, true, types
	case *webidl.Constructor:
		for _, p := range m.Params {
			types = append(types, p.Type)
		}
		return keepConstructor, true, types
	case *webidl.Const:
		return m.Name, false, []*webidl.TypeRef{m.Type}
	case *webidl.Iterable:
		return "iterable", false, []*webidl.TypeRef{m.KeyType, m.ValueType}
	default:
		// A member kind added to the parser must decide here whether a keep list
		// names it; letting it through unexamined would bypass the filter.
		panic(fmt.Sprintf("bindgen: keep list: unhandled WebIDL member %T", m))
	}
}

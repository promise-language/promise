package bindgen

import (
	"errors"
	"fmt"
	"maps"
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
// it: an ancestor stays, with its listed members and the member names its kept
// descendants keep, so the `is` clause resolves. A member of a kept interface is
// kept when `Interface.member` is listed (`Interface.constructor` for its
// constructors); constants, iterable declarations and anonymous special
// operations go with their interface. Dictionaries, enums, typedefs and
// callbacks are not listed.
//
// The closure, to a fixed point: a typedef whose type names a dropped definition
// is dropped, and so is a dictionary whose parent is dropped or whose required
// member names one. Then an optional dictionary member naming one is removed,
// and so is an interface member — together with every member sharing its name,
// on its interface and on every kept interface inheriting from it.
//
// A member name is the unit, and the unit spans the `is` chain, so the ordinals
// WebIdlToIR assigns never depend on the list: a derived member is numbered
// after the instance members of its name it inherits (docs/wasm-bindings.md,
// "Shadowed Members"), so it keeps its ordinal only if the list keeps exactly
// those. Constructors are never inherited, so their unit stops at their
// interface.
//
// A reference is a name the lowered type carries (convertWebIdlTypeRef), so a
// name inside a union, Promise<T> or record<K, V> — each lowered to JsValue — is
// not one. A name absent from the input is not a dropped definition: a
// declaration naming it is left to fail to compile, as an unbound parent does.
func ApplyKeepList(file *webidl.File, keep *KeepList) []KeepDrop {
	ifaces := interfacesByName(file)

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

	// Every kept interface's selection first, so a name dangling on an ancestor
	// is known before the interfaces inheriting it are reduced.
	inherited := descendantKeeps(file, ifaces, kept, keep)
	selections := make(map[*webidl.Interface]*memberSelection)
	for _, iface := range file.Interfaces {
		if isMixin(iface) || !kept[iface.Name] {
			continue
		}
		selections[iface] = selectMembers(iface, func(unit string) bool {
			return keep.members[iface.Name+"."+unit] || inherited[iface][unit]
		}, droppedRef)
	}

	var interfaces []*webidl.Interface
	for _, iface := range file.Interfaces {
		if isMixin(iface) {
			interfaces = append(interfaces, iface)
			continue
		}
		if !kept[iface.Name] {
			continue
		}
		drops = append(drops, keepMembers(iface, selections, ifaces)...)
		interfaces = append(interfaces, iface)
	}
	file.Interfaces = interfaces

	return drops
}

// descendantKeeps returns, for each interface, the member names a kept
// interface inheriting from it keeps by a listed entry for a member it declares.
// The unit spans the `is` chain, so an ancestor keeps those names too. An entry
// naming a member its interface does not declare selects nothing, here as
// everywhere; constructors are never inherited, so their entries stay put.
func descendantKeeps(file *webidl.File, ifaces map[string]*webidl.Interface, kept map[string]bool, keep *KeepList) map[*webidl.Interface]map[string]bool {
	units := make(map[*webidl.Interface]map[string]bool)
	for _, d := range file.Interfaces {
		if isMixin(d) || !kept[d.Name] {
			continue
		}
		for _, m := range d.Members {
			unit, listed, _ := keepUnit(m)
			if !listed || unit == keepConstructor || !keep.members[d.Name+"."+unit] {
				continue
			}
			for _, a := range ancestors(d, ifaces) {
				if units[a] == nil {
					units[a] = make(map[string]bool)
				}
				units[a][unit] = true
			}
		}
	}
	return units
}

// ancestors returns the interfaces iface inherits from, nearest first, ending at
// an unbound parent or where an inheritance cycle comes back around.
func ancestors(iface *webidl.Interface, ifaces map[string]*webidl.Interface) []*webidl.Interface {
	var chain []*webidl.Interface
	seen := map[*webidl.Interface]bool{iface: true}
	for a := ifaces[iface.Parent]; a != nil && !seen[a]; a = ifaces[a.Parent] {
		seen[a] = true
		chain = append(chain, a)
	}
	return chain
}

// memberSelection is a kept interface's selected members, each with its unit,
// and the units among them that name a dropped definition.
type memberSelection struct {
	members []selectedMember
	// dangling maps a unit to the dropped definition one of its members names;
	// order lists those units as their first such member appears.
	dangling map[string]string
	order    []string
}

type selectedMember struct {
	member webidl.Member
	unit   string
}

// selectMembers selects a kept interface's members — those of a unit selected
// reports, and the unlisted kinds that go with their interface — and records
// each unit whose members name a dropped definition.
func selectMembers(iface *webidl.Interface, selected func(unit string) bool, droppedRef func(...*webidl.TypeRef) string) *memberSelection {
	s := &memberSelection{dangling: make(map[string]string)}
	for _, m := range iface.Members {
		unit, listed, types := keepUnit(m)
		if listed && !selected(unit) {
			continue
		}
		s.members = append(s.members, selectedMember{member: m, unit: unit})
		if _, seen := s.dangling[unit]; seen {
			continue
		}
		if ref := droppedRef(types...); ref != "" {
			s.dangling[unit] = ref
			s.order = append(s.order, unit)
		}
	}
	return s
}

// keepMembers reduces a kept interface to its selected members, then drops each
// member name whose members name a dropped definition — on the interface, or on
// an ancestor it inherits the name from, whose reference (the nearest such
// ancestor's) it reports. One KeepDrop per name: the interface's own dangling
// names first, then those it inherits, each in member order.
func keepMembers(iface *webidl.Interface, selections map[*webidl.Interface]*memberSelection, ifaces map[string]*webidl.Interface) []KeepDrop {
	own := selections[iface]
	dangling := maps.Clone(own.dangling)
	for _, a := range ancestors(iface, ifaces) {
		ancestor := selections[a]
		if ancestor == nil {
			continue
		}
		for unit, ref := range ancestor.dangling {
			if _, ok := dangling[unit]; !ok && unit != keepConstructor {
				dangling[unit] = ref
			}
		}
	}

	drops := make([]KeepDrop, 0, len(dangling))
	reported := make(map[string]bool)
	report := func(unit string) {
		if !reported[unit] {
			reported[unit] = true
			drops = append(drops, KeepDrop{What: iface.Name + "." + unit, Ref: dangling[unit]})
		}
	}
	for _, unit := range own.order {
		report(unit)
	}
	iface.Members = nil
	for _, s := range own.members {
		if _, drop := dangling[s.unit]; drop {
			report(s.unit)
			continue
		}
		iface.Members = append(iface.Members, s.member)
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

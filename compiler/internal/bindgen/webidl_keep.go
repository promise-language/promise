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
	// Unbound reports that Ref is defined in none of the bound IDL files, rather
	// than dropped by the list.
	Unbound bool
}

// ApplyKeepList reduces a merged file (webidl.Merge has run, so a mixin's
// members sit on each interface that includes it) to what keep selects, and
// returns what it then had to drop so that nothing kept names a dropped
// definition.
//
// An interface is kept when it is listed, or when a kept interface inherits from
// it: an ancestor stays, with its listed members and the units its kept
// descendants keep, so the `is` clause resolves. A member of a kept interface is
// kept when `Interface.member` is listed (`Interface.constructor` for its
// constructors); constants, iterable declarations and anonymous special
// operations go with their interface. Either keeps the member's whole unit.
// Dictionaries, enums, typedefs and callbacks are not listed.
//
// The closure, to a fixed point: a typedef whose type names a dropped definition
// is dropped, and so is a dictionary whose parent is dropped or whose required
// member names one. Then an optional dictionary member naming one is removed,
// and so is an interface member — together with every member of its unit, on
// its interface and on every kept interface inheriting from it.
//
// A member's unit is its Promise name (keepUnit), which is what WebIdlToIR
// counts its overload set by, and the unit spans the `is` chain, so the
// ordinals WebIdlToIR assigns never depend on the list: a derived member is
// numbered after the instance members of its Promise name it inherits
// (docs/wasm-bindings.md, "Shadowed Members"), so it keeps its ordinal only if
// the list keeps exactly those. Only instance members are inherited — statics,
// constants and constructors are not — so a unit reaches an ancestor — kept up
// the chain, or dropped down it — only where the ancestor holds an instance
// member of it.
//
// A reference is what firstRef walks: a name the type carries lowered with no
// typedef inlined, so a name inside a union, Promise<T> or record<K, V> — each
// lowered to JsValue — is not one, and a use of a typedef names the typedef,
// which the closure drops when its target names a dropped definition. A name
// defined outside IDL references what it names: WindowProxy is Window, and a
// [LegacyWindowAlias] identifier its interface. A name no bound IDL file defines
// (definedTypes), as a reference or as a dictionary's parent, counts as a
// dropped definition, and its KeepDrop is Unbound.
func ApplyKeepList(file *webidl.File, keep *KeepList) []KeepDrop {
	ifaces := interfacesByName(file)
	defined := definedTypes(file)

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
		return firstRef(file.WindowAliases, func(name string) bool { return dropped[name] || !defined[name] }, types...)
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
			if d.Parent != "" && (dropped[d.Parent] || !defined[d.Parent]) {
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

	// Every kept interface's selection first, so a unit dangling on an ancestor
	// is known before the interfaces inheriting it are reduced.
	own := make(map[*webidl.Interface]map[memberUnit]bool)
	inheritable := make(map[*webidl.Interface]map[memberUnit]bool)
	for _, iface := range file.Interfaces {
		if !isMixin(iface) && kept[iface.Name] {
			own[iface], inheritable[iface] = unitsOf(iface, keep)
		}
	}
	inherited := descendantKeeps(own, ifaces)
	selections := make(map[*webidl.Interface]*memberSelection)
	for _, iface := range file.Interfaces {
		if isMixin(iface) || !kept[iface.Name] {
			continue
		}
		selections[iface] = selectMembers(iface, func(unit memberUnit) bool {
			return own[iface][unit] || inherited[iface][unit] && inheritable[iface][unit]
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
		drops = append(drops, keepMembers(iface, selections, inheritable, ifaces)...)
		interfaces = append(interfaces, iface)
	}
	file.Interfaces = interfaces

	for i := range drops {
		drops[i].Unbound = !defined[drops[i].Ref]
	}
	return drops
}

// unitsOf returns the units of a kept interface's members that it keeps by
// itself — each holding a member the list names, or one that goes with its
// interface — and the units a descendant's members are numbered against: each
// holding an instance attribute or operation, the only members a descendant
// inherits (memberNamer; statics, constants and constructors are `global`). An
// entry naming a member the interface does not declare selects nothing, here
// as everywhere.
func unitsOf(iface *webidl.Interface, keep *KeepList) (own, inheritable map[memberUnit]bool) {
	own = make(map[memberUnit]bool)
	inheritable = make(map[memberUnit]bool)
	for _, m := range iface.Members {
		unit, label, listed, _, _ := keepUnit(m)
		if !listed || keep.members[iface.Name+"."+label] {
			own[unit] = true
		}
		switch m := m.(type) {
		case *webidl.Attribute:
			inheritable[unit] = inheritable[unit] || !m.Static
		case *webidl.Operation:
			inheritable[unit] = inheritable[unit] || !m.Static && unit.lowered
		}
	}
	return own, inheritable
}

// descendantKeeps returns, for each interface, the units a kept interface
// inheriting from it keeps by itself (own). The unit spans the `is` chain, so an
// ancestor keeps those units too — each one it holds an instance member of
// (unitsOf), which ApplyKeepList checks.
func descendantKeeps(own map[*webidl.Interface]map[memberUnit]bool, ifaces map[string]*webidl.Interface) map[*webidl.Interface]map[memberUnit]bool {
	units := make(map[*webidl.Interface]map[memberUnit]bool)
	for d, keeps := range own {
		for _, a := range webidl.Ancestors(d, ifaces) {
			if units[a] == nil {
				units[a] = make(map[memberUnit]bool)
			}
			for unit := range keeps {
				units[a][unit] = true
			}
		}
	}
	return units
}

// memberSelection is a kept interface's selected members, each with its unit
// and label, and the units among them that name a dropped definition.
type memberSelection struct {
	members []selectedMember
	// dangling maps a unit to the dropped definition one of its members names;
	// order holds, for each label, the first of its members that names one, in
	// member order.
	dangling map[memberUnit]string
	order    []selectedMember
}

type selectedMember struct {
	member webidl.Member
	unit   memberUnit
	label  string
	ref    string // the dropped definition the member names, or ""
}

// selectMembers selects a kept interface's members — those of a unit selected
// reports — and records each unit whose members name a dropped definition.
func selectMembers(iface *webidl.Interface, selected func(unit memberUnit) bool, droppedRef func(...*webidl.TypeRef) string) *memberSelection {
	s := &memberSelection{dangling: make(map[memberUnit]string)}
	ordered := make(map[string]bool)
	for _, m := range iface.Members {
		unit, label, _, types, _ := keepUnit(m)
		if !selected(unit) {
			continue
		}
		member := selectedMember{member: m, unit: unit, label: label, ref: droppedRef(types...)}
		s.members = append(s.members, member)
		if member.ref == "" || ordered[label] {
			continue
		}
		ordered[label] = true
		s.order = append(s.order, member)
		if _, seen := s.dangling[unit]; !seen {
			s.dangling[unit] = member.ref
		}
	}
	return s
}

// keepMembers reduces a kept interface to its selected members, then drops each
// unit whose members name a dropped definition — on the interface, or on an
// ancestor it inherits the unit from; an ancestor's unit holding no instance
// member is not inherited. One KeepDrop per dropped label: first those whose
// own members name a dropped definition, each reporting the one it names, then
// the rest, each reporting the one its unit was dropped for (the interface's
// own, or else the nearest such ancestor's), each in member order.
func keepMembers(iface *webidl.Interface, selections map[*webidl.Interface]*memberSelection, inheritable map[*webidl.Interface]map[memberUnit]bool, ifaces map[string]*webidl.Interface) []KeepDrop {
	own := selections[iface]
	dangling := maps.Clone(own.dangling)
	for _, a := range webidl.Ancestors(iface, ifaces) {
		ancestor := selections[a]
		if ancestor == nil {
			continue
		}
		for unit, ref := range ancestor.dangling {
			if _, ok := dangling[unit]; !ok && inheritable[a][unit] {
				dangling[unit] = ref
			}
		}
	}

	drops := make([]KeepDrop, 0, len(dangling))
	reported := make(map[string]bool)
	report := func(s selectedMember, ref string) {
		if !reported[s.label] {
			reported[s.label] = true
			drops = append(drops, KeepDrop{What: iface.Name + "." + s.label, Ref: ref})
		}
	}
	for _, s := range own.order {
		report(s, s.ref)
	}
	iface.Members = nil
	for _, s := range own.members {
		if ref, drop := dangling[s.unit]; drop {
			report(s, ref)
			continue
		}
		iface.Members = append(iface.Members, s.member)
	}
	return drops
}

// memberUnit is what a member is kept or dropped with: every member of its
// interface that lowers to its Promise name (memberName), the name its ordinal
// is counted by. A member that lowers to none is keyed by its keyword, with
// lowered false, so it never shares a unit with a member whose Promise name
// that keyword happens to spell.
type memberUnit struct {
	name    string
	lowered bool
}

// keepUnit returns a member's unit; its label — the name a keep list names it
// by, if listed reports that a list must name it for it to be kept, and the
// name a KeepDrop reports it by: an attribute's or operation's identifier, an
// anonymous special operation's keyword, `constructor` for every constructor;
// the types it references; and its position, which unboundRefs reports it at.
func keepUnit(m webidl.Member) (unit memberUnit, label string, listed bool, types []*webidl.TypeRef, pos webidl.Pos) {
	switch m := m.(type) {
	case *webidl.Attribute:
		label, listed, types, pos = m.Name, true, []*webidl.TypeRef{m.Type}, m.Pos
	case *webidl.Operation:
		types = []*webidl.TypeRef{m.Return}
		for _, p := range m.Params {
			types = append(types, p.Type)
		}
		label, listed, pos = m.Name, true, m.Pos
		if m.Name == "" {
			label, listed = m.Special, false
		}
	case *webidl.Constructor:
		for _, p := range m.Params {
			types = append(types, p.Type)
		}
		label, listed, pos = keepConstructor, true, m.Pos
	case *webidl.Const:
		label, listed, types, pos = m.Name, false, []*webidl.TypeRef{m.Type}, m.Pos
	case *webidl.Iterable:
		label, listed, types, pos = "iterable", false, []*webidl.TypeRef{m.KeyType, m.ValueType}, m.Pos
	default:
		// A member kind added to the parser must decide here whether a keep list
		// names it; letting it through unexamined would bypass the filter.
		panic(fmt.Sprintf("bindgen: keep list: unhandled WebIDL member %T", m))
	}
	if name, ok := memberName(m); ok {
		return memberUnit{name: name, lowered: true}, label, listed, types, pos
	}
	return memberUnit{name: label}, label, listed, types, pos
}
